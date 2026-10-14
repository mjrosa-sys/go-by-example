# Web development in Go — handlers: the one interface

This folder answers three questions, in order:

1. **How are handlers used?** (the four ways to make one, and the stdlib's
   ready-made ones)
2. **What are their key characteristics?** (the ResponseWriter contract,
   sniffing, chunking, panics, cancellation — all observed live)
3. **How do they work internally?** (the dispatch path, the `response`
   struct, and the famous *"Life Of A Write"* pipeline)

Everything was **written and verified against Go 1.26.5**, the toolchain on
this machine. Every output block below is a real observation from running
these very programs with `curl`. The server internals live in
`/usr/local/go/src/net/http/server.go` — section 7 is a map to its most
interesting lines.

> **How to study this folder:** run both examples, read [main.go](main.go)
> top to bottom (the comments ARE the lesson), then run the lab in
> section 6 with both servers' terminals visible — half of this lesson
> happens in the **server logs**, not in the client.

## What is in this folder

| Path | What it teaches | Port |
|---|---|---|
| [main.go](main.go) | the tour: every way to make a handler, the ResponseWriter rules, streaming, panics, context | 8095 |
| [middleware/](middleware/main.go) | handlers composing handlers: a production-shaped chain, incl. the stdlib's `TimeoutHandler` and `MaxBytesHandler` | 8096 |

## 0. The one interface

Everything you have met so far — `http.FileServer`, `*ServeMux`, `StripPrefix`,
your middleware — reduces to this (server.go:88):

```go
type Handler interface {
	ServeHTTP(ResponseWriter, *Request)
}
```

A handler receives the response being assembled (`w`) and everything the
client asked (`r`), writes what it wants, and **returns**. That's the entire
programming model. Returning is how you say "the response is finished" —
and the doc adds the fine print: never touch `w` again after returning, and
don't mutate `r` (reading its body is fine).

```
http.Server          owns TCP: one goroutine per CONNECTION (conn.serve)
   │  for each request on the connection:
   ▼
readRequest          parses bytes into a *Request; builds its context
   │
   ▼
serverHandler        "handler := srv.Handler; if nil, DefaultServeMux"
   │                ← (server.go:3301) — your mux goes in here
   ▼
YOUR Handler         (mux → middleware → handler: all the same interface)
   │  ServeHTTP(w, r)
   ▼
response writer      buffers, sniffs, chunks, flushes — the topic of §3
```

## 1. Demonstrations: how handlers are used

### 1.1 Four ways to be one (all four are in [main.go](main.go))

```go
// Way 1: function + adapter, out loud. HandlerFunc's whole implementation:
//     func (f HandlerFunc) ServeHTTP(w ResponseWriter, r *Request) { f(w, r) }
mux.Handle("GET /handlerfunc", http.HandlerFunc(myHandler))

// Way 2: mux.HandleFunc — the sugar that does Way 1 for you
mux.HandleFunc("GET /small", myHandler)

// Way 3: a struct with a ServeHTTP METHOD — handlers with config/state
type greeter struct{ greeting string }
func (g greeter) ServeHTTP(w http.ResponseWriter, r *http.Request) { ... }
mux.Handle("GET /greeter/{name}", greeter{greeting: "Hello"})

// Way 4: a factory returning a closure — config without a type
func makeFarewell(word string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ... })
}
mux.Handle("GET /farewell", makeFarewell("Goodbye"))
```

Which to pick: Way 2 for leaf handlers; Way 3 the moment dependencies
appear (a database, a logger — this is how real code passes them); Way 4
for small factories; Way 1 mostly exists to explain that 2 and 4 are the
same thing. One compiler-enforced nuance: wrappers like `TimeoutHandler`
demand a real `Handler` value — passing a bare function is a compile error
until you wrap it in `http.HandlerFunc`.

### 1.2 The stdlib's ready-made handlers

`go doc net/http.Handler` literally ends with a list of functions that
return Handlers — the standard library is its own best middleware library:

| Handler | What it does |
|---|---|
| `http.FileServer(http.Dir("."))` | serves a whole directory tree (see the fileserver lesson) |
| `http.StripPrefix("/static", h)` | removes a URL prefix before calling h |
| `http.TimeoutHandler(h, 2*time.Second, "msg")` | hard wall-clock budget: 503 + msg on overrun |
| `http.MaxBytesHandler(h, 4<<10)` | 4KB ceiling on the request body (the reader errors past it) |
| `http.RedirectHandler(url, code)` | every request becomes a redirect |
| `http.NotFoundHandler()` | the mux's own 404, as a value |
| `*ServeMux` | the router — a Handler like any other (see the mux lesson) |
| `http.HandlerFunc(fn)` | your function, promoted |

### 1.3 Run the examples

From this directory (`webdev/handlers`):

```bash
go run .              # the tour   -> http://localhost:8095
go run ./middleware   # the chain  -> http://localhost:8096
```

## 2. Key characteristics (all observed on Go 1.26.5)

### 2.1 The ResponseWriter contract: status, headers, body — in that order

The response is assembled in one fixed sequence, and the tour's routes
demo each rule. A handler that does nothing but `fmt.Fprint(w, ...)`:

```
$ curl -s -D - http://localhost:8095/small
HTTP/1.1 200 OK
Date: Wed, 14 Oct 2026 15:42:10 GMT
Content-Length: 16
Content-Type: text/plain; charset=utf-8

a small response
```

Three headers you never set, computed for you: the **200** (implicit: the
first `Write` triggers `WriteHeader(StatusOK)`), the **Content-Type**
(sniffed — next), and the **Content-Length** (free, because the whole body
fit in the 2048-byte buffer — section 3.2). The reverse rule is the one
that bites, and `/late` demonstrates it: **headers must be set before the
first byte of body**, because your header map is *cloned into the chunk
writer at WriteHeader time* (server.go:1183). After that, mutations edit a
map nobody will ever send:

```
$ curl -s -D - http://localhost:8095/late | grep -E "^HTTP|^X-"
HTTP/1.1 200 OK
X-Before: shipped        <- set before the first Write: travels
                         <- X-After was set after: lost forever
```

And calling `WriteHeader` twice is not an error — it's IGNORED, with a
server-side warning that names the caller it saw. Observed on this
machine, with the logging middleware wrapping the writer:

```
http: superfluous response.WriteHeader call from main.(*statusRecorder).WriteHeader (main.go:395)
```

Read that carefully: the warning points at the WRAPPER's method, not the
handler — `relevantCaller()` reports who called the real `WriteHeader`.
The same route exposed a second wrapper truth: our recorder stored the
ignored second status, so the middleware log printed
`GET /nocontent -> 500` while the client received **204**. Recorders report
what you *attempted*; only the wire is truth.

### 2.2 Content-Type: sniffed, or obeyed — never checked

If you stay silent, the server reads your first 512 bytes and *sniffs*
(`http.DetectContentType`, the same algorithm browsers use on files):

```
$ curl -sI http://localhost:8095/html | grep -i content-type
Content-Type: text/html; charset=utf-8      <- sniffed from "<html>..."
```

If you speak first, the server obeys you without question —
`Content-Type` is a **promise you make**, not a fact the server validates:

```
$ curl -sI http://localhost:8095/typed | grep -i content-type
Content-Type: application/json               <- body is plain text; header wins
```

### 2.3 Content-Length is free — under 2KB, over nothing

A response whose body fits in the 2048-byte chunking buffer
(`bufferBeforeChunkingSize`, server.go:341) and never called `Flush()`
gets a computed Content-Length — you saw `Content-Length: 16` above.
Cross the line and the server switches to **chunked transfer encoding**
(no total length is knowable in advance):

```
$ curl -s -D - -o /dev/null http://localhost:8095/big
HTTP/1.1 200 OK
Transfer-Encoding: chunked                  <- 8KB body, no length known
```

This one threshold is why "small handler" responses look simple and
streaming looks different — same mechanism, one 2KB question: *did the
whole body fit before we had to flush?*

### 2.4 Bodies some responses are not allowed to have

`204 No Content` forbids a body outright: any `Write` is refused with the
sentinel error `ErrBodyNotAllowed` —
`"http: request method or response status code does not allow body"`
(server.go:43). Same for 1xx and 304. The `/nocontent` route shows both
the refusal and the correct reaction (read the error — it's a return
value, not a panic):

```
$ curl -s -D - http://localhost:8095/nocontent
HTTP/1.1 204 No Content
Date: ...
                        <- the body we tried to write: refused, by rule

[server log]  write after 204 refused: http: request method or response status code does not allow body
```

**HEAD is the other body-less case, but different**: for HEAD requests
the server COUNTS your writes and drops them, so headers can still
describe the body you *would* have sent:

```
$ curl -sI http://localhost:8095/small
HTTP/1.1 200 OK
Content-Length: 16        <- the handler wrote 16 bytes; they were dropped
```

(That is also why a GET pattern matching HEAD — mux lesson — is safe:
the machinery on both sides cooperates.)

### 2.5 Header keys are canonicalized

Set `X-Request-ID`, and the wire says `X-Request-Id` — observed on the
middleware example, where the `attachRequestID` middleware calls
`w.Header().Set("X-Request-ID", id)` and curl reports:

```
$ curl -sD - -o /dev/null http://localhost:8096/public/health
HTTP/1.1 200 OK
X-Request-Id: 44e9d160023102bd      <- the header we set as "X-Request-ID"
```

The Header map canonicalizes keys to MIME form (each dash-separated word
capitalized, rest lowercase). Case never matters for HTTP, but it will
surprise your `grep X-Request-ID` — as it surprised ours.

### 2.6 Optional abilities: streaming needs `Flusher`

`Flush()` is NOT on the ResponseWriter interface — it's an *optional*
interface (`http.Flusher`), and the `/stream` route uses
`http.NewResponseController(w)` (Go 1.22's official route to optional
abilities) to push ticks out live, one every 400ms:

```
$ curl -sN --max-time 3 http://localhost:8095/stream     (-N: no client buffering)
tick 1 at 12:43:16.090
tick 2 at 12:43:16.491        <- timestamps prove live arrival,
tick 3 at 12:43:16.892          not one burst at the end
...
```

The catch that makes this interesting: **wrappers hide optional
interfaces**. Our statusRecorder middleware sits between the handler and
the real writer; a type assertion `w.(http.Flusher)` on it fails, so
naive streaming code breaks *behind middleware*. The fix is the `Unwrap()`
convention — the wrapper exposes what it wraps, and
`ResponseController` climbs the ladder (responsecontroller.go) until it
finds a writer that really can flush. Both examples' `statusRecorder`
implements `Unwrap()` for exactly this reason; three lines of glue that
keep `/stream` and `/private/flush` working through a four-layer chain.
After the first flush the response is chunked forever — a flush is a
promise that no Content-Length is coming.

### 2.7 Handlers run concurrently — and the client can hang up

Two facts from the server side that every handler author needs
(the mux lesson showed the first; here comes the second):

1. **goroutine-per-request** — many requests = many goroutines inside your
   `ServeHTTP` at once. Read-only config is automatically safe; mutated
   state needs sync (see the mux lesson's atomic counter).
2. **`r.Context()` is the client's leash on you.** It's created per
   request (server.go:1040) and canceled when the client disconnects or
   the handler returns. `/slow` does five seconds of "work" while
   *selecting* on it:

```go
select {
case <-time.After(5 * time.Second):
	fmt.Fprintln(w, "finished the full 5 seconds of work")
case <-r.Context().Done():
	log.Printf("/slow: client went away after %v — aborting remaining work", ...)
}
```

Observed: `curl --max-time 1` bails at one second, and the handler stops
immediately instead of burning the remaining four:

```
[server log]  /slow: client went away after 1.003s — aborting remaining work
```

Without the select (a plain `time.Sleep`), the goroutine wakes up alone
and writes into a dead connection — wasted work. `ctx.Err()` tells you
*why* you were canceled; the two wrap-up helpers worth knowing are
`http.NewRequestWithContext` on the client side and `context.WithTimeout`
when *you* impose the budget.

### 2.8 Panics are contained — twice over

A panicking handler CANNOT kill the server. That's a documented
guarantee: `conn.serve` recovers every panic (server.go:1904), logs
`"http: panic serving <addr>"` plus a stack trace, and closes **that
connection**. Observed on `/boom`:

```
$ curl -s -w "[exit %{exitcode} status %{http_code}]" http://localhost:8095/boom
[exit 52 status 000]          <- curl 52 = "empty reply": no HTTP response at all

[server log]  http: panic serving [::1]:60504: the handler exploded
goroutine 30 [running]:
net/http.(*conn).serve.func1()                    <- the recover defer
...
main.boomHandler(...)                              <- your panic
...
main.main.logRequests.func2(...)                  <- your middleware frames
net/http.serverHandler.ServeHTTP(...)
net/http.(*conn).serve(...)
created by net/http.(*Server).Serve in goroutine 1
```

Two things to note in that stack: (1) the *entire middleware chain is
visible in it* — panics narrate your architecture; (2) no 500 is sent —
the client gets silence. Production servers therefore add their own
**recover middleware** (the [middleware/](middleware/main.go) example has
one), which converts panics into a real `500` response on a connection
that survives:

```
$ curl -w "[status %{http_code}]" -H "Authorization: Bearer opensesame" \
       http://localhost:8096/private/panic
internal server error
[status 500]                          <- a proper response; server healthy after
```

And there is one sanctioned *silent* panic: `panic(http.ErrAbortHandler)`
("I'm bailing out, don't log it"). Same connection drop as `/boom`, no
log line — verified: `/boom` and `/abort` look identical to the client,
but only one of them is noisy on the server.

### 2.9 The fine print of the contract (from the Handler doc, verified)

| Rule | Why it exists |
|---|---|
| never use `w` after `ServeHTTP` returns (or concurrently) | the response is finalized in `finishRequest` right after you return |
| don't mutate `r` (reading `r.Body` is fine) | other goroutines/goroutine-per-request share the request's lifetime |
| read the request body BEFORE writing the response | once headers flush, HTTP/1.x may not let you keep reading |
| return quickly; long work should watch `r.Context()` | the goroutine and its connection are held until you return |
| `Write`'s error is data, not a panic | a hung-up client makes writes fail — that's normal, handle it |

### 2.10 What handlers do NOT get

A handler is deliberately tiny — everything else is composition: no
routing (mux lesson), no body size limits (`MaxBytesHandler`), no time
budgets (`TimeoutHandler`), no auth (middleware), no cookies (`r.Cookie`),
no forms (`r.ParseForm` reads the body for you), no JSON (`encoding/json`),
no templates (`html/template`). The interface's smallness is WHY all of
those plug into each other.

## 3. How handlers work internally

Four mechanisms, all in `/usr/local/go/src/net/http/server.go`.

### 3.1 Dispatch: how your handler gets called

Per connection, `conn.serve` (server.go:1897) runs a loop: read a request
(`readRequest`, which parses headers and creates the request's context at
server.go:1040), then hand the pair to your tree with one call
(server.go:2073, via `serverHandler.ServeHTTP`):

```go
func (sh serverHandler) ServeHTTP(rw ResponseWriter, req *Request) {
	handler := sh.srv.Handler
	if handler == nil {
		handler = DefaultServeMux          // ← where the "nil Handler" magic lives
	}
	...
	handler.ServeHTTP(rw, req)             // ← your entire web program, one call
}
```

When your handler returns, `finishRequest` flushes anything left in the
buffers and decides whether the connection may be reused: it will drain up
to `maxPostHandlerReadBytes` (256KB, server.go:1112) of unread request
body to enable keep-alive — beyond that, the connection is closed.

### 3.2 The `response` struct is your ResponseWriter

The `w` you receive is `*response` (server.go:426): it holds the
connection, the request, your header map (`handlerHeader`), the bytes
written so far, the status, and — the key to everything in section 2 —
`w *bufio.Writer` with a **2048-byte** buffer feeding a `chunkWriter`.
The stdlib documents the whole pipeline in a famous comment,
"The Life Of A Write" (server.go:1586), paraphrased faithfully:

```
your w.Write(data)
  │
  ├─ implicit WriteHeader(200) if no status was written yet
  ├─ refused if the status forbids a body (204/304/1xx/HEAD rules)
  ├─ refused if you already wrote more than a declared Content-Length
  ▼
1. *response (w)                 the ResponseWriter you know
2. bufio.Writer (2048 bytes)     the chunking buffer: if the handler
3. chunkWriter                    finishes before this fills, the whole
   │                              body has a length → auto Content-Length
   ├─ writeHeader(final bytes):  otherwise → chunked encoding
   │    · clones your header map (the headers-first rule)
   │    · sniffs Content-Type from the first ≤512 bytes if silent
   │    · adds Date; writes status line + headers (or chunk size prefix)
4. conn.bufw (4KB bufio)
5. the net.Conn                   the socket
```

That is the entire mystery of "where did Content-Length come from" and
"when did my headers actually ship": they were decided the moment the
buffer had to move.

### 3.3 Keep-alive, HEAD, and the 256KB drain

`finishRequest` closes the connection unless: HTTP/1.1 (or negotiated),
no `Connection: close`, a known body length (Content-Length or chunked),
and the unread remainder of the *request* body is small enough to drain
(≤256KB). HEAD responses skip the body but keep the framing headers. A
handler that over-writes its declared Content-Length trips
`ErrContentLength` ("wrote more than the declared Content-Length") and the
connection is closed rather than send a corrupt frame.

### 3.4 TimeoutHandler and MaxBytesHandler: stdlib middleware anatomy

`TimeoutHandler` (server.go:3787) runs your handler in **another goroutine**
with a timer: on the deadline it writes 503 + your message itself, and the
stranded handler's later writes return `ErrHandlerTimeout`. This exists
precisely for handlers that DON'T watch `r.Context()` — wrapping is how
you put a budget on code you can't fix. Observed: 3-second handler, 2s
ceiling, client sees the 503 at `took 2.001306s`.

`MaxBytesHandler` wraps only `r.Body` (in a cloned request — requests
are values; the mux lesson's `StripPrefix` clones for the same reason).
Past the limit, reads fail with the typed error `*http.MaxBytesError`
(request.go:1194) — your handler matches it with `errors.As` and answers
413. And the devious detail: `MaxBytesReader` **calls back into the
response** (server.go:564, `requestTooLarge`) to force `closeAfterReply`,
so an oversized body can never squat on a keep-alive connection.

## 4. The middleware example: a production-shaped chain

[middleware/](middleware/main.go) assembles the stack a small service
would actually run. Order is policy — trace it before changing it:

```
logRequests            outermost: sees the TRUE status of everything below
  attachRequestID       X-Request-Id header + context.WithValue
    recoverPanics       panics below become clean 500s (connection survives)
      requireAuth         guards /private/* with a bearer token (401 otherwise)
        mux               the router
          TimeoutHandler    stdlib: 2s wall-clock ceiling   (on /private/slow)
          MaxBytesHandler   stdlib: 4KB body ceiling        (on /private/echo)
          handlers         business logic
```

Why each layer is where it is:

- **recover inside requestID & logging**: so a panic's 500 is logged and
  carries the same id the client got.
- **recover outside auth**: so a bug IN the auth middleware is also
  recovered.
- **logging outermost**: so 401s and 500s appear in the log, not just 200s.
- **`requestID` uses context.WithValue** — the one legitimate job of
  context values: request-scoped data crossing middleware boundaries,
  read below via `r.Context().Value(ctxKeyRequestID{})` with a private
  struct key (zero-byte type, no cross-package collisions). For config
  and dependencies, struct fields (Way 3) remain the honest choice.

Observed end to end:

```
$ curl -D - http://localhost:8096/public/health
X-Request-Id: 44e9d160023102bd
healthy. your request id is 44e9d... (delivered via context)

$ curl http://localhost:8096/private/report
missing or invalid bearer token        [401]

$ curl -H "Authorization: Bearer opensesame" http://localhost:8096/private/report
the secret report: everything is fine.  [200]

$ curl -H "Authorization: Bearer opensesame" http://localhost:8096/private/slow
the report is taking too long           [503, took 2.001s]

$ head -c 2048 /dev/zero | curl --data-binary @- -H "$AUTH" http://localhost:8096/private/echo
received 2048 bytes — thanks!          [200]

$ head -c 8192 /dev/zero | curl --data-binary @- -H "$AUTH" http://localhost:8096/private/echo
request too large: http: request body too large   [413]

$ curl -H "$AUTH" http://localhost:8096/private/flush
flushed line 1 at 12:44:43.989          <- streaming THROUGH the whole chain:
flushed line 2 at 12:44:44.290             works because of Unwrap()
```

And the recovered-panic stack trace (from the server log) is the chain
made visible — every frame, outside in:

```
main.panicHandler(...)                          ← the bug
net/http.HandlerFunc.ServeHTTP(...)
net/http.(*ServeMux).ServeHTTP(...)
main.main.recoverPanics.func2(...)              ← the recover
net/http.HandlerFunc.ServeHTTP(...)
main.main.requireAuth.func3(...)                ← the gate
net/http.HandlerFunc.ServeHTTP(...)
main.main.attachRequestID.func4(...)             ← the id
net/http.HandlerFunc.ServeHTTP(...)
main.main.logRequests.func5(...)                 ← the logger
net/http.serverHandler.ServeHTTP(...)
net/http.(*conn).serve(...)
```

Note what's NOT there: `net/http.(*conn).serve.func1` — the conn-level
recover. Your middleware caught the panic first, so the stdlib's
last-resort never fired. Two safety nets, layered.

## 5. Use cases: where handlers are the right tool

- **Every route, obviously** — the handler IS the unit of "answer this
  request".
- **Handlers as pluggable products**: the interface is the ecosystem
  contract. Anything that exposes `net/http/pprof`'s routes, Prometheus
  metrics, health endpoints, or a webhook receiver drops into your mux as
  a Handler value — zero glue.
- **Streaming: SSE, progress, tailing logs** — `Write` + `Flush` + a
  loop + `r.Context()`; HTTP/1.1 chunked does the rest.
- **Long-running APIs** — the `select`-on-context pattern with
  cancellation-aware work (`ctx`-carrying database calls) is what keeps
  goroutine counts honest under load.
- **Wrapping what you can't fix** — `TimeoutHandler` around a slow legacy
  handler, `MaxBytesHandler` around any body reader: stdlib seatbelts
  before every custom one.

## 6. The lab

Run both servers, keep their terminals visible, and:

```bash
B=http://localhost:8095
M=http://localhost:8096
A="Authorization: Bearer opensesame"

curl -s -D - $B/small                  # implicit 200, sniffing, auto Content-Length
curl -sI $B/html                       # sniffed text/html
curl -sI $B/typed                      # explicit header wins
curl -s -D - $B/late | grep -E "^X-"   # X-After never ships
curl -s -D - -o /dev/null $B/big      # Transfer-Encoding: chunked
curl -s -D - $B/nocontent              # 204; then READ THE SERVER LOG
curl -sI $B/small                      # HEAD: length kept, body dropped
curl -sN $B/stream                     # live ticks (Ctrl+C to stop early)
curl -s $B/boom; echo "exit $?"        # 52: panic = dropped connection
curl -s $B/abort; echo "exit $?"       # same... and NO log line
curl --max-time 1 $B/slow; echo        # abandon /slow, watch the log
curl -s $B/handlerfunc; curl -s $B/greeter/reader; curl -s $B/farewell

curl -sD - -o /dev/null $M/public/health   # note X-Request-Id (canonicalized!)
curl -s $M/private/report                   # 401
curl -s -H "$A" $M/private/report           # 200
curl -s -H "$A" $M/private/slow             # 503 after exactly 2s
head -c 8192 /dev/zero | curl -s --data-binary @- -H "$A" $M/private/echo   # 413
curl -sN -H "$A" $M/private/flush           # streams through 4 wrapper layers
curl -s -H "$A" $M/private/panic            # recovered 500 — server healthy
curl -s -o /dev/null -w "%{http_code}\n" $M/public/health   # still 200
```

The two most instructive places to look are the SERVER LOGS: after
`/nocontent` (two warnings) and after `/boom` vs `/abort` (noise vs
silence).

## 7. Read the source — it's on your machine

All of this lives in `/usr/local/go/src/net/http/server.go` (~3500 lines,
generously commented) plus two small satellites:

| Where | What it is |
|---|---|
| `server.go:88` | the `Handler` interface — the contract, including the panic rule |
| `server.go:2282` | `HandlerFunc` — the 3-line adapter that makes functions handlers |
| `server.go:341` | `bufferBeforeChunkingSize = 2048` — the chunking threshold |
| `server.go:426` | `type response struct` — your ResponseWriter's real body |
| `server.go:564` | `requestTooLarge` — the MaxBytesReader callback |
| `server.go:1040` | request context creation (in `readRequest`) |
| `server.go:1149` | `WriteHeader` — superfluous warning + header map clone |
| `server.go:1258` | `chunkWriter.writeHeader` — sniffing & framing decisions |
| `server.go:1477` | HEAD/204/304: where bodies are refused or discarded |
| `server.go:1586` | **"The Life Of A Write"** — the pipeline comment, worth reading verbatim |
| `server.go:1620` | `Write` — implicit 200, ErrBodyNotAllowed, ErrContentLength |
| `server.go:1664` | `finishRequest` — final flush + keep-alive decision |
| `server.go:1112` | `maxPostHandlerReadBytes` — the 256KB keep-alive drain |
| `server.go:1897` | `conn.serve` — the goroutine-per-connection loop |
| `server.go:1904` | the recover: panics contained, `ErrAbortHandler` exempt |
| `server.go:2073` | the dispatch frame (visible in every panic stack) |
| `server.go:2201` | `Hijack` — taking the connection away from HTTP (websockets) |
| `server.go:3301` | `serverHandler.ServeHTTP` — `nil` → `DefaultServeMux`, one call in |
| `server.go:3787` | `TimeoutHandler` — stdlib middleware with its own goroutine |
| `request.go:1194` | `MaxBytesError` — the typed "body too large" sentinel |
| `responsecontroller.go` | `ResponseController` + the `Unwrap()` ladder |
| `contenttype.go` | `DetectContentType` — the sniffer |

Reading order: `Handler` (the contract) → "Life Of A Write" (the plumbing)
→ `conn.serve` (the loop that calls you).

## 8. Homework

1. **Trailers.** Make a handler that declares `w.Header().Set("Trailer",
   "X-Done")`, writes a body, then sets `w.Header().Set(http.TrailerPrefix+
   "X-Done", "true")` after the writes. Inspect with `curl -v` — the header
   arrives after the body. (The one sanctioned post-write header.)
2. **Break a wrapper.** Delete `Unwrap` from `statusRecorder` and watch
   `/private/flush` degrade. Explain the error path (which layer detected
   it, what the client sees).
3. **The honest recorder.** Make `statusRecorder` refuse to record the
   phantom 500: hint — check `rec.ResponseWriter`'s... actually the fix
   used by real libraries: only record the FIRST status per request, and
   compare against the log line afterwards.
4. **A middleware of your own: CORS.** For responses to requests with an
   `Origin` header, set `Access-Control-Allow-Origin: *`; handle the
   `OPTIONS` preflight (method check, `Access-Control-Allow-Methods`).
   Test with curl's `-H "Origin: example.com"`.
5. **Context budget.** Rewrite `/slow` to use `context.WithTimeout` +
   `select` instead of `time.After` — then explain why `time.After`'s
   timer leaks until it fires (it stays in the timer heap) and why the
   select-on-context version is the production idiom.
6. **Server-Sent Events, for real.** `/stream` is 90% of an SSE endpoint:
   set `Content-Type: text/event-stream` and write lines in the
   `data: <payload>\n\n` format; consume it with `curl -N` and then with
   the browser's `new EventSource("/stream")`.
7. **Test handlers without a server.** `httptest.NewRecorder()` gives you
   a `ResponseWriter` you can inspect (`rec.Code`, `rec.Body`,
   `rec.Header()`); call `yourHandler(rec, req)` directly and assert.
   Then do it again through `httptest.NewServer` to see both levels.

## 9. Glossary (one line each)

| Term | Meaning |
|---|---|
| `http.Handler` | the one-method interface: `ServeHTTP(ResponseWriter, *Request)` |
| `ResponseWriter` | the response under construction: header map + write + status |
| `http.HandlerFunc` | the adapter type that turns a function into a Handler |
| adapter type | a type whose only purpose is giving an existing value a new interface |
| middleware | an ordinary handler that calls another: `func(Handler) Handler` |
| implicit 200 | the first `Write` writes the status line if you didn't |
| sniffing | Content-Type guessed from the first ≤512 bytes (`DetectContentType`) |
| chunked encoding | body framing with per-block length prefixes, when total size is unknown |
| `bufferBeforeChunkingSize` | the 2048-byte threshold between "auto Content-Length" and chunking |
| `Flusher` | optional interface for pushing buffered bytes to the client now |
| `Unwrap()` | a wrapper's "here's the writer inside me" method; the stdlib's ladder |
| `ResponseController` | Go 1.22+ official way to Flush/Hijack/set deadlines through wrappers |
| `r.Context()` | the request's cancellation channel: dead when the client hangs up |
| `ErrAbortHandler` | the silent bail-out panic: drop the connection, skip the log |
| keep-alive | connection reuse, granted only when the response framing is unambiguous |
| trailers | headers sent AFTER the body, declared upfront via the `Trailer` header |

---

*This completes the web-development triad: [fileserver](../fileserver/README.md)
(a Handler that reads disk), [servemux](../servemux/README.md) (a Handler
that picks handlers), and handlers (the interface they all share). Natural
next steps: `html/template` for server-rendered pages, or middleware +
`httptest` as a deeper follow-up to this folder.*
