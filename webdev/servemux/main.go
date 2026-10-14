// webdev/servemux — a guided tour of http.ServeMux, Go's standard router.
//
// WHAT IS A "MUX"?
//
// Short for "multiplexer". An HTTP server receives a stream of requests;
// a mux is the switchboard that looks at each one and decides which of your
// handlers should answer it, based on the request's method, host and path.
// Every serious Go web program — even tiny ones — has a mux in it, whether
// you use this one or a third-party router. The mux is where "web
// development" starts in Go: the Handler interface is the noun, the mux is
// the verb that picks one.
//
// ServeMux matches PATTERNS. A pattern looks like:
//
//	[METHOD ] [HOST]/[PATH]
//
//	"GET /health"             method + exact path (a GET pattern also matches HEAD)
//	"POST /health"            the POST twin — same path, different method
//	"GET /users/{id}"         {x} matches exactly ONE path segment, captured as "id"
//	"GET /files/{path...}"    {x...} matches the REST of the path, slashes included
//	"GET /{$}"                {$} anchors the end: this matches "/" and nothing else
//	"/static/"                trailing slash = a subtree: everything under /static/
//	"/"                       no method, catch-all path: every request
//	"api.example.com/x"       host-scoped: only requests for that Host header
//
// Patterns are validated and matched against the ESCAPED path, segment by
// segment: a %2F inside a segment stays inside the segment (then gets
// unescaped when compared). Registering an invalid or CONFLICTING pattern
// panics immediately at startup — a routing table that cannot misroute
// silently.
//
// HOW TO RUN
//
//	go run .            # from THIS directory
//
// then open http://localhost:8090 and try (or see README.md for the lab):
//
//	GET  /health           -> method-scoped route (HEAD works too, POST twin exists)
//	DELETE /health         -> 405 + Allow: GET, HEAD, POST (no pattern matched the method)
//	GET  /hello/rosen      -> single wildcard {name} captured via r.PathValue
//	GET  /hello/a/b        -> 404: a single wildcard is exactly ONE segment
//	GET  /users/admin      -> literal pattern beats the wildcard pattern (precedence)
//	GET  /users/alice      -> ...and the wildcard catches everything else
//	GET  /users/7/posts/3  -> two wildcards captured from one pattern
//	GET  /files/a/b/c.txt  -> {path...} multi wildcard: "a/b/c.txt", slashes and all
//	GET  /                 -> "GET /{$}": ONLY the bare root
//	GET  /nope             -> 404: nothing matched (no "/" catch-all registered here)
//	ANY  /ping             -> no method in the pattern = every method allowed
//	GET  /static           -> 307 redirect to /static/ (subtree canonicalization)
//	Host: api.example.com + GET /hostonly -> host-scoped pattern
//	GET  /visits           -> a Handler that is a STRUCT WITH STATE
//
// Read top to bottom — the comments are the lesson.
package main

import (
	"fmt"         // formatted printing into the response body
	"log"         // timestamped logging; log.Fatal also exits the program
	"net/http"    // ServeMux, Handler, Server: the whole cast
	"sync/atomic" // atomic.Int64: concurrency-safe counter (see visitCounter)
	"time"        // durations for the middleware timing and the server timeout
)

func main() {
	// ===================================================================
	// STEP 1 — Make a mux: http.NewServeMux, not the global one.
	// ===================================================================
	//
	// A ServeMux is a value you own: a struct holding a routing tree and
	// a lock. This one-line construction is the modern idiom. The
	// alternative is the package-global DefaultServeMux, which the
	// package-level http.Handle / http.HandleFunc write into, and which
	// http.ListenAndServe(":port", nil) reads. Globals sound convenient,
	// but any import anywhere in your program can add routes to
	// DefaultServeMux, you can't easily test around it, and two patterns
	// you never saw might conflict. A private mux has none of those
	// failure modes, so real code — and this file — uses NewServeMux.
	mux := http.NewServeMux()

	// ===================================================================
	// STEP 2 — Registration: mux.HandleFunc and mux.Handle.
	// ===================================================================
	//
	// Both take a pattern and put a handler under it. They differ only in
	// the second argument:
	//
	//	mux.HandleFunc(pattern string, h func(ResponseWriter, *Request))
	//	mux.Handle(pattern string, h Handler)
	//
	// A Handler is any type with a ServeHTTP(ResponseWriter, *Request)
	// method. HandleFunc is pure convenience: it wraps your function in
	// http.HandlerFunc, a one-method adapter type that exists to turn a
	// plain function into a Handler (the same trick STEP 8 uses with a
	// closure, and the same one the middleware at the bottom uses).
	//
	// Two facts about registration, both enforced by the source:
	//
	//   * it PANICS on invalid patterns and on conflicts (see STEP 4) —
	//     at startup, before serving, not on some request at 3am. The
	//     error even names the file:line of the earlier registration.
	//   * it is safe to call while the server runs (the tree is guarded
	//     by an RWMutex), but in practice you register everything before
	//     ListenAndServe and never touch it again.

	// ===================================================================
	// STEP 3 — Method-scoped routes, and the 405 they produce.
	// ===================================================================
	//
	// "GET /health" matches GET and HEAD (a GET pattern matches HEAD too —
	// the stdlib treats HEAD as "GET without the body", and browsers use
	// it constantly). It does NOT match POST. Below, the same path is
	// registered for POST as well: the two patterns match disjoint sets
	// of requests, so they can coexist. Try DELETE /health: no pattern
	// matches — but the PATH does. The mux then answers:
	//
	//	405 Method Not Allowed
	//	Allow: GET, HEAD, POST
	//
	// ...with the Allow header listing every method that WOULD have
	// worked (HEAD listed because GET patterns match HEAD). A 405 is the
	// mux telling your client "right place, wrong verb — here's the
	// menu". Compare with 404: wrong PLACE entirely.
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("POST /health", reportHealthHandler)

	// ===================================================================
	// STEP 4 — Wildcards: {x} captures one segment, {x...} captures rest.
	// ===================================================================
	//
	// In "GET /hello/{name}", {name} matches exactly ONE path segment —
	// the run between two literal "/" characters in the escaped URL.
	// /hello/rosen matches; /hello/a/b does not; /hello/ (empty) does
	// not. The captured text is unescaped and read with:
	//
	//	r.PathValue("name")
	//
	// which returns "" if the matched pattern has no such wildcard.
	// Wildcard names must be valid Go identifiers and unique within one
	// pattern — parse errors panic (fail fast).
	//
	// "{x...}" (always the LAST segment) matches everything remaining,
	// slashes included: /files/a/b/c.txt captures path="a/b/c.txt".
	// Under the hood, a trailing "/" is sugar for an ANONYMOUS "{x...}"
	// (that's why "/static/" is a subtree, STEP 7), and "{$}" is sugar
	// for "the path must end HERE" (STEP 6).
	mux.HandleFunc("GET /hello/{name}", helloNameHandler)
	mux.HandleFunc("GET /users/{id}/posts/{postID}", userPostHandler)
	mux.HandleFunc("GET /files/{path...}", filesHandler)

	// ===================================================================
	// STEP 5 — Precedence: the most specific pattern wins.
	// ===================================================================
	//
	// Both patterns below match a request for /users/admin. No panic:
	// they don't conflict, because one is MORE SPECIFIC than the other.
	// The rule, verbatim from the source (pattern.go):
	//
	//   "P1 is more specific than P2 if P2 matches all the requests of
	//    P1 and more" — i.e. P1 matches a strict subset.
	//
	// "GET /users/admin" (one exact path) matches a strict subset of
	// "GET /users/{name}" (any one segment), so the literal wins for
	// /users/admin and the wildcard serves everyone else. Same logic
	// ranks: literal > {x} > {x...}; deeper patterns > shallower;
	// method-scoped > methodless (for that method's requests);
	// host-scoped > hostless.
	//
	// And when two patterns OVERLAP but neither is more specific
	// ("{x}/b" vs "a/{y}", or the same path twice), registration PANICS
	// with an error naming both patterns and where each was registered.
	// That is the guarantee "can't misroute silently" is made of.
	mux.HandleFunc("GET /users/admin", usersAdminHandler)
	mux.HandleFunc("GET /users/{name}", userNameHandler)

	// ===================================================================
	// STEP 6 — {$}: the end anchor, and honest 404s.
	// ===================================================================
	//
	// "/" alone is the catch-ALL (every path). "GET /{$}" is the opposite
	// extreme: ONLY the bare root, nothing else. Registering just this
	// and not "/" means every unknown path 404s — you will SEE 404s
	// instead of everything quietly landing on a catch-all. (For a
	// contrast, webdev/fileserver registers "/" — catch-all — for its
	// FileServer.)
	mux.HandleFunc("GET /{$}", homePageHandler)

	// ===================================================================
	// STEP 7 — Subtrees, trailing slashes, and the 307s.
	// ===================================================================
	//
	// "/static/" matches EVERY path that starts with /static/. Request
	// the bare "/static" and the mux doesn't hand it to the handler —
	// it answers a 307 Temporary Redirect to "/static/". Why redirect at
	// all? So that relative URLs inside pages ("next.html") resolve
	// correctly: browsers compute them against the URL with the slash.
	// And why 307 and not the classic 301? 307 preserves the request
	// method and body — a POST would survive the trip unchanged, which
	// 301 does not guarantee.
	//
	// The mux sanitizes paths the same way: a request for "/a/../health"
	// or "//health" never reaches a handler — it's answered by a 307 to
	// the cleaned path ("/health"). This happens BEFORE matching, one
	// reason path traversal attacks die before seeing your handlers.
	mux.HandleFunc("/static/", staticZoneHandler)

	// ===================================================================
	// STEP 8 — Host patterns (and no method at all).
	// ===================================================================
	//
	// "api.example.com/hostonly" matches only requests whose Host header
	// is api.example.com. Test locally without DNS:
	//
	//	curl -H "Host: api.example.com" http://localhost:8090/hostonly
	//
	// (Any port in the Host header — "api.example.com:8090" — is
	// stripped before matching.) Host-scoped patterns are how one binary
	// serves many vhosts. Precedence exception, straight from the source:
	// a host pattern beats a hostless pattern EVEN IF they would
	// otherwise conflict — a compatibility carve-out from pre-1.22 Go.
	//
	// "/ping" below has NO method part: it matches every method. POST
	// it, DELETE it — 200 "pong" every time. The method axis only
	// restricts when you ask it to.
	mux.HandleFunc("api.example.com/hostonly", hostOnlyHandler)
	mux.HandleFunc("/ping", pingHandler)

	// ===================================================================
	// STEP 9 — Any struct with ServeHTTP is a Handler — including state.
	// ===================================================================
	//
	// mux.Handle takes a Handler value; visitCounter (below) is a small
	// STRUCT with a ServeHTTP method, so it qualifies — no function
	// needed. It carries a counter, which teaches THE fact about handler
	// concurrency: net/http serves each request in its own goroutine
	// (Serve accepts connections, "creating a new service goroutine for
	// each", server.go). Ten simultaneous requests = ten goroutines
	// inside the same method. Shared state must be concurrency-safe —
	// hence atomic.Int64. (Classic first mistake: an int and "n++" —
	// works in every test, corrupts under real traffic.)
	//
	// Reload /visits a few times and watch the count. Then hammer it:
	//	for i in $(seq 1 50); do curl -s http://localhost:8090/visits & done; wait
	mux.Handle("GET /visits", &visitCounter{})

	// ===================================================================
	// STEP 10 — Middleware: watching the mux make its decisions.
	// ===================================================================
	//
	// logRequests (bottom of the file) wraps the ENTIRE mux and prints,
	// for each request: method, path, resulting status, and — new in
	// this lesson — the PATTERN THAT MATCHED, read from r.Pattern. In
	// Go 1.26.5, r.Pattern is a plain string field on Request, set by
	// ServeMux.ServeHTTP right before it calls your handler (empty for
	// the mux's own 404s and 405s). It is the mux explaining itself:
	//
	//	GET /users/alice -> 200 via "GET /users/{name}"
	//	DELETE /health   -> 405 via ""           <- matched nothing
	//
	// Middleware itself is the same "func(Handler) Handler" shape as
	// always: the mux is just a Handler, so it wraps like any other.
	// One catch worth knowing: the middleware reads r.Pattern AFTER
	// next.ServeHTTP returns — the same Request value is mutated in
	// place by the mux while dispatching.
	handlerWithLogging := logRequests(mux)

	// ===================================================================
	// STEP 11 — The server: unchanged machinery from the fileserver lesson.
	// ===================================================================
	server := &http.Server{
		Addr:              ":8090",
		Handler:           handlerWithLogging,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("mux tour listening on http://localhost:8090  (Ctrl+C stops it)")
	log.Fatal(server.ListenAndServe())
}

// ----------------------------------------------------------------------
// Handlers. Same contract as always: (w ResponseWriter, r *Request).
// ----------------------------------------------------------------------

// healthHandler answers GET /health. HEAD requests land here too: a GET
// pattern matches HEAD, and net/http suppresses the body automatically.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "healthy. you sent %s, matched pattern %q\n", r.Method, r.Pattern)
}

// reportHealthHandler answers POST /health. Registered on the SAME path
// as healthHandler but a different method — the two patterns match
// disjoint request sets, so the mux stores them side by side without
// complaint. The DELETE you send at the lab proves the partition works.
func reportHealthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "report accepted (matched %q)\n", r.Pattern)
}

// helloNameHandler answers GET /hello/{name}: one segment, captured.
func helloNameHandler(w http.ResponseWriter, r *http.Request) {
	// PathValue unescapes for free: /hello/rosen%20jr arrives as "rosen jr".
	fmt.Fprintf(w, "hello, %q! (wildcard {name} of pattern %q)\n",
		r.PathValue("name"), r.Pattern)
}

// userPostHandler answers GET /users/{id}/posts/{postID}: literals and
// wildcards can be interleaved; each wildcard has its own name.
func userPostHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "user %s, post %s (patterns %q)\n",
		r.PathValue("id"), r.PathValue("postID"), r.Pattern)
}

// filesHandler answers GET /files/{path...}: the multi wildcard.
func filesHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "the whole rest of the path is %q (pattern %q)\n",
		r.PathValue("path"), r.Pattern)
}

// usersAdminHandler answers GET /users/admin — the literal pattern that
// beats the wildcard pattern below for this one exact path.
func usersAdminHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "you are THE admin: literal %q beat the wildcard\n", r.Pattern)
}

// userNameHandler answers GET /users/{name} — everything /users/X that
// no more specific literal claims.
func userNameHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "hello user %q, served by wildcard pattern %q\n",
		r.PathValue("name"), r.Pattern)
}

// homePageHandler answers GET /{$} — ONLY the bare "/".
func homePageHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "the root, and only the root (pattern %q)\n", r.Pattern)
}

// staticZoneHandler answers the "/static/" subtree. Note it receives
// the request with its ORIGINAL path — the subtree pattern swallowed
// /static/<anything> in one bite.
func staticZoneHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "inside the subtree; path I see: %s (pattern %q)\n", r.URL.Path, r.Pattern)
}

// hostOnlyHandler answers the host-scoped pattern
// "api.example.com/hostonly": reachable only with that Host header.
func hostOnlyHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "you reached a HOST-scoped pattern; your Host: %s\n", r.Host)
}

// pingHandler answers "/ping" with no method — every verb is welcome.
func pingHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "pong (%s works here — the pattern names no method)\n", r.Method)
}

// ----------------------------------------------------------------------
// A Handler that is a struct with STATE (STEP 9).
// ----------------------------------------------------------------------

// visitCounter counts requests. net/http runs each request in its own
// goroutine, so ServeHTTP may execute concurrently on many CPUs at once:
// the counter MUST be concurrency-safe, or requests will race and lose
// increments. atomic.Int64 is the smallest correct choice.
type visitCounter struct {
	n atomic.Int64
}

// ServeHTTP satisfies the Handler interface — with a POINTER receiver,
// so the counter is shared by everyone who holds the same *visitCounter
// value (the one registered in main).
func (v *visitCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "visit number %d (pattern %q)\n", v.n.Add(1), r.Pattern)
}

// ----------------------------------------------------------------------
// Middleware (STEP 10): the mux narrated, one line per request.
// ----------------------------------------------------------------------

// logRequests prints the method, path, status — and the PATTERN the mux
// chose, from r.Pattern. Empty pattern means the mux itself produced the
// answer (a 404, a 405, a sanitizing redirect): no handler matched.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r) // mux runs, matching happens, r.Pattern gets set

		pattern := r.Pattern
		if pattern == "" {
			pattern = "(no pattern — the mux answered itself)"
		}
		log.Printf("%-6s %-24s -> %d via %-20q (in %v)",
			r.Method, r.URL.Path, rec.status, pattern,
			time.Since(start).Round(time.Microsecond))
	})
}

// statusRecorder is the same "wrap and observe" ResponseWriter trick as
// the fileserver lesson: embed the interface (every method delegates),
// override the one that reports the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}
