// webdev/servemux/mount — sub-routers: nesting one mux inside another.
//
// THE ONE FACT THIS EXAMPLE IS BUILT ON:
//
// A *ServeMux is itself an http.Handler — it has a ServeHTTP method. So
// a mux can be REGISTERED INSIDE ANOTHER MUX like any handler value.
// That single sentence is the door to real-world routing architecture:
// per-package routers ("the API mux", "the admin mux"), API versioning,
// and mountable "sub-applications", all with zero extra dependencies.
//
// The pair of tools is always the same:
//
//	mux.Handle("/v1/", http.StripPrefix("/v1", apiV1))
//	           ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
//	           "/v1/" scopes WHO gets handed in;
//	           StripPrefix removes "/v1" so the inner mux sees clean paths.
//
// HOW TO RUN
//
//	go run .          # from THIS directory
//
// then try http://localhost:8091:
//
//	GET /              -> outer mux's landing page ("GET /{$}")
//	GET /v1            -> 307 redirect to /v1/ (outer mux canonicalizing)
//	GET /v1/           -> the INNER mux's root ("GET /{$}" after stripping)
//	GET /v1/health     -> inner mux, inner PathValue works
//	GET /v1/users/7    -> inner wildcard: id = 7
//	GET /v1/files/a/b   -> inner multi wildcard: path = "a/b"
//	DELETE /v1/health  -> 405 + Allow — produced by the INNER mux
//	GET /v1/nope       -> 404 — produced by the INNER mux
//
// Watch the log while you do this: the middleware prints the pattern that
// matched — and the pattern it prints is the OUTER one ("/v1/"), while the
// handler inside prints r.Pattern as the INNER one. Both are right:
// StripPrefix clones the Request before handing it down, so the inner mux
// fills in ITS pattern on the clone. One request, two truths, no lies.
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	// ------------------------------------------------------------------
	// The INNER mux: a self-contained little API, written as if /v1
	// did not exist. Its patterns never mention the version — which is
	// exactly the point: the inner package can be routed, tested
	// (httptest + this mux) and moved without touching the mount.
	// ------------------------------------------------------------------
	apiV1 := http.NewServeMux()
	apiV1.HandleFunc("GET /{$}", v1RootHandler) // serves "/v1/" after the strip
	apiV1.HandleFunc("GET /health", v1HealthHandler)
	apiV1.HandleFunc("GET /users/{id}", v1UserHandler)
	apiV1.HandleFunc("GET /files/{path...}", v1FilesHandler)

	// ------------------------------------------------------------------
	// The OUTER mux: the program's real entry point. It owns the top
	// level and DECIDES who gets which subtree.
	// ------------------------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", landingHandler)

	// The mount, anatomy of one line:
	//
	//	mux.Handle("/v1/", http.StripPrefix("/v1", apiV1))
	//
	//   * Pattern "/v1/" (trailing slash!) is a subtree: it matches
	//     /v1, /v1/, /v1/anything — and NOTHING else. A request for
	//     "/v1zzz" does not match it (first segment would be "v1zzz"),
	//     so it 404s at the OUTER mux and never reaches StripPrefix.
	//     If we had registered "/" instead, "/v1zzz" WOULD reach
	//     StripPrefix, which would happily strip "/v1" off it and hand
	//     "zzz" to the inner mux. The pattern is the scope; the strip is
	//     the rewrite. You need BOTH, and they must agree.
	//   * http.StripPrefix("/v1", h) removes "/v1" from the URL path of
	//     the request it passes to h (and 404s itself if the path does
	//     not start with the prefix). After the strip, "/v1/users/7"
	//     looks like plain "/users/7" to the inner mux.
	//   * apiV1, a *ServeMux, is the Handler being handed in. Interface
	//     satisfaction in action: the outer mux cannot tell — and does
	//     not care — whether this value is a file server, a function, or
	//     another mux.
	mux.Handle("/v1/", http.StripPrefix("/v1", apiV1))

	// Cross-cutting concerns attach at whichever level you want them:
	// wrapping the OUTER mux applies one middleware to EVERYTHING —
	// outer routes, mounted routes, all of it. Wrapping only apiV1 would
	// scope it to the v1 API. (Here: same logging middleware as the main
	// example, plus the read-header timeout from previous lessons.)
	server := &http.Server{
		Addr:              ":8091",
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("mount example listening on http://localhost:8091 — try /v1/users/7")
	log.Fatal(server.ListenAndServe())
}

// ----------------------------------------------------------------------
// Inner handlers: they think they run at the top level. PathValue,
// r.Pattern, 404s and 405s all behave as if no /v1 existed.
// ----------------------------------------------------------------------

// v1RootHandler answers the inner "GET /{$}" — reached via GET /v1/,
// after the outer mux redirected the bare /v1 with a 307 and StripPrefix
// turned "/v1/" into "/".
func v1RootHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "API v1 root. The handler matched %q — the INNER pattern.\n", r.Pattern)
}

// v1HealthHandler answers the inner GET /health.
func v1HealthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "v1 health OK (matched %q)\n", r.Pattern)
}

// v1UserHandler answers the inner GET /users/{id}; PathValue works
// normally inside a mounted mux.
func v1UserHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "user %s (wildcard of inner pattern %q)\n",
		r.PathValue("id"), r.Pattern)
}

// v1FilesHandler answers the inner multi wildcard GET /files/{path...}.
func v1FilesHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "v1 files: rest of path is %q (inner pattern %q)\n",
		r.PathValue("path"), r.Pattern)
}

// landingHandler is the OUTER mux's own page — proof that the outer mux
// can hold normal routes next to mounted ones.
func landingHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "outer mux landing page (outer pattern %q)\n", r.Pattern)
}

// ----------------------------------------------------------------------
// Same middleware as the main example, kept so this folder is
// self-contained. It logs the pattern of the OUTER mux; note how it
// DIFFERS from what the inner handlers print for the same request —
// StripPrefix cloned the Request, and the inner mux set its own pattern
// on the clone, not on this one.
// ----------------------------------------------------------------------

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		pattern := r.Pattern
		if pattern == "" {
			pattern = "(no pattern — a mux answered itself)"
		}
		log.Printf("%-6s %-24s -> %d via %-16q (in %v)",
			r.Method, r.URL.Path, rec.status, pattern,
			time.Since(start).Round(time.Microsecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}
