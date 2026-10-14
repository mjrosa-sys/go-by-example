// webdev/handlers — a guided tour of http.Handler, the ONE interface that
// all of Go web development is built on.
//
// WHAT IS A HANDLER?
//
// One method:  ServeHTTP(ResponseWriter, *Request)
//
// That's the entire contract. A Handler receives the request (r) and the
// response being assembled (w), writes what it wants, and RETURNS. That is
// the whole verb-object structure of Go web programming: the Server moves
// bytes, the mux picks a handler, the handler answers. Everything you have
// met so far IS one: http.FileServer returns one, *ServeMux is one, every
// middleware is one. This tour shows the ways handlers are MADE, the rules
// of the ResponseWriter they write to, and what the server does around
// them: implicit headers, sniffing, chunking, panics, and cancellation.
//
// HOW TO RUN
//
//	go run .           # from THIS directory
//
// then open http://localhost:8095 and try (full lab in README.md):
//
//	GET /small          -> no headers set: 200, sniffed type, auto Content-Length
//	GET /html           -> Content-Type SNIFFED from the first bytes
//	GET /typed          -> explicit Content-Type wins (never validated!)
//	GET /late           -> a header set after the first Write is lost forever
//	GET /big            -> >2KB: switches to chunked transfer encoding
//	GET /nocontent      -> 204 (no body allowed) + a superfluous WriteHeader
//	GET /stream         -> live ticks via Flush: watch them arrive one by one
//	GET /boom           -> a panicking handler: connection dies, server LIVES
//	GET /abort          -> panic(http.ErrAbortHandler): same bail-out, no log
//	GET /slow           -> r.Context(): work stops when the client hangs up
//	GET /greeter/you    -> a handler that is a STRUCT with config
//	GET /farewell       -> a handler that is a CLOSURE over config
//	GET /handlerfunc    -> the http.HandlerFunc adapter, out loud
//
// Read top to bottom — the comments are the lesson.
package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	mux := http.NewServeMux()

	// ===================================================================
	// STEP 1 — Four ways to be a handler. All four are the same thing.
	// ===================================================================
	//
	// The interface (server.go:88):
	//
	//	type Handler interface {
	//	    ServeHTTP(ResponseWriter, *Request)
	//	}
	//
	// Go's menu of implementations:
	//
	//	1. a plain function + http.HandlerFunc        (adapter type)
	//	2. mux.HandleFunc(pattern, fn)                (the sugar that does 1 for you)
	//	3. a struct with a ServeHTTP method           (handlers with state/config)
	//	4. a closure returned by a factory function    (handlers with private config)
	//
	// The tour registers one route per style; they differ in how you WRITE
	// them, not in what they ARE.

	// Way 2 — the everyday one: mux.HandleFunc wraps your function.
	mux.HandleFunc("GET /small", smallHandler)
	mux.HandleFunc("GET /html", htmlHandler)
	mux.HandleFunc("GET /typed", typedHandler)
	mux.HandleFunc("GET /late", lateHandler)
	mux.HandleFunc("GET /big", bigHandler)
	mux.HandleFunc("GET /nocontent", noContentHandler)
	mux.HandleFunc("GET /stream", streamHandler)
	mux.HandleFunc("GET /boom", boomHandler)
	mux.HandleFunc("GET /abort", abortHandler)
	mux.HandleFunc("GET /slow", slowHandler)

	// Way 1 — the same conversion, made visible once: http.HandlerFunc is a
	// named type whose underlying type IS the function signature, and whose
	// only method is the one-method adapter. Its ENTIRE implementation
	// (server.go:2282) is:
	//
	//	func (f HandlerFunc) ServeHTTP(w ResponseWriter, r *Request) {
	//	    f(w, r)
	//	}
	//
	// That 3-line type is why functions can be handlers: type conversion is
	// free, and methods on named function types are legal Go.
	mux.Handle("GET /handlerfunc", http.HandlerFunc(handlerfuncHandler))

	// Way 3 — a struct with a ServeHTTP METHOD. This is how you make a
	// handler with CONFIGURATION or STATE, and it's how every "handler with
	// dependencies" in real code looks (a database, a logger, a client).
	//
	// Concurrency note, inherited from the server ("a new service goroutine
	// for each request" — conn.serve): many requests can run this method at
	// once. `greeting` is READ-ONLY config, and read-only data is safe to
	// share without locks — no atomic needed, unlike the mux lesson's
	// counter. Decide per field: read-only config? or mutated state?
	mux.Handle("GET /greeter/{name}", greeter{greeting: "Hello"})

	// Way 4 — a factory returning a CLOSURE. The returned HandlerFunc
	// (plain function → Handler, Way 1 again) carries its configuration in
	// captured variables. Same power as Way 3, less ceremony; struct wins
	// when config grows or needs methods.
	mux.Handle("GET /farewell", makeFarewell("Goodbye"))

	// ===================================================================
	// STEP 2 — Middleware, again, because it is the handler's superpower:
	// any handler can be replaced by a WRAPPED handler. The logRequests
	// below is a Handler that wraps a Handler: the composition that makes
	// FileServer, ServeMux, StripPrefix and your own middlewares all the
	// same shape (see webdev/handlers/middleware/ for the full toolbox).
	// ===================================================================
	handler := logRequests(mux)

	server := &http.Server{
		Addr:              ":8095",
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("handlers tour listening on http://localhost:8095  (Ctrl+C stops it)")
	log.Fatal(server.ListenAndServe())
}

// ----------------------------------------------------------------------
// STEP 1 handlers: the ResponseWriter contract, demonstrated live.
// ----------------------------------------------------------------------

// smallHandler is the minimal honest handler: write bytes, return.
//
// Three headers you did NOT set appear on the wire:
//
//	HTTP/1.1 200 OK            — implicit: first Write triggers
//	                             WriteHeader(StatusOK) (server.go:1639)
//	Content-Type: text/plain;  — SNIFFED from the first 512 bytes
//	  charset=utf-8              (http.DetectContentType)
//	Content-Length: 17          — computed for free: the whole body fits in
//	                             the 2048-byte chunking buffer, and no
//	                             Flush() was called, so the server buffers,
//	                             measures, and stamps the exact length
//
// Check for yourself:  curl -D - http://localhost:8095/small
func smallHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "a small response")
}

// htmlHandler sets no Content-Type ON PURPOSE: the sniffer reads the first
// 512 bytes, sees "<html>", and answers "text/html; charset=utf-8". Browsers
// will render this as a page. Sniffing is the same algorithm browsers use
// on files, running in reverse; it only fires when you stay silent.
func htmlHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "<html><body><h1>sniffed as HTML</h1></body></html>")
}

// typedHandler shows the other side: an EXPLICIT Content-Type is never
// questioned. Below, the body is plain text but the header claims JSON —
// and the server obeys, lying to the client with total sincerity. The
// lesson: Content-Type is a PROMISE you make, not a fact the server checks.
func typedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"truthiness": "the header wins, always"}`)
}

// lateHandler demonstrates the #1 ResponseWriter rule: HEADERS FIRST.
//
// The header block is finalized when the status is written — and the
// status is written at the FIRST Write (implicitly 200). Internally, the
// exact moment is in WriteHeader: your Header() map is CLONED into the
// chunk writer (server.go:1183-1185). After that, mutating your map edits
// the copy nobody will ever send.
//
// Try it:  curl -D - http://localhost:8095/late
// X-Before: shipped   <- set before the first Write: travels
// (X-After is absent) <- set after: cloned out of existence
//
// (The ONLY things you may usefully set after the body starts are
// HTTP TRAILERS — declared via the "Trailer" header or the
// http.TrailerPrefix convention. See the ResponseWriter doc.)
func lateHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Before", "shipped")

	fmt.Fprint(w, "first bytes") // implicit WriteHeader(200) happens here

	w.Header().Set("X-After", "lost forever") // mutates the dead original map
	fmt.Fprint(w, " — more bytes")
}

// bigHandler crosses the 2048-byte threshold (bufferBeforeChunkingSize,
// server.go:341). A body that cannot be fully buffered before the handler
// returns can't get a Content-Length, so HTTP/1.1 switches to
// "Transfer-Encoding: chunked": each buffered block travels wrapped in its
// own length prefix. Watch it:  curl -s -D - -o /dev/null :8095/big
//
// The framing decision is made for you, per response, by one question:
// "did the whole body fit in the 2KB buffer before we had to flush?"
func bigHandler(w http.ResponseWriter, r *http.Request) {
	for i := 0; i < 32; i++ {
		fmt.Fprintln(w, strings.Repeat("~", 255)) // 32 lines x 256 bytes = 8KB
	}
}

// noContentHandler stacks two ResponseWriter edge rules on one route:
//
//   - 204 No Content means exactly that: NO body is allowed, ever. Any
//     Write is refused with ErrBodyNotAllowed —
//     "http: request method or response status code does not allow body"
//     (server.go:43). The same refusal covers 1xx and 304. (HEAD is the
//     other discarding case, but there writes are COUNTED and dropped, so
//     Content-Length still describes the body you would have sent.)
//   - a second WriteHeader is IGNORED by the server, and a warning is
//     logged naming the caller it saw — which, because our logging
//     middleware wraps the writer, is the WRAPPER's method, not the
//     handler. Observed on this machine:
//     "http: superfluous response.WriteHeader call from
//     main.(*statusRecorder).WriteHeader (main.go:NNN)"
//     One more subtlety this route exposes: the statusRecorder records
//     the second, ignored status code — so the middleware log prints
//     "GET /nocontent -> 500" while the client actually received 204.
//     The recorder reports what you ATTEMPTED; only the wire is truth.
func noContentHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)           // 204: the status IS the answer
	w.WriteHeader(http.StatusInternalServerError) // superfluous: logged, ignored

	// Errors from Write are a return value, not a panic: read them.
	if _, err := fmt.Fprint(w, "this body is not allowed"); err != nil {
		log.Printf("write after 204 refused: %v", err)
	}
}

// ----------------------------------------------------------------------
// STEP 3 — Optional interfaces: streaming past the buffer.
// ----------------------------------------------------------------------

// streamHandler delivers bytes as they are produced, instead of letting
// them wait in the 2KB buffer for the handler to return. That matters for
// progress updates, Server-Sent Events, and anything "live".
//
// Flushing is an OPTIONAL ResponseWriter ability (interface Flusher), and
// wrappers like our statusRecorder can accidentally hide it. Go 1.22's
// answer is http.ResponseController: it walks the wrapper chain via the
// Unwrap() convention (each wrapper exposes what it wraps) and finds the
// real Flush. Our statusRecorder implements Unwrap below — without it,
// ctl.Flush() would return http.ErrNotSupported and this route would 500.
//
// See it live:  curl -N http://localhost:8095/stream   (-N = no buffering)
// Ticks arrive one at a time; after the first flush the response is
// chunked and can never grow a Content-Length.
func streamHandler(w http.ResponseWriter, r *http.Request) {
	ctl := http.NewResponseController(w)

	for i := 1; i <= 6; i++ {
		fmt.Fprintf(w, "tick %d at %s\n", i, time.Now().Format("15:04:05.000"))
		if err := ctl.Flush(); err != nil {
			// A wrapper in the chain hid the ability: surface it, don't hang.
			http.Error(w, "streaming not supported by this ResponseWriter",
				http.StatusInternalServerError)
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// ----------------------------------------------------------------------
// STEP 4 — Panics and the contract's escape hatch.
// ----------------------------------------------------------------------

// boomHandler panics on purpose. What happens next is a SERVER guarantee,
// documented on Handler itself: the panic is recovered at the connection
// level (conn.serve, server.go:1904), a stack trace goes to the server log
// ("http: panic serving 127.0.0.1:NNN: the handler exploded"), and THIS
// connection is closed — the client sees an empty reply, not a 500.
//
// The key word is "isolated": the panic was in the active request; the
// server, other connections, other goroutines continue. Try /boom, then
// load /small right after: the server never even blinked.
//
// The un-recovered internet: in production you usually want a recover
// MIDDLEWARE instead (see ./middleware), which turns panics into clean
// 500s WITH a response. Both layers can coexist: middleware for the
// panics you can catch, conn.serve for the ones you missed.
func boomHandler(w http.ResponseWriter, r *http.Request) {
	panic("the handler exploded")
}

// abortHandler uses the OTHER panic: the sentinel http.ErrAbortHandler.
// Same effect as boomHandler (connection dropped, work over), but the
// server SILENTLY swallows it — no log, no stack trace. It exists for the
// legitimate bail-out: "I know this response is unsalvageable; kill the
// connection, and no, nothing went wrong worth logging." Streaming huge
// files that the client canceled is the classic use.
//
// Compare the server log after /boom vs /abort: one is noisy, one isn't.
func abortHandler(w http.ResponseWriter, r *http.Request) {
	panic(http.ErrAbortHandler)
}

// ----------------------------------------------------------------------
// STEP 5 — The request context: the client can hang up on you.
// ----------------------------------------------------------------------

// slowHandler does "5 seconds of work" — but the right way: it listens to
// r.Context() while it works. That context is created per request by the
// server (readRequest, server.go:1040) and canceled when the client goes
// away (or when the handler returns — whichever first).
//
// Why it matters: a plain time.Sleep would wake the goroutine after the
// client left, and its writes would go nowhere — you'd pay for 5 seconds
// of CPU/IO that nobody wanted. The select below turns "client left" into
// "stop immediately". Run the lab:
//
//	curl --max-time 1 http://localhost:8095/slow    # client bails at ~1s
//	-> server log: "/slow: client went away after ~1s — aborting remaining work"
func slowHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	select {
	case <-time.After(5 * time.Second):
		fmt.Fprintln(w, "finished the full 5 seconds of work")
	case <-r.Context().Done():
		// Context canceled: the reason (why) is in ctx.Err().
		log.Printf("/slow: client went away after %v — aborting remaining work",
			time.Since(start).Round(time.Millisecond))
	}
}

// ----------------------------------------------------------------------
// STEP 1 continued: the other two handler shapes.
// ----------------------------------------------------------------------

// handlerfuncHandler exists to be wrapped by hand in main (Way 1): a named
// function registered through mux.Handle via http.HandlerFunc.
func handlerfuncHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "a plain function, promoted to Handler by http.HandlerFunc")
}

// greeter is Way 3: a handler with configuration, via a method on a
// struct. ServeHTTP reads PathValue out of the mux lesson's wildcards, so
// handlers compose with everything you've already learned.
type greeter struct {
	greeting string // read-only config: safe under goroutine-per-request
}

func (g greeter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "%s, %s!\n", g.greeting, r.PathValue("name"))
}

// makeFarewell is Way 4: a factory whose returned closure CAPTURES its
// configuration. Same abilities as the struct, zero type declarations.
func makeFarewell(word string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s, and thanks for all the requests!\n", word)
	})
}

// ----------------------------------------------------------------------
// Middleware (recap, plus the one upgrade that makes streaming work).
// ----------------------------------------------------------------------

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		log.Printf("%-6s %-16s -> %d (in %v)",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Microsecond))
	})
}

// statusRecorder: same wrap-and-observe pattern as the previous lessons —
// plus one upgrade that is the whole point of /stream above:
//
// Unwrap tells the stdlib where to find the ResponseWriter's optional
// abilities. Interfaces like Flusher (and Hijacker, and io.ReaderFrom)
// live on the ORIGINAL writer; a wrapper hides them by type assertion.
// Unwrap() is the stdlib's official ladder out:
// http.NewResponseController walks it (responsecontroller.go) until it
// finds a writer that really can Flush/Hijack. Without these three lines,
// any streaming handler behind this middleware breaks.
func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}
