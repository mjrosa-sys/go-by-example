// webdev/handlers/middleware — handlers composing handlers: the real-world
// chain, built from the same one-method interface.
//
// # THE BIG IDEA
//
// Middleware is nothing new: it is an ordinary Handler whose body happens
// to call another Handler. "func(Handler) Handler" is the entire
// middleware ecosystem of Go. The stdlib itself ships middleware —
// http.TimeoutHandler, http.MaxBytesHandler, http.StripPrefix — because
// wrapping handlers is just... handlers.
//
// This example assembles the chain a small production service would
// actually use, and points out the two stdlib wrappers you should reach
// for before writing your own:
//
//	logRequests              outermost: sees the true status of everything
//	  attachRequestID          context.WithValue: per-request data, done right
//	    recoverPanics          panics become clean 500s (connection survives)
//	      requireAuth             guards /private/* with a bearer token
//	        mux                  the router
//	          TimeoutHandler        stdlib: hard 2s ceiling on slow work
//	          MaxBytesHandler       stdlib: 4KB ceiling on request bodies
//	          handlers             business logic
//
// HOW TO RUN
//
//	go run .          # from THIS directory
//
// then try (full lab in ../README.md):
//
//	# public: no auth needed; note the X-Request-ID on every response
//	curl -D - http://localhost:8096/public/health
//
//	# private: 401 without the token, 200 with it
//	curl http://localhost:8096/private/report
//	curl -H "Authorization: Bearer opensesame" http://localhost:8096/private/report
//
//	# TimeoutHandler: handler sleeps 3s, ceiling is 2s -> 503 after 2s
//	curl -w "\ntook %{time_total}s\n" -H "Authorization: Bearer opensesame" \
//	     http://localhost:8096/private/slow
//
//	# MaxBytesHandler: echo up to 4KB, 413 beyond
//	head -c 2048 /dev/zero | curl -H "Authorization: Bearer opensesame" \
//	     --data-binary @- http://localhost:8096/private/echo
//	head -c 8192 /dev/zero | curl -H "Authorization: Bearer opensesame" \
//	     --data-binary @- http://localhost:8096/private/echo
//
//	# panic BELOW the chain: recovered into a clean 500 (server lives)
//	curl -H "Authorization: Bearer opensesame" http://localhost:8096/private/panic
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// The token a client must present for /private/* routes. A constant in
// code is the demo version of "a secret in a config store".
const authToken = "opensesame"

// ctxKeyRequestID is the CONTEXT KEY type. Always use a private struct
// type as a context key: string keys collide between packages, struct
// types cannot. The empty struct occupies zero bytes.
type ctxKeyRequestID struct{}

func main() {
	mux := http.NewServeMux()

	// Ordinary routes: the mux lesson, unchanged.
	mux.HandleFunc("GET /public/health", healthHandler)
	mux.HandleFunc("GET /private/report", reportHandler)
	mux.HandleFunc("GET /private/flush", flushHandler)
	mux.HandleFunc("GET /private/panic", panicHandler)

	// Stdlib middleware #1 — http.TimeoutHandler (server.go:3787): runs the
	// wrapped handler under a wall-clock budget. slowReportHandler sleeps
	// 3 seconds and IGNORES r.Context() — which is precisely the kind of
	// handler you cannot fix, and the reason this wrapper exists. At the
	// 2s deadline the client gets 503 + our message, and any further
	// writes by the inner handler return ErrHandlerTimeout.
	//
	// Note from the docs: TimeoutHandler does NOT support Hijacker or
	// Flusher — don't wrap streaming routes with it.
	//
	// Also note the http.HandlerFunc(...) conversion: unlike mux.HandleFunc,
	// which adapts functions for you, this wrapper demands an actual
	// Handler value — the compiler refuses a bare function. That's "Way 1"
	// from the main example, enforced by the type system.
	mux.Handle("GET /private/slow",
		http.TimeoutHandler(http.HandlerFunc(slowReportHandler), 2*time.Second, "the report is taking too long"))

	// Stdlib middleware #2 — http.MaxBytesHandler: wraps r.Body in a
	// MaxBytesReader with a 4KB ceiling. Readers that cross it get a
	// *http.MaxBytesError — a TYPED error, matched with errors.As below.
	// Bonus fact from the source: on overflow, MaxBytesReader calls back
	// into the response (requestTooLarge) to force closeAfterReply, so a
	// too-large body can never hijack keep-alive. The 413 ANSWER is the
	// inner handler's job; the connection hygiene is the server's.
	mux.Handle("POST /private/echo", http.MaxBytesHandler(http.HandlerFunc(echoHandler), 4<<10))

	// ------------------------------------------------------------------
	// The chain, assembled bottom-up so each layer wraps the next.
	// Order is POLICY — trace it before you change it:
	//   * recoverPanics sits INSIDE logging: panics become 500s that the
	//     logger can see (and the response is written before logging ends).
	//   * recoverPanics sits OUTSIDE auth: a panic IN the auth middleware
	//     is also recovered, not just panics in handlers.
	//   * attachRequestID sits outside recover: every 500 in the logs
	//     carries the same id the client got in X-Request-ID.
	//   * logRequests is outermost: it observes the true status of every
	//     decision below, including 401s.
	// ------------------------------------------------------------------
	var handler http.Handler = mux
	handler = recoverPanics(handler)
	handler = requireAuth(handler)
	handler = attachRequestID(handler)
	handler = logRequests(handler)

	server := &http.Server{
		Addr:              ":8096",
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("middleware chain listening on http://localhost:8096 — try /private/report with and without the token")
	log.Fatal(server.ListenAndServe())
}

// ----------------------------------------------------------------------
// Route handlers: the business end of the chain.
// ----------------------------------------------------------------------

// healthHandler proves the requestID middleware ran: it reads the id back
// out of the context that middleware installed, and echoes it.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(ctxKeyRequestID{}).(string)
	fmt.Fprintf(w, "healthy. your request id is %s (delivered via context)\n", id)
}

// reportHandler is the "business logic" behind auth.
func reportHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "the secret report: everything is fine.")
}

// slowReportHandler deliberately misbehaves: 3 seconds of work, no select
// on r.Context(). A well-behaved handler would use the pattern from the
// /slow route in the main example — this one is here to be WRAPPED, by
// code you don't control.
func slowReportHandler(w http.ResponseWriter, r *http.Request) {
	time.Sleep(3 * time.Second)
	fmt.Fprintln(w, "finally! the report took 3 seconds")
}

// echoHandler reads the (already MaxBytes-limited) body and answers with
// its size. When the limit trips, io.ReadAll returns *http.MaxBytesError:
// a typed sentinel — match it with errors.As, and answer 413 yourself.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large: "+tooLarge.Error(),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "reading body failed", http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, "received %d bytes — thanks!\n", len(data))
}

// flushHandler streams through the whole chain — which only works because
// statusRecorder implements Unwrap (see the main example's /stream). The
// ResponseController walk is exactly the stdlib's answer to
// "middleware hides optional abilities".
func flushHandler(w http.ResponseWriter, r *http.Request) {
	ctl := http.NewResponseController(w)
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(w, "flushed line %d at %s\n", i, time.Now().Format("15:04:05.000"))
		if err := ctl.Flush(); err != nil {
			http.Error(w, "streaming unsupported through this chain", http.StatusInternalServerError)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// panicHandler simulates a production bug: a panic INSIDE business logic.
// Compare with the /boom route of the main example (no recover there:
// the connection dies): here recoverPanics writes a proper 500 response,
// and the client (and keep-alive!) survive.
func panicHandler(w http.ResponseWriter, r *http.Request) {
	panic("database on fire")
}

// ----------------------------------------------------------------------
// The middleware layers, top-down in assembly order.
// ----------------------------------------------------------------------

// logRequests: the same wrap-and-observe pattern from previous lessons.
// Outermost, so 401s and 500s from the layers below appear here.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		log.Printf("%-6s %-18s -> %d (in %v)",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond))
	})
}

// attachRequestID gives each request a random id: put it in the RESPONSE
// (X-Request-ID) so clients can quote it, and into the CONTEXT so handlers
// deep below can read it without a parameter threading through every
// signature. That is context.WithValue's actual job description: request-
// scoped data crossing middleware boundaries. (For anything else — config,
// caches — use explicit struct fields, not context.)
//
// Note the plumbing detail: requests are not to be mutated (the Handler
// doc says so), so we build r2 := r.WithContext(...), a shallow copy
// sharing everything else — the sanctioned way, same idea as the mux
// lesson's StripPrefix clone.
func attachRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			log.Printf("requestID: cannot generate id: %v", err)
			http.Error(w, "cannot generate request id", http.StatusInternalServerError)
			return
		}
		id := hex.EncodeToString(b[:])

		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, id))

		next.ServeHTTP(w, r)
	})
}

// requireAuth guards every path under /private/: no "Authorization:
// Bearer opensesame" header, no entry. Two lessons in one:
//
//   - middleware can scope ITSELF (the prefix check) — alternative:
//     build two muxes and wrap only the private one;
//   - 401 is the mux lesson's "method mismatch" concept, one axis over:
//     it's not the verb, it's the viewer, that's wrong.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/private/") {
			next.ServeHTTP(w, r) // public: pass through untouched
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+authToken {
			// http.Error: status + text/plain + X-Content-Type-Options:
			// nosniff, in one call. You've seen its output three lessons ago.
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanics is the layer the main example's /boom route was missing.
// The stdlib guarantees a panic can't kill the server, but WITHOUT this
// layer the client gets a dropped connection; WITH it, the client gets a
// real 500 response on a connection that stays alive.
//
// Details worth copying:
//   - defer + recover in a closure — the only recover idiom;
//   - ErrAbortHandler is re-panicked, not swallowed: that sentinel exists
//     for silent bail-outs (main example's /abort) — respect the signal;
//   - writing the 500 is only safe if NOTHING has been written yet. A
//     handler that panicked mid-body gets its connection closed instead —
//     which is fine: half-finished responses are worse than none.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p) // the caller asked for silence: forward, don't handle
				}
				log.Printf("recovered panic: %v\n%s", p, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ----------------------------------------------------------------------
// The wrapped writer, with the Unwrap upgrade from the main example.
// ----------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

// Unwrap is what keeps this recorder from breaking /private/flush:
// http.NewResponseController climbs this method to reach the real
// writer's Flush ability. Without it, any streaming route behind this
// middleware would get http.ErrNotSupported from every Flush call.
func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}
