# Web development in Go — the router: `http.ServeMux`

This folder answers three questions, in order:

1. **How is Go's `ServeMux` used?** (the registration API and the pattern language)
2. **What are its key characteristics?** (matching, precedence, redirects, concurrency — all observed live)
3. **What are its key internal workings?** (pattern compilation, the routing tree, and where the captured values actually live)

Everything was **written and verified against Go 1.26.5** — the exact toolchain on
this machine. Every output block below is a real observation from running
these very programs with `curl`. The whole router is ~600 lines of readable
source in `/usr/local/go/src/net/http/` (`server.go`, `pattern.go`,
`routing_tree.go`, `routing_index.go`) — section 8 gives you the map.

> **How to study this folder:** run both examples, read [main.go](main.go)
> top to bottom (the comments ARE the lesson), then run the lab in section 7
> and watch the middleware log narrate every routing decision the mux makes.

## What is in this folder

| Path | What it teaches | Port |
|---|---|---|
| [main.go](main.go) | the tour: pattern grammar, method matching, wildcards, precedence, hosts, stateful handlers, middleware | 8090 |
| [mount/](mount/main.go) | sub-routers: a ServeMux is itself a Handler, so muxes nest via `mux.Handle` + `StripPrefix` | 8091 |

## 0. Where the mux sits — the life of one request

```
client
  │  GET /users/7/posts/3 HTTP/1.1
  ▼
http.Server                       owns TCP: accept, connections, timeouts.
  │                               one goroutine PER REQUEST (server.go, "Serve")
  ▼
*ServeMux  — the switchboard       matches the pattern "GET /users/{id}/posts/{postID}",
  │                               writes r.Pattern and the captured values,
  ▼                               then dispatches.
your handler                      writes status + headers + body via the
                                  ResponseWriter.
```

Division of labor, memorize this: **the Server moves bytes, the mux decides,
the handler answers.** This folder is about the middle box.

## 1. How `ServeMux` is used

### 1.1 Make a mux — private, not global

```go
mux := http.NewServeMux()          // yours: a value, testable, conflict-free
```

The alternative is the package-global `http.DefaultServeMux` that the
package-level `http.Handle`/`http.HandleFunc` (and `ListenAndServe(":80", nil)`)
use. Globals mean *any import anywhere* can silently register routes on your
router and any two patterns can surprise-conflict. Real code uses
`NewServeMux`; the global one survives for 5-line demos and backwards
compatibility.

### 1.2 Register: `Handle` and `HandleFunc`

```go
mux.HandleFunc("GET /health", healthHandler)     // func(ResponseWriter, *Request)
mux.Handle("GET /visits", &visitCounter{})        // any Handler value
```

Both take a pattern; they differ only in the second argument.
`HandleFunc` wraps your plain function in `http.HandlerFunc` — an *adapter
type* whose only job is to give a function a `ServeHTTP` method, turning it
into a `Handler`. **Registration panics** on invalid patterns and on conflicts
— at startup, before serving — and the panic names the file:line of every
registration involved (section 3.5 has a real one).

### 1.3 The pattern grammar

A pattern is `[METHOD ] [HOST]/[PATH]` — method, host, path, all optional:

| Pattern | Matches |
|---|---|
| `"/ping"` | every method, exactly `/ping` |
| `"GET /health"` | GET and **HEAD** at `/health` (a GET pattern matches HEAD) |
| `"POST /health"` | only POST at `/health` — coexists with the GET twin above |
| `"GET /users/{id}"` | `{x}`: exactly **one** path segment, captured as `id` |
| `"GET /files/{path...}"` | `{x...}`: the **rest of the path**, slashes included |
| `"GET /{$}"` | `{$}` is an end anchor: **only** `/` (a bare `"/"` is every path) |
| `"/static/"` | trailing slash = a **subtree**: everything under `/static/` |
| `"api.example.com/x"` | host-scoped: only that `Host` header, any method |

## 2. How to run the examples

From this directory (`webdev/servemux`):

```bash
go run .          # the tour  -> http://localhost:8090
go run ./mount    # sub-muxes -> http://localhost:8091
```

The middleware prints one line per request, including **the pattern the mux
matched** (read from `r.Pattern`, a plain `string` field on `Request` in
Go 1.26.5, assigned by the mux itself while dispatching). This is the mux
explaining itself in real time:

```
2026/10/14 01:48:24 GET    /users/alice    -> 200 via "GET /users/{name}"   (in 7µs)
2026/10/14 01:48:24 DELETE /health         -> 405 via "(no pattern — the mux answered itself)" (in 15µs)
2026/10/14 01:48:39 HEAD   /static         -> 307 via "/static/"            (in 10µs)
```

## 3. Key characteristics (all observed on Go 1.26.5)

### 3.1 Methods: scoping, partitioning, and the 405 with receipts

`GET /health` matches GET **and** HEAD — the stdlib defines HEAD as "GET,
minus the body" (browsers use it constantly). Every other method needs its
own pattern. Same path + different methods is legal: the two patterns match
disjoint request sets, so no conflict. The magic shows up on an unregistered
method — the mux distinguishes "wrong verb" from "wrong place":

```
$ curl -s -X DELETE -D - http://localhost:8090/health
HTTP/1.1 405 Method Not Allowed
Allow: GET, HEAD, POST
```

That `Allow` header is computed from the tree: every method that WOULD have
matched this (host, path), **sorted alphabetically**, with `HEAD` added
automatically whenever `GET` matches. A 405 means "right place, wrong verb —
here's the menu"; a 404 means "no pattern knows this path at all". Compare
`DELETE /ping`, where the pattern names no method:

```
$ curl -s -X DELETE http://localhost:8090/ping
pong (DELETE works here — the pattern names no method)
```

### 3.2 Wildcards: `{x}`, `{x...}`, and `PathValue`

`{x}` matches exactly ONE segment — the run between two literal slashes in
the **escaped** URL. It cannot be empty, cannot contain a slash, cannot be a
bare trailing slash:

```
$ curl -s http://localhost:8090/hello/rosen%20jr
hello, "rosen jr"! (wildcard {name} of pattern "GET /hello/{name}")

$ curl -s -w "[%{http_code}]" http://localhost:8090/hello/a/b
404 page not found          # {name} is one segment, this path has two

$ curl -s -w "[%{http_code}]" http://localhost:8090/hello/
404 page not found          # a trailing slash is not a matchable segment
```

`{x...}` must be the last segment and swallows everything remaining:

```
$ curl -s http://localhost:8090/files/a/b/c.txt
the whole rest of the path is "a/b/c.txt" (pattern "GET /files/{path...}")
```

Inside the handler, `r.PathValue("name")` returns the captured value,
**URL-unescaped for free** (see the `rosen jr` line above). Reading a name the
pattern doesn't have returns `""`. Also worth knowing: the whole captured
rest is unescaped too, so an encoded slash inside it becomes a real slash
*in the value* even though it never split the *path*:

```
$ curl -s "http://localhost:8090/files/a%2Fb"
the whole rest of the path is "a/b" ...
```

### 3.3 `{$}` — the end anchor (and honest 404s)

`"/"` alone is the catch-all (every path). `"GET /{$}"` is its opposite
extreme: **only** the bare root. [main.go](main.go) deliberately registers
no catch-all, so every unknown path really 404s:

```
$ curl -s http://localhost:8090/
the root, and only the root (pattern "GET /{$}")

$ curl -s -w "[%{http_code}]" http://localhost:8090/nope
404 page not found
```

(A catch-all has its uses — see the fileserver lesson — but it also *shadows*
404s and 405s: everything falls into it. Choosing whether to register `/` is
choosing whether you want honest 404s.)

### 3.4 Subtrees, trailing slashes, and the two automatic 307s

A pattern ending in `/` is a **subtree**. Requesting its bare form triggers
the mux's first automatic redirect:

```
$ curl -sI http://localhost:8090/static | grep -Ei "HTTP/|Location"
HTTP/1.1 307 Temporary Redirect
Location: /static/
```

Why redirect at all? So relative URLs resolve correctly (a page at `/static/`
can link `next.html`). Why **307** and not the classic 301? 307 guarantees
the method and body are preserved across the redirect — a POST stays a POST.
The second automatic redirect is **path sanitizing**: requests containing
`.`/`..` segments or repeated slashes never reach any handler:

```
$ curl --path-as-is -o /dev/null -w "%{http_code} -> %{redirect_url}\n" \
    "http://localhost:8090/users/../health"
307 -> http://localhost:8090/health
```

(Same reason `curl` needs `--path-as-is` here: otherwise *curl* cleans the
path and you'd be testing nothing.) This is also why path traversal attacks
die before seeing your handlers — the fileserver lesson showed the second
layer of that defense.

### 3.5 Precedence: "most specific wins", and conflicts PANIC

The rule, verbatim from the source (`pattern.go`): **P1 is more specific than
P2 if P2 matches all the requests of P1 and more** — a strict-subset
relationship. It ranks every axis: literal > `{x}` > `{x...}`, deeper >
shallower, method-scoped > methodless, host-scoped > hostless. That is why
this pair is legal and useful ([main.go](main.go) registers both):

```
$ curl -s http://localhost:8090/users/admin
you are THE admin: literal "GET /users/admin" beat the wildcard

$ curl -s http://localhost:8090/users/alice
hello user "alice", served by wildcard pattern "GET /users/{name}"
```

And when two patterns **overlap** but neither is more specific, registration
does not "pick one" or "last-write-wins" — it **panics at startup**, with a
message that names both patterns, both registration sites, and explains
itself in plain English. Real output, unedited:

```
panic: pattern "GET /users/{id}" (registered at ./.tmp/conflict/main.go:15)
    conflicts with pattern "GET /users/{name}" (registered at ./.tmp/conflict/main.go:13):
        GET /users/{id} matches the same requests as GET /users/{name}

goroutine 1 [running]:
net/http.(*ServeMux).register(...)
	/usr/local/go/src/net/http/server.go:2882
...
```

The panic text comes from `describeConflict` (`pattern.go:398`), which knows
the three failure shapes: *matches the same requests as* (equivalent),
*overlaps with* (both match some request, neither more specific), and the
method/host variants. The stack even points you at the exact registration
line. A routing table that cannot misroute silently — you get this
guarantee by *default*, not by writing tests.

### 3.6 Host patterns: one binary, many vhosts

`"api.example.com/hostonly"` matches only requests whose `Host` header is
that name. No DNS needed to try it, and **ports are stripped before
matching** (`stripHostPort(r.Host)`):

```
$ curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8090/hostonly
404                                                    # wrong host...

$ curl -s -H "Host: api.example.com:8090" http://localhost:8090/hostonly
you reached a HOST-scoped pattern; your Host: api.example.com:8090   # port ignored
```

One carve-out from the source: a host pattern beats a hostless pattern even
when they would otherwise conflict — a backwards-compatibility exception from
pre-1.22 Go.

### 3.7 Handlers run concurrently — state must be safe

`net/http` gives **every request its own goroutine**. Ten simultaneous
requests = ten goroutines inside the same handler, on many CPUs. The
`/visits` route in [main.go](main.go) is a handler that is a *struct with
state*, and the state is an `atomic.Int64` precisely because of this:

```
$ curl -s http://localhost:8090/visits; curl -s http://localhost:8090/visits
visit number 1 (pattern "GET /visits")
visit number 2 (pattern "GET /visits")
```

Change the counter to a plain `int` and the program still compiles, still
passes casual tests — and loses increments under real load. This is the
first place Go's concurrency model visibly enters web code.

### 3.8 The mux is a Handler — and it annotates your request

`*ServeMux` has a `ServeHTTP` method, so it satisfies `Handler` — you can
wrap it in middleware, register it inside another mux ([mount/](mount/)),
or pass it to `httptest`. While dispatching, `ServeMux.ServeHTTP` assigns
into the request itself (`server.go:2814`):

```go
h, r.Pattern, r.pat, r.matches = mux.findHandler(r)
```

so handlers (and middleware, after dispatch) can read `r.Pattern` — the
pattern string that matched. Observed via the middleware log: matches carry
the pattern; 404s and 405s leave it empty; redirects report the pattern that
*would have* matched:

```
GET    /users/alice    -> 200 via "GET /users/{name}"     # matched
DELETE /health         -> 405 via "(no pattern ...)"       # mux's own answer
HEAD   /static         -> 307 via "/static/"               # the subtree pattern
GET    //health        -> 307 via "GET /health"            # after sanitizing
```

### 3.9 Escaping: matched per segment, unescaped for comparison

The mux walks the **escaped** path and splits on literal `/` only — so
`%2F` never splits a segment — then **unescapes each segment** before
comparing it to pattern literals or capturing it (that's why `PathValue`
hands you `rosen jr`, not `rosen%20jr`). Patterns may contain `%XX` escapes
too: `"/a%2fb/"` is the pattern for the path whose second segment is `a/b`.
The fileserver lesson's `/files/docs%2Fcheatsheet.txt` experiment was this
rule biting a hand-rolled validator.

### 3.10 What the mux does NOT do

| Missing | The usual answer |
|---|---|
| authorization | middleware (`func(Handler) Handler`) before/around routes |
| query-string/JSON-body parsing | `r.URL.Query()`, `encoding/json` in handlers |
| per-route middleware stacks | wrap handlers at registration time (or use chi/gin) |
| regexp path constraints | validate in the handler, or choose a third-party router |
| automatic CORS | middleware |

Since Go 1.22 the stdlib mux covers most of what people historically pulled
`chi`/`gin`/`gorilla/mux` in for. Reach for those when you want route groups
with shared middleware chains, URL generation, or pattern libraries — not
for basic routing.

## 4. Key internal workings — how the mux actually does it

Four small files under `/usr/local/go/src/net/http/`. The tour below is
verified against this machine's Go 1.26.5 source.

### 4.1 A pattern is compiled at registration (`pattern.go`)

`parsePattern` turns the string into a small struct:

```go
type pattern struct {
	str      string   // the original string, what r.Pattern reports
	method   string   // "GET", "POST", ... or ""
	host     string   // "api.example.com", or ""
	segments []segment // the path, segment by segment
	loc      string   // file:line of the registration call — for panic messages
}

type segment struct {
	s     string // literal, or wildcard NAME, or "/" for {$}
	wild  bool   // {x}
	multi bool   // {x...}
}
```

Two pieces of sugar become visible here: a trailing `/` is stored as an
**anonymous multi wildcard** (that's why `/static/` is a subtree), and `{$}`
is stored as a **literal segment `"/"`** (that's why `/{$}` matches only the
bare root). Validation happens here, once, at registration — bad patterns
never reach request time.

### 4.2 Patterns are inserted into a routing tree (`routing_tree.go`)

Not a map, not a list — a decision tree:

```
root ── host ──┬─ "api.example.com" ── method ── "GET" ── segments...
               └─ "" (any host)     ── method ─┬─ "GET" ── segments...
                                                    │
                              segments: literal "users" ──┬─ literal "admin" ── leaf(pattern, handler)
                                                        └─ "" (wildcard) ── leaf(pattern, handler)
```

Each level of the tree branches on host first, then method, then one path
segment at a time. At each segment node, children live in a small map keyed
by the literal text, plus two special slots: `""` for a single wildcard and
a dedicated `multiChild` for `{x...}`. A **leaf** holds the pattern and its
handler. Registration also records patterns into a `routingIndex`
(`routing_index.go`) that narrows which patterns need conflict-checking
against a newcomer — that's how conflict detection stays cheap.

### 4.3 Matching: ordered tries with backtracking (`matchPath`)

The tree walk is where "most specific wins" *becomes mechanical*. For each
path segment, in order:

1. try the **literal** child with that exact text (most specific);
2. if that fails, the **single wildcard** child (`""`) — capturing the segment;
3. if that fails, the **multi wildcard** child — capturing the rest;
4. otherwise, fail — and the caller (the parent segment) **backtracks** and
   tries its own next-best option.

Backtracking is even documented with an example in the file header: given
patterns `/a/b/z` and `/a/{x}/c`, the path `/a/b/c` first tries `/a/b/z`,
fails at the last segment, backtracks one level, and succeeds via
`/a/{x}/c`. Method matching follows the same pattern (`matchMethodAndPath`):
exact method child first, then — for HEAD requests — the GET child, then the
no-method child. Host matching: host-specific first, then no-host.

### 4.4 Where the captured values live (`request.go`)

`PathValue` reads not a map but a **slice indexed by position**:

```go
func (r *Request) PathValue(name string) string {
	if i := r.patIndex(name); i >= 0 {   // where is "name" in the matched pattern?
		return r.matches[i]
	}
	return r.otherValues[name]           // values YOU injected via SetPathValue
}
```

The walk over the tree collects captures in `matches []string` in pattern
order, and `ServeMux.ServeHTTP` assigns the whole slice to the request in
one line (see 3.8). Values from **anonymous** multis (a trailing `/`) are
deliberately not recorded — there is no name to hang them on. And
`SetPathValue` writes to `r.otherValues`, a different bag — which is how
middleware can inject path-like values that handlers read with the same API.

### 4.5 The 405 hunt (`matchingMethods`)

When the (host, method, path) triple misses, the mux asks a different
question: "which METHODS *would* match this (host, path)?" It walks the
tree's method children running the same path match for each
(`matchingMethodsPath`), tries again with a trailing slash appended (the
subtree case), adds `HEAD` if `GET` matched, sorts the result, and returns
it as the `Allow` header — that's the receipt in 3.1. No method anywhere
for that path → the mux's own `NotFoundHandler` writes the familiar
`404 page not found`.

### 4.6 Redirects, before and instead of matching (`findHandler`)

`findHandler` (`server.go:2659`) is the whole pipeline:

```
escapedPath := r.URL.EscapedPath()           // match the ESCAPED path...
path        := cleanPath(escapedPath)         // ...but clean it first
host        := stripHostPort(r.Host)

n, matches := tree.match(host, method, path)
if inexact match && path has no trailing slash:
    retry match with path + "/" → if exact: 307 redirect to path+"/"
if path != escapedPath:                       // //x, /a/../b — needs cleaning
    307 redirect to the cleaned path (query string preserved)
if no match at all: → 405 hunt, else 404
else: dispatch, setting r.Pattern / r.matches
```

Note the order: sanitizing and canonicalization happen **before** your
handlers exist as far as the attacker is concerned; handlers only ever see
clean paths.

### 4.7 Mounting internals — why two patterns are "true" at once

`StripPrefix` clones the `Request` (`*r2 = *r` — a shallow copy of the
struct) before handing it down, so the inner mux writes `r2.Pattern` and
`r2.matches` on the **clone**, while your outer middleware still holds the
original. The mount experiment shows both logs — outer middleware says
`via "/v1/"` for `GET /v1/users/7` while the inner handler reports
`r.Pattern` as `"GET /users/{id}"`. One request, two truths, both accurate.

## 5. The mount example: muxes inside muxes

[mount/](mount/main.go) builds an API that doesn't know it lives under
`/v1`, then hangs it on an outer mux with the classic one-liner:

```go
mux.Handle("/v1/", http.StripPrefix("/v1", apiV1))
```

Three things to internalize from that line:

- **the pattern scopes** — `"/v1/"` (with trailing slash!) matches
  `/v1/...` and nothing else. A request for `/v1zzz` fails at the OUTER
  mux (404) and never reaches `StripPrefix`; had we registered `"/"`
  instead, `StripPrefix` would happily turn `/v1zzz` into `zzz` and leak it
  into the inner mux. Pattern = scope, StripPrefix = rewrite; they must
  agree.
- **the mux is a Handler** — `apiV1` is passed as an ordinary handler
  value; the outer mux neither knows nor cares that it is itself a mux.
- **cross-cutting concerns attach at any level** — wrap the outer mux and
  one middleware covers everything; wrap `apiV1` and it covers only the
  mounted API.

Observed matrix:

| Request | Result | Produced by |
|---|---|---|
| `GET /` | 200 outer landing | outer pattern `GET /{$}` |
| `GET /v1` | **307 → /v1/** | outer mux (subtree canonicalization) |
| `GET /v1/` | 200 "API v1 root", inner pattern `GET /{$}` | inner mux, post-strip |
| `GET /v1/users/7` | 200, `PathValue("id")` = 7 | inner mux |
| `DELETE /v1/health` | **405 `Allow: GET, HEAD`** | **inner** mux |
| `GET /v1/nope` | 404 page not found | **inner** mux |
| `GET /v1zzz` | 404 | **outer** mux (scoping!) |

The 404/405 rows are the payoff: error handling composes. Each mux answers
for its own table; the outer one never needs to know the inner one's routes.

## 6. Use cases

**Reach for ServeMux when...**

- **Any API, small to large.** Patterns with methods + wildcards are what
  `chi`/`gorilla` were imported for, pre-1.22. `GET /tasks/{id}` *is* the
  route; 405s and `PathValue` come along for free.
- **One binary, many concerns.** Landing page + `/static/` (fileserver
  lesson) + `/api/` routes, all in one mux, one process, one port.
- **Virtual hosts.** `api.example.com/...`, `admin.example.com/...` — same
  process, different `Host` headers, different route tables.
- **Sub-routers / versioning** ([mount/](mount/)): `/v1/`, `/v2/`, or
  admin areas as self-contained muxes — clean code layout AND clean URL
  layout.
- **Mounting third-party handlers.** Any `Handler` plugs in: `pprof`
  routes, a file server, a health endpoint from a library.

**Think twice when...**

- You want **route groups with middleware chains** (`/admin/*` gets auth +
  logging, `/public/*` gets none). Doable at registration time with the
  stdlib, but a router like chi expresses it directly.
- You need **URL generation** (reverse routing: "give me the URL for
  route X with id 7") or **regexp constraints** on wildcards.
- You want **automatic OPTIONS/HEAD handling beyond the basics**, or
  404/405 page customization without writing a wrapper — plan a small
  wrapper, or pull a router.

None of these mean "the stdlib mux is a toy" — it is the routing core under
everything, and `routing_tree.go`'s matcher is excellent. Start with it;
leave it when you can name the feature you miss.

## 7. The lab

`go run .` and `go run ./mount` from this folder, then poke:

```bash
# methods and the 405 receipt
curl -s http://localhost:8090/health              # GET twin
curl -s -X POST http://localhost:8090/health     # POST twin
curl -s -X DELETE -D - http://localhost:8090/health | head -3
curl -s -X DELETE http://localhost:8090/ping      # no method in pattern: still 200

# wildcards
curl -s http://localhost:8090/hello/rosen%20jr        # unescaped capture
curl -s -w "\n%{http_code}\n" http://localhost:8090/hello/a/b   # 404: two segments
curl -s http://localhost:8090/users/7/posts/3          # two captures
curl -s http://localhost:8090/files/a/b/c.txt          # multi wildcard

# precedence
curl -s http://localhost:8090/users/admin              # literal wins
curl -s http://localhost:8090/users/alice              # wildcard serves the rest

# redirects and anchors
curl -sI http://localhost:8090/static                  # 307 -> /static/
curl --path-as-is -o /dev/null -w "%{http_code} -> %{redirect_url}\n" \
     "http://localhost:8090//health"                   # sanitizing 307
curl -s http://localhost:8090/                        # GET /{$}
curl -s -w "\n%{http_code}\n" http://localhost:8090/nope   # honest 404

# host scoping and state
curl -s -H "Host: api.example.com:8090" http://localhost:8090/hostonly   # 200
curl -s http://localhost:8090/hostonly                                  # 404
curl -s http://localhost:8090/visits                                    # 1..N
for i in $(seq 1 20); do curl -s http://localhost:8090/visits & done; wait   # concurrency

# mounting
curl -s http://localhost:8091/v1/users/7               # inner PathValue
curl -s -X DELETE -D - http://localhost:8091/v1/health | grep -i allow  # inner 405
curl -s -w "\n%{http_code}\n" http://localhost:8091/v1zzz               # outer 404
```

And break the source, once, on purpose — in a scratch file:

```go
mux.HandleFunc("GET /users/{name}", h1)
mux.HandleFunc("GET /users/{id}", h2)     // panics: read the message, it's a lesson
```

## 8. Read the source — it's on your machine

Four files make up the entire 1.22+ router (GOROOT = `/usr/local/go`):

| Where | What it is |
|---|---|
| `pattern.go:19` | `pattern` struct — the compiled form of a pattern string |
| `pattern.go:61` | `segment` struct — literal / single / multi, plus the two sugar rules |
| `pattern.go:84` | `parsePattern` — the grammar, and all its error messages |
| `pattern.go:232` | `conflictsWith` — the precedence rules, as code |
| `pattern.go:398` | `describeConflict` — the panic text generator |
| `routing_tree.go:27` | `routingNode` — the tree node (children, multiChild, leaf fields) |
| `routing_tree.go:52` | `addPattern` — insert: host level → method level → segment levels |
| `routing_tree.go:117` | `match` — host fallback |
| `routing_tree.go:132` | `matchMethodAndPath` — exact → HEAD-as-GET → no-method fallback |
| `routing_tree.go:154` | `matchPath` — literal → wildcard → multi, **with backtracking** |
| `routing_tree.go:219` | `matchingMethods` — the 405 hunt |
| `routing_index.go:14` | `routingIndex` — how conflict checks stay cheap |
| `server.go:2575` | `ServeMux` struct — a tree + an index + an RWMutex |
| `server.go:2588` | `DefaultServeMux` — the global you're avoiding |
| `server.go:2659` | `findHandler` — the whole per-request pipeline |
| `server.go:2721` | `matchOrRedirect` — trailing-slash 307s |
| `server.go:2772` | `exactMatch` — what counts as "already exact" |
| `server.go:2798` | `matchingMethods` + the `Allow` header assembly |
| `server.go:2814` | `ServeMux.ServeHTTP` — one assignment line that sets `r.Pattern` |
| `server.go:2886` | `registerErr` — validation, `runtime.Caller` for locations, conflict error |
| `request.go:325` | `Request.Pattern` — a plain `string` in Go 1.26.5 |
| `request.go:1469` | `PathValue` / `SetPathValue` — the slice + the side-map |

Reading order: `ServeMux.ServeHTTP` → `findHandler` → `matchPath`. That's
the whole story in ~150 lines with comments.

## 9. Homework

1. **Prove concurrency to yourself.** Make a handler `time.Sleep(1*time.Second)`
   and fire 10 requests in parallel (`seq 1 10 | xargs -P10 -I{} curl -s URL`).
   Total wall time ~1s, not ~10s: goroutine per request, live.
2. **HEAD the ping.** Register `"HEAD /ping"` next to the methodless
   `"/ping"`. Which serves `HEAD /ping` now? (Precedence: HEAD-scoped beats
   methodless for HEAD requests.) Then re-check `GET /ping` still works.
3. **A tidy CRUD router.** Build `GET /tasks`, `POST /tasks`,
   `GET /tasks/{id}`, `PUT /tasks/{id}`, `DELETE /tasks/{id}` — then use
   `curl -X PATCH` and *read the Allow header the mux computed for you*.
4. **Custom 404.** The mux's 404 body is fixed. Wrap the mux in a handler
   that uses your `statusRecorder` to detect 404s and rewrite them into a
   branded page. (Careful: you must not have written anything yet — think
   about why, re-read the fileserver lesson's middleware notes.)
5. **Test routing without a network.** `httptest.NewServer(mux)` in a
   `_test.go`: assert `GET /tasks/7` returns 200 with `id=7`, and that
   `DELETE /tasks/7` returns exactly `Allow: GET, HEAD, PUT`.
6. **Two vhosts, one binary.** Register `site.example.com/{$}` and
   `api.example.com/{$}` with different content; drive both with
   `curl -H "Host: ..."`. Add a hostless `GET /{$}` and check it wins for
   every OTHER host.
7. **(Advanced)** Rebuild `r.otherValues`: middleware that reads
   `?user=rosen` from the query and injects it with
   `r.SetPathValue("user", ...)` — handlers now call
   `r.PathValue("user")` uniformly for path- and query-sourced values.

## 10. Glossary (one line each)

| Term | Meaning |
|---|---|
| mux / multiplexer | the switchboard that maps incoming requests to handlers by pattern |
| pattern | the registration key: `[METHOD ][HOST]/[PATH]` with wildcards |
| wildcard `{x}` | matches exactly one path segment; captured into `r.PathValue("x")` |
| multi wildcard `{x...}` | matches the rest of the path, slashes included; last position only |
| `{$}` | end anchor: the pattern matches where the path ends — `"GET /{$}"` is only `/` |
| subtree | pattern ending in `/`; stored internally as an anonymous multi wildcard |
| precedence | most specific pattern wins; specificity = matches a strict subset |
| conflict | two overlapping patterns, neither more specific → panic at registration |
| canonical redirect | 307 to the "one true form" (`/static` → `/static/`, `//x` → `/x`) |
| `DefaultServeMux` | the package-global mux used by `http.Handle` and `ListenAndServe(addr, nil)` |
| adapter type | a type whose job is giving an existing value an interface — `http.HandlerFunc` |
| vhost | serving different route tables per `Host` header, from one process |
| `r.Pattern` | the pattern string that matched this request (empty on 404/405), set by the mux |
| goroutine-per-request | each request is served in its own goroutine — handler state must be safe |

---

*Companion folder: [`webdev/fileserver`](../fileserver/README.md) — where a
`FileServer` handler meets this same mux. Next candidates: middleware
patterns & `httptest`, or `html/template` for server-rendered pages.*
