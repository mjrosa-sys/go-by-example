# Web development in Go — the file server (`net/http`)

This folder answers three questions, in order:

1. **How do I open a file server in Go?**
2. **What are the key characteristics of Go's file server?** (what you get for free, and what you don't)
3. **What are the use cases?** (when to reach for it, and when not to)

Everything here was **written and verified against Go 1.26.5** — the exact
toolchain on this machine. Every output block you see below is real: it was
produced by running these very programs with `curl` against them, not copied
from a tutorial. The standard library is installed as source, so you can (and
should) read it: on this machine it lives under `/usr/local/go/src/net/http/`.

> **How to study this folder:** run each example, read its code top to bottom
> (the comments ARE the lesson), then run the lab experiments in section 6 and
> *break things on purpose*. You learn a server by observing what it does when
> you poke it.

## What is in this folder

| Path | What it teaches | Port |
|---|---|---|
| [main.go](main.go) | the guided tour: `http.FileServer`, `http.Dir`, `http.StripPrefix`, `http.ServeFile`, patterns, middleware, `http.Server` | 8080 |
| [public/](public/) | the files `main.go` serves (sample content to poke at) | — |
| [embed-example/](embed-example/) | baking the assets INTO the binary with `//go:embed` | 8081 |
| [no-listing/](no-listing/) | disabling directory listings by wrapping an interface | 8082 |

## 0. The 60-second HTTP mental model

HTTP is a request/response protocol. Your browser (any HTTP client) sends
something conceptually like:

```
GET /docs/cheatsheet.txt HTTP/1.1     <- method + URL path + protocol version
Host: localhost:8080                   <- headers (key: value)
```

and the server answers with a status line, headers, and a body:

```
HTTP/1.1 200 OK                        <- status line (200 = success)
Content-Type: text/plain; charset=utf-8 <- headers describing the body
Content-Length: 826

This is notes.txt ...                   <- the body (the actual bytes)
```

A **file server** is the simplest useful web program: *URL path → file on
disk → bytes to the response body*. Everything in this folder is Go's standard
library doing that mapping with production-grade care (MIME types, security,
caching, partial downloads).

## 1. How to open a file server

The smallest honest version is **four statements** (this compiles — we built
it before putting it here):

```go
package main

import (
	"log"
	"net/http"
)

func main() {
	http.Handle("/", http.FileServer(http.Dir("./public")))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Line by line:

- `http.Handle("/", ...)` — registers a handler for the pattern `/` ("every
  path") on the **package-global router**, `http.DefaultServeMux`.
- `http.Dir("./public")` — the directory `./public`, as a *file system* the
  server can read from. It is a security boundary: request paths are resolved
  **inside** it (details in section 3.1).
- `http.FileServer(...)` — builds the handler that maps requests to files.
- `http.ListenAndServe(":8080", nil)` — starts a TCP server on port 8080,
  with `nil` meaning "use `http.DefaultServeMux`". It **blocks forever**;
  when it returns, something failed, so `log.Fatal` prints the error and exits.

[main.go](main.go) is the same idea, but structured the way real programs are:
a private mux (`http.NewServeMux()`), named handler functions, a logging
middleware, and an explicit `http.Server{...}` value (which is also where you
would configure timeouts and graceful shutdown later). The comments inside
explain every line.

## 2. How to run the examples

From this directory (`webdev/fileserver`):

```bash
go run .                    # main example      -> http://localhost:8080
go run ./embed-example      # embedded variant  -> http://localhost:8081
go run ./no-listing         # no listings       -> http://localhost:8082
```

> Paths like `http.Dir("./public")` are relative to the **current working
> directory** — the folder you start the program from. Run `go run .` from the
> repo root and every request 404s. This is the #1 beginner bug with
> `http.Dir`.

While `main.go` runs, the middleware prints one line per request — this is
what you should see when you open `http://localhost:8080` in a browser
(three requests: the page, its stylesheet, its image):

```
2026/10/13 20:32:53 file server listening on http://localhost:8080  (Ctrl+C stops it)
2026/10/13 20:33:51 GET /                            -> 200 OK (in 1.455ms)
2026/10/13 20:33:51 GET /styles.css                 -> 200 OK (in 56µs)
2026/10/13 20:33:51 GET /pixel.png                  -> 200 OK (in 53µs)
```

Watch that log while you do section 6's experiments: **the status code column
is the file server's behavior made visible.**

## 3. Key characteristics of Go's file server

All of these live in one stdlib file: `net/http/fs.go`. Below, each
characteristic comes with the **observed** behavior on Go 1.26.5.

### 3.1 URL → file mapping, and the security boundary

The core contract: the URL path becomes a path **inside the directory you
gave `http.Dir`**. Requesting `/docs/cheatsheet.txt` opens
`./public/docs/cheatsheet.txt`.

How the clamp happens (Go 1.26 `Dir.Open`, `fs.go:77`):

```
requested:  /../../etc/passwd
step 1:     path.Clean("/" + "/../../etc/passwd")   // Clean clamps ".." at the root
            => "/etc/passwd"                        // the escape attempt evaporated
step 2:     filepath.Localize(...)                  // rejects OS-unsafe spellings
step 3:     filepath.Join("./public", "etc/passwd")
            => "./public/etc/passwd"                // opened, doesn't exist, 404
```

And there is a **second** layer: the `ServeMux` never lets a request containing
`.` or `..` path *segments* reach any handler at all — it replies with a
redirect to the cleaned path first (`server.go`, `matchOrRedirect`).

Observed (raw requests, using `curl --path-as-is` so curl doesn't clean them
itself):

```
$ curl --path-as-is -o /dev/null -w "%{http_code} -> %{redirect_url}\n" \
    "http://localhost:8080/../../etc/passwd"
307 -> http://localhost:8080/etc/passwd     # mux sanitizes: redirects to cleaned path

$ curl http://localhost:8080/etc/passwd
404 page not found                          # FileServer: ./public/etc/passwd missing
```

**Verdict: path traversal is dead by construction.** What still gets through
(see 3.11): symlinks pointing outside the tree, and dot-files like `.git`.

### 3.2 MIME types come from the extension (with a sniffing fallback)

The `Content-Type` header — the part that tells the browser "this is CSS, not
HTML" — comes from `mime.TypeByExtension` (`fs.go:288`). Only if the
extension is unknown does the server read the first 512 bytes and *sniff*
(`http.DetectContentType`, the same function browsers use).

Observed on this machine (same disk, three extensions):

```
$ curl -sI http://localhost:8080/styles.css | grep -i content-type
Content-Type: text/css; charset=utf-8

$ curl -sI http://localhost:8080/data.json | grep -i content-type
Content-Type: application/json

$ curl -sI http://localhost:8080/pixel.png | grep -i content-type
Content-Type: image/png
```

Fun fact: `mime.TypeByExtension` consults a built-in table **plus** the
system's MIME databases (`/etc/mime.types` and friends), which is why your
`.css` works everywhere but your custom `.weird` extension falls through to
sniffing.

### 3.3 The index.html convention

A directory URL serves that directory's `index.html` if it exists (and only
`index.html` — Go 1.26 looks for exactly that name, not `index.htm`). That is
why `GET /` serves `public/index.html`.

URLs ending in `/index.html` are redirected to the directory's clean URL, so
there's only one canonical address per directory:

```
$ curl -sI http://localhost:8080/index.html
HTTP/1.1 301 Moved Permanently
Location: ./
```

### 3.4 Directory listings (when there is no index.html)

A directory without an `index.html` gets an auto-generated HTML listing — a
feature that is convenient in development and a **data leak in production**
(hand a stranger a map of every file). It looks exactly like this on 1.26.5:

```
$ curl -s http://localhost:8080/docs/
<!doctype html>
<meta name="viewport" content="width=device-width">
<pre>
<a href="cheatsheet.txt">cheatsheet.txt</a>
<a href="deep/">deep/</a>
</pre>
```

Notes: names are sorted, directories get a trailing `/`, both the URL and the
display text are escaped (a file named `<script>` cannot inject HTML). To
turn listings **off**, see [no-listing/](no-listing/) (section 5.2).

### 3.5 Redirects — and there are TWO different redirect makers

You will meet two different redirects, from two different layers:

| Request | Status | Produced by | Why |
|---|---|---|---|
| `/docs` | **301** Moved Permanently | the FileServer (`fs.go:709`) | canonical form: directories end with `/` so relative links resolve |
| `/index.html` | **301** | the FileServer (`fs.go:685`) | canonical form: one address per directory |
| `/static` | **307** Temporary Redirect | the ServeMux | subtree patterns registered with a trailing slash redirect their bare form |
| `/docs/../main.go` | **307** | the ServeMux | path sanitizing before any handler runs |

Observed, both flavors:

```
$ curl -sI http://localhost:8080/docs | head -2
HTTP/1.1 301 Moved Permanently
Location: docs/            # note: RELATIVE, so it works under StripPrefix mounts too

$ curl -sI http://localhost:8080/static | head -3
HTTP/1.1 307 Temporary Redirect
Location: /static/
```

That relative `Location: docs/` is deliberate — the stdlib comments say plain
`http.Redirect` "would make the path absolute, which would be a problem
running under StripPrefix". Tiny detail, real consequence.

### 3.6 Conditional requests: free caching with `304 Not Modified`

Every file response carries `Last-Modified` (the file's modification time).
A client that saw the file before can send it back in `If-Modified-Since` —
if nothing changed, the server answers **304** with no body. The file bytes
never travel; that is the whole HTTP caching model in two headers.

Observed:

```
$ curl -sI http://localhost:8080/notes.txt | grep -i last-modified
Last-Modified: Tue, 13 Oct 2026 23:21:05 GMT

$ curl -sI -H "If-Modified-Since: Tue, 13 Oct 2026 23:21:05 GMT" \
    http://localhost:8080/notes.txt
HTTP/1.1 304 Not Modified
Last-Modified: Tue, 13 Oct 2026 23:21:05 GMT
Date: Tue, 13 Oct 2026 23:33:51 GMT
```

Note what the 304 does NOT have: no `Content-Type`, no `Content-Length` — the
stdlib deletes body-describing headers (`writeNotModified`, `fs.go:627`),
because a 304 has no body by definition. There is **no ETag** by default; if
you set an `ETag` header yourself, `ServeContent` will honor
`If-None-Match`/`If-Match` for you (RFC 7232 logic is already implemented).

### 3.7 Range requests: `206 Partial Content`

Every response advertises `Accept-Ranges: bytes`, and a `Range` header gets
honored. This is the machinery behind "resume a broken download" (curl
`-C -`), video players scrubbing without downloading the whole file, and
browser parallel fetching:

```
$ curl -s -D - -H "Range: bytes=0-9" http://localhost:8080/notes.txt
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Content-Length: 10                       # length of the SLICE, not the file
Content-Range: bytes 0-9/826            # what you got / total size
Content-Type: text/plain; charset=utf-8

This is no                              # bytes 0-9 of the file, exactly
```

Multiple ranges (`Range: bytes=0-9,20-29`) produce a
`multipart/byteranges` response — the stdlib builds the MIME container for
you (`fs.go:366`). An impossible range is answered `416` with a
`Content-Range: bytes */826` hint (`fs.go:331`).

### 3.8 HTTP methods: the file server has NO opinion

`FileServer`/`ServeFile` do not check the method at all. Any method — POST,
PUT, DELETE — is served like a GET (only HEAD is special: body suppressed).
Observed:

```
$ curl -X POST -w "\n[status %{http_code}]\n" http://localhost:8080/notes.txt
This is notes.txt -- the exact bytes of this file are what the server sends.
...
[status 200]
```

Method discipline comes from the **router**: a pattern like
`GET /hello/{name}` matches GET and HEAD only. But beware the interplay —
because our `/` catch-all names no method, a POST that misses
`GET /hello/{name}` falls through to the FileServer:

```
POST http://localhost:8080/hello/rosen  -> 404 (the catch-all FileServer looked
                                           for ./public/hello/rosen and missed)
```

Isolate the route (no catch-all registered) and the mux's real "wrong method"
answer appears:

```
$ curl -s -X POST -D - http://localhost:8090/hello/rosen
HTTP/1.1 405 Method Not Allowed
Allow: GET, HEAD                        # the mux tells the client what works

Method Not Allowed
```

### 3.9 Streaming and memory behavior

The file server never slurps a file into memory: it opens, `Stat`s for the
size (that's your `Content-Length`), then streams with `io.CopyN` one buffer
at a time. A 10 GB file costs the same few KB of RAM as a 10 KB one. On
Linux, `io.Copy` even notices the writer is a socket and the reader is a real
file and uses the kernel's zero-copy `sendfile` path.

`HEAD` is fully supported — the server runs the same logic but skips the body
write (note it still sends `Content-Length`: "this is how big it would have
been").

### 3.10 What the file server does NOT do for you

Go gives you a correct, efficient core — the *policy* is your job:

| Missing | What to add | How much work |
|---|---|---|
| `Cache-Control` header (freshness hints) | `w.Header().Set("Cache-Control", "public, max-age=3600")` in a middleware | ~5 lines |
| `ETag` (strong revalidation) | compute a hash of the file, set `ETag`, let `ServeContent` do the rest | ~10 lines |
| Authorization (who may fetch?) | middleware that checks a session/token before `next.ServeHTTP` | ~15 lines |
| Compression (gzip/brotli) | pre-compress assets, or wrap the ResponseWriter (careful: breaks `Content-Length` — see `fs.go:400`'s long comment) | ~30 lines |
| Rate limiting | `golang.org/x/time/rate` in a middleware | ~10 lines |
| Custom 404 page | wrap the handler; if no one else wrote to the response, write your own page | ~20 lines |

The pattern for all of them is the same **middleware** shape as
`logRequests` in [main.go](main.go): *a function taking an `http.Handler`
and returning a wrapped one.* Learn that shape once, use it everywhere.

### 3.11 Remaining sharp edges (things that DO get through)

These are documented on `http.Dir` itself and worth tattooing somewhere:

- **Symlinks**: `Dir` follows them — a link inside your public tree that
  points to `/etc` will happily serve `/etc`. Never serve a directory users
  can write to.
- **Dot-files**: `.git/`, `.env`, `.htpasswd` are served like any other
  file. Serve a dedicated, curated directory; or wrap the `FileSystem` (the
  [no-listing](no-listing/) example shows exactly how to wrap one).
- **The CWD dependency**: `http.Dir("./public")` resolves from *where the
  process was started*, which is not where the *binary* lives. Absolute
  paths, an env var, or `//go:embed` remove the ambiguity.
- **Errors**: a failed `Open` is mapped by matching *sentinel errors*
  (`fs.go:769`): `fs.ErrNotExist` → 404, `fs.ErrPermission` → 403, anything
  else → **500**. If you write your own `FileSystem`/handlers, return the
  right sentinel — random errors become 500s.

## 4. Use cases

**Reach for `http.FileServer` when...**

- **Serving a static site**: docs, blogs, landing pages. Build the HTML
  (or hand-write it), point a `Dir` at it, done. One binary, no nginx for
  small/medium traffic.
- **The `/static/` assets of a bigger Go web app**: mount CSS/JS/images under
  a prefix with `StripPrefix`, keep your dynamic routes next to it in the
  same mux.
- **Download/file-distribution endpoints**: `http.ServeFile` behind a route
  (plus your auth middleware) is a legitimate download server; Range support
  and resume come for free.
- **Local development and tooling**: serving report folders, generated
  files, fixtures. The directory listing alone makes it a 30-second dev tool.
- **Single-binary deploys** ([embed-example](embed-example/)): ship one
  executable, `FROM scratch` Docker images, no "did you copy the assets
  folder?" incidents. Change an asset ⇒ rebuild.

**Think twice when...**

- Users **upload** files: you need scanning, quotas, per-user permissions —
  the file server is only the read half of such a system.
- You need auth per file: file serving has no opinions; you must wrap it in
  your own authorization middleware.
- You serve at CDN scale: put nginx/CloudFront in front; the Go server is
  your origin. It will happily saturate a 1 Gbps link, but a CDN is cheaper
  than bandwidth.
- Your "static" content is actually per-user dynamic: use `http.ServeContent`
  with an `io.ReadSeeker` instead of files.

## 5. The two sibling examples

### 5.1 [embed-example/](embed-example/) — files inside the binary

`//go:embed assets` compiles the whole `assets/` tree into the executable.
At runtime it is a read-only `embed.FS` (implements the modern `io/fs`
interfaces), served by `http.FileServerFS` — the `fs.FS`-flavored twin of
`http.FileServer`. `fs.Sub(assets, "assets")` re-roots the FS so URLs don't
carry the `assets/` prefix. Same listing behavior, same MIME logic, same
everything — different bytes source.

One subtle observed difference: **embedded files have no modification time**
(`embed.FS` reports a zero `ModTime`), so responses have **no `Last-Modified`
header** and 304 revalidation never triggers. It doesn't matter: the content
of an embedded file is as immutable as the binary itself.

```
$ curl -sI http://localhost:8081/styles.css
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 343
Content-Type: text/css; charset=utf-8
                    <- no Last-Modified; there is no mtime to report
```

The classic proof it works:

```bash
cd embed-example
go build -o /tmp/app .   # build ANYWHERE
cd / && /tmp/app         # run with no ./assets in sight — still serves everything
```

### 5.2 [no-listing/](no-listing/) — disabling directory listings

The recipe: wrap the `http.FileSystem` **interface** (the one method the
FileServer consumes), and when `Open` lands on a directory without an
`index.html`, return `fs.ErrNotExist` — the sentinel the FileServer already
translates into a clean `404 page not found`. Observed matrix:

| URL | Result | Why |
|---|---|---|
| `/` | 200 | root has `index.html` — untouched |
| `/about/` | 200 | directory WITH `index.html` — untouched |
| `/docs/` | **404** | directory without `index.html` — the wrapper lies: "does not exist" |
| `/docs/secret.txt` | **200** | the file itself is still served! |

That last row is the security moral: **disabling listings is obscurity, not
access control.** If a name leaks (old links, logs, `git log`), the file is
fetchable. Real protection is authorization middleware deciding *who* may
call — never *whether they guessed the URL*.

## 6. The lab: run these while the servers are up

`main.go` on :8080, unless noted. `-s` silent, `-I` headers only, `-D -` dump
headers, `-w` print variables. Use `--path-as-is` whenever you test weird
paths or curl will silently clean them for you and you'll test the wrong
thing.

```bash
# 1. The basics — files at URL paths
curl -s http://localhost:8080/                     # index.html for "/"
curl -s http://localhost:8080/docs/deep/nested.txt
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/missing.txt   # 404

# 2. MIME by extension
curl -sI http://localhost:8080/styles.css | grep -i content-type   # text/css
curl -sI http://localhost:8080/data.json | grep -i content-type    # application/json
curl -sI http://localhost:8080/pixel.png | grep -i content-type    # image/png

# 3. Redirects
curl -sI http://localhost:8080/docs         # 301 -> docs/   (FileServer)
curl -sI http://localhost:8080/index.html  # 301 -> ./      (FileServer)
curl -sI http://localhost:8080/static       # 307 -> /static/ (ServeMux!)

# 4. Caching dance (copy the date from the first command into the second)
curl -sI http://localhost:8080/notes.txt | grep -i last-modified
curl -sI -H "If-Modified-Since: <that date>" http://localhost:8080/notes.txt   # 304

# 5. Partial content
curl -s -H "Range: bytes=0-9" http://localhost:8080/notes.txt                 # 206
curl -s -H "Range: bytes=0-9,20-29" -D - http://localhost:8080/notes.txt      # multipart!

# 6. Methods (surprise inside)
curl -X POST -w "\n%{http_code}\n" http://localhost:8080/notes.txt            # 200 (!)
curl -X POST -w "\n%{http_code}\n" http://localhost:8080/hello/rosen         # 404 (why?)

# 7. Attack it (both fail safely)
curl --path-as-is -o /dev/null -w "%{http_code} -> %{redirect_url}\n" \
     "http://localhost:8080/../../etc/passwd"                                # 307, cleaned
curl -w "\n%{http_code}\n" "http://localhost:8080/files/docs%2Fcheatsheet.txt" # 400 (validation)

# 8. Prefix mounting
curl -s http://localhost:8080/static/notes.txt                                # via StripPrefix

# 9. The forced download
curl -sI http://localhost:8080/download | grep -i content-disposition

# 10. Siblings (:8081 embed, :8082 no-listing)
curl -s http://localhost:8081/docs/                     # listing works over embed.FS
curl -sI http://localhost:8081/styles.css               # ...but no Last-Modified
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8082/docs/          # 404
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8082/docs/secret.txt # 200 (!)
```

Expected log lines in `main.go`'s terminal while you do all that (real ones,
captured during verification):

```
GET /                            -> 200 OK (in 1.455ms)
HEAD /docs                       -> 301 Moved Permanently (in 52µs)
GET /docs/../main.go             -> 307 Temporary Redirect (in 26µs)
GET /files/docs/cheatsheet.txt   -> 400 Bad Request (in 18µs)     <- %2F was decoded!
POST /notes.txt                  -> 200 OK (in 79µs)
```

Notice experiment 7's log line: curl sent `docs%2Fcheatsheet.txt`, the
middleware logged `docs/cheatsheet.txt` — `PathValue` had already decoded the
`%2F` into a real slash, which is exactly why `serveByName` refuses slashes.

## 7. Read the source — it's on your machine

`go doc net/http.fs`-level knowledge is good; the source is better. On this
box (`GOROOT=/usr/local/go`, Go 1.26.5), the whole file server is essentially
**one file** — `/usr/local/go/src/net/http/fs.go` (~1100 lines, half are
comments):

| Where | What it is |
|---|---|
| `fs.go:77` | `Dir.Open` — the security clamp (`path.Clean`, `Localize`, `Join`) |
| `fs.go:139` | `dirList` — the auto-listing generator |
| `fs.go:245` | `ServeContent` — public API: MIME, caching, ranges |
| `fs.go:274` | `serveContent` — the real engine: preconditions, sniffing, 206s |
| `fs.go:627` | `writeNotModified` — what a 304 keeps and deletes |
| `fs.go:679` | `serveFile` — the FileServer's per-request flow (read this one first) |
| `fs.go:769` | `toHTTPError` — sentinel error → status code mapping |
| `fs.go:785` | `localRedirect` — the relative 301s |
| `fs.go:814` | `ServeFile` — single-file API + its `..` seatbelt |
| `fs.go:971` | `FileServer` — the 2-line handler you registered |
| `server.go:2334` | `StripPrefix` |
| `server.go:2696` | the mux's 405-with-Allow logic |

Reading order suggestion: `FileServer` → `serveFile` → `serveContent`. You
now recognize every line of that path.

## 8. Homework (each is a small, real exercise)

1. **Cache-Control middleware**: wrap `logRequests`'s output with a middleware
   setting `Cache-Control: public, max-age=300` — but only for `/static/`
   requests. Check with `curl -I`.
2. **True 405 for a file route**: register `"GET /only/{name}"` in `main.go`
   with no catch-all and confirm `Allow: GET, HEAD` on a POST. Then explain
   to a friend why the catch-all `"/"` changes the answer for
   `/hello/{name}` (section 3.8 has the receipts).
3. **SPA fallback**: a single-page app needs *unknown* paths to serve
   `index.html` (so client-side routing works). Write a wrapper handler: try
   the file; if the file server 404s, serve `./public/index.html`. (Hint:
   the statusRecorder tells you if it 404'd — but careful: by then the
   404 status is already written. Better: check if `os.Stat` finds the file
   first.)
4. **ETags**: in `serveByName`, `w.Header().Set("ETag", ...)` from a hash of
   the file's (size, mtime) before calling `ServeFile`. Send
   `If-None-Match` back with curl and watch the 304 — you didn't write any
   validation code; `ServeContent` did it for you.
5. **Test it without a network**: `httptest.NewServer(fileServer)` gives you
   a real server on an ephemeral port in-process. Write a test asserting
   `GET /notes.txt` returns 200 and `text/plain`. This is how professionals
   test handlers.
6. **Unwrappable middleware**: `logRequests`'s recorder hides optional
   `ResponseWriter` abilities like `Flush()` from the file server. Add an
   `Unwrap() http.ResponseWriter` method (see `responsecontroller.go:43`)
   and read about `http.ResponseController` — the stdlib's own answer to
   exactly this problem.

## 9. Glossary (one line each)

| Term | Meaning |
|---|---|
| Handler | anything with a `ServeHTTP(ResponseWriter, *Request)` method; the one interface all of `net/http` is built on |
| ServeMux | the stdlib's router; matches *patterns* (host/method/path with wildcards) to handlers |
| pattern | the routing key, e.g. `GET /hello/{name}`; `{name}` captures one segment, `{name...}` the rest, `{$}` anchors the end |
| middleware | a function `func(http.Handler) http.Handler` that wraps another handler to add behavior (logging, auth, CORS...) |
| `http.Dir` | a directory on disk seen as an `http.FileSystem` (the older, `net/http`-local FS interface) |
| `fs.FS` / `embed.FS` | the modern, generic file-system interface (`io/fs`), and its "files compiled into the binary" implementation |
| adapter type | a type that gives an existing kind of value a new interface — e.g. `http.HandlerFunc` wraps a plain function; `http.FS` wraps an `fs.FS` |
| sentinel error | a pre-declared error value meaning a condition — `fs.ErrNotExist`; handlers and libraries *match* sentinels, never parse strings |
| canonical redirect | 301 to the one true URL form (`/docs` → `/docs/`, `/index.html` → `./`) |
| conditional request | client sends what it knows (`If-Modified-Since`/`If-None-Match`); server answers 304 instead of resending bytes |
| Range request | client asks for a byte slice (`Range: bytes=0-9`); server answers 206 with exactly those bytes |
| CWD | current working directory — where you *started* the process; the base of relative paths like `./public` |

---

*Next folder idea: the same mux + middleware ideas with `http.ServeContent`
and `httptest`, or move up the stack to `html/template` for server-rendered
pages.*
