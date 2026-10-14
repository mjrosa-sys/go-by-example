// webdev/fileserver — a guided tour of Go's built-in file server.
//
// WHAT IS A "FILE SERVER"?
//
// An HTTP server that answers requests with the contents of files: the URL
// path of each request is used as a path INSIDE a directory that you pick,
// the file found there is opened, and its bytes are copied to the HTTP
// response. Go ships a production-grade file server in the standard library
// (package net/http), so a real one really is a few lines of code — this
// file only looks long because of the comments, which ARE the lesson.
//
// # HOW TO RUN
//
// Run it from THIS directory (paths like "./public" below are relative to
// the directory you start the program from — see the note on http.Dir):
//
//	go run .
//
// then open http://localhost:8080 and try the URLs in the list below.
// Every one of them teaches a different characteristic of the file server:
//
//	/                       -> public/index.html            (http.FileServer)
//	/docs/                  -> auto-generated HTML listing  (dir has no index.html)
//	/docs                   -> 301 redirect to /docs/         (canonical form)
//	/docs/deep/nested.txt   -> a file inside nested directories
//	/static/notes.txt       -> same tree mounted under /static/ (http.StripPrefix)
//	/hello/rosen            -> a plain Go handler, NOT a file (wildcard pattern)
//	/download               -> notes.txt as a forced download (http.ServeFile)
//	/files/notes.txt        -> one file chosen by URL, with input validation
//	/missing.txt            -> 404 page not found
//
// Read the file top to bottom. Then break it on purpose and see what happens.
package main

import (
	"fmt"      // formatted printing: fmt.Fprintf writes directly into the HTTP response
	"log"      // timestamped logging to stderr; log.Fatal prints and exits the program
	"net/http" // the HTTP server, the router (ServeMux) and the file server live here
	"strings"  // string helpers, used for input validation in serveByName
	"time"     // time.Duration, used for the middleware timing and the server timeout
)

func main() {
	// ===================================================================
	// STEP 1 — The router: an *http.ServeMux.
	// ===================================================================
	//
	// A ServeMux ("mux" = multiplexer, think "request switchboard") looks
	// at each incoming request and decides WHICH handler should answer
	// it. You teach it by registering ("pattern -> handler") pairs.
	//
	// http.NewServeMux() creates a private mux, which is what you want:
	// handlers must be registered before the server starts, and a mux is
	// a plain value you fully control. (The package-level http.Handle /
	// http.HandleFunc functions register into a GLOBAL mux,
	// http.DefaultServeMux — convenient for 5-line demos, riskier in
	// larger programs because any import can silently add routes to it.)
	//
	// Pattern syntax (since Go 1.22), from most to least specific:
	//
	//	"GET /hello/{name}"   method + literal prefix + a wildcard segment;
	//	                      {name} captures ONE path segment into r.PathValue("name")
	//	"/static/{path...}"   a wildcard that swallows the REST of the path, slashes included
	//	"/{$}"                matches ONLY the exact path "/"  ({$} = end anchor)
	//	"/static/"            everything that starts with "/static/" (a "subtree")
	//	"/"                   everything — the catch-all; whoever registers the
	//	                      MOST SPECIFIC matching pattern wins, and registering
	//	                      two patterns that overlap ambiguously PANICS at startup
	//	                      (a routing table that can't misroute silently)
	//
	// Bonus job of the mux: it SANITIZES paths before matching. A request
	// for "/docs/../main.go" never reaches any handler — the mux replies
	// with a redirect to the cleaned path "/main.go". Same for repeated
	// slashes ("//"). Note the subtlety: %2E (an escaped ".") is preserved
	// and is NOT treated as a separator.
	mux := http.NewServeMux()

	// ===================================================================
	// STEP 2 — The star of this example: http.FileServer + http.Dir.
	// ===================================================================
	//
	//   http.Dir("./public")       -> the directory "./public" turned into
	//                                  an http.FileSystem (a "rooted" view
	//                                  of part of the OS file system)
	//   http.FileServer(root)       -> a Handler that serves files from it
	//   mux.Handle("/", fileServer) -> "for anything starting with /, ask
	//                                  the file server"
	//
	// A Handler is anything implementing this one-method interface (the
	// heart of ALL of net/http — servers, routers, middleware, files):
	//
	//	type Handler interface {
	//	    ServeHTTP(ResponseWriter, *Request)
	//	}
	//
	// so "the file server" is not magic: it is a regular value of a
	// regular type that happens to implement Handler, and like any value
	// it can be stored in a variable and registered in MANY routes:
	fileServer := http.FileServer(http.Dir("./public"))
	mux.Handle("/", fileServer)

	// Inside one call, http.Dir("./public") is doing three things:
	//
	//   1. It says WHERE files come from. Its type is literally
	//      `type Dir string` — a new type over plain string, so
	//      http.Dir("./public") is a free conversion, no allocation.
	//   2. It makes the path RELATIVE to the current working directory
	//      (CWD), i.e. the folder you ran `go run .` from. Run this
	//      program from the repo root and every request will 404. An
	//      absolute path (http.Dir("/var/www")) has no such ambiguity.
	//   3. It establishes a SECURITY BOUNDARY: every request path gets
	//      mapped INSIDE ./public. In Go 1.26 its Open method computes
	//      path.Clean("/" + name)[1:] — the leading "/" clamps escaping
	//      attempts at the root. Concretely, a request for
	//      "/../../etc/passwd" cleans to "/etc/passwd" and the file the
	//      server then tries to open is ./public/etc/passwd. No escape.
	//      (Caveats that survive: symlinks pointing out of the tree, and
	//      dot-files like .git — http.Dir happily serves both.)

	// For every request that lands here, the file server, for free:
	//
	//   * finds the file at the URL path (with the clamping above);
	//   * sets Content-Type from the extension (styles.css -> "text/css;
	//     charset=utf-8", data.json -> "application/json", pixel.png ->
	//     "image/png") via the mime package — unknown extensions get
	//     sniffed by reading the first 512 bytes instead;
	//   * serves index.html when a directory URL is requested ("/" is
	//     really "/index.html"), and redirects URLs ending in
	//     "/index.html" to the directory's clean URL;
	//   * redirects "/docs" -> "/docs/" (301) so relative links resolve;
	//   * generates a minimal HTML listing for directories that have no
	//     index.html — visit /docs/ to see it;
	//   * sets Last-Modified and answers 304 Not Modified when the client
	//     sends If-Modified-Since with that exact time (re-validation);
	//   * sets Accept-Ranges: bytes and answers 206 Partial Content for
	//     Range requests — this is what makes video scrubbing and
	//     resumable downloads work;
	//   * sets Content-Length and streams the file with io.Copy (it does
	//     NOT read the whole file into memory);
	//   * answers HEAD with headers only.
	//
	// One surprise worth knowing: the file server has NO opinion about
	// HTTP methods — POST or DELETE to /notes.txt answers 200 with the
	// file, just like GET. Only the ROUTER can scope methods (see the
	// "GET /hello/{name}" pattern below).

	// ===================================================================
	// STEP 3 — Mounting the same files under /static/ — http.StripPrefix.
	// ===================================================================
	//
	// We register the same fileServer value again, but under a prefix.
	// The problem: the file server interprets the FULL URL path as a
	// path inside ./public. Without help, a request for
	// "/static/notes.txt" would look for ./public/static/notes.txt —
	// which does not exist.
	//
	// http.StripPrefix("/static", fileServer) is a handler wrapper that
	// removes "/static" from the URL path, then calls the wrapped
	// handler:
	//
	//	URL requested:  /static/notes.txt
	//	seen by mux:     matches "/static/" subtree (or "/" catch-all?)
	//	passed along:   /notes.txt   <- prefix removed by StripPrefix
	//	opened on disk: ./public/notes.txt
	//
	// Register "/static" WITHOUT the trailing slash inside StripPrefix
	// (so that "/static/notes.txt" -> "/notes.txt", not "notes.txt").
	// If a request arrives whose path does not start with the prefix,
	// StripPrefix answers 404 itself — the inner handler never runs.
	// (You will never see that 404 here: for patterns ending in "/", the
	// mux itself first redirects "/static" to "/static/".)
	//
	// StripPrefix is your first MIDDLEWARE-shaped idea: a function that
	// takes a Handler and returns a new Handler that does something
	// first (here: rewrite the request) and delegates after.
	mux.Handle("/static/", http.StripPrefix("/static", fileServer))

	// ===================================================================
	// STEP 4 — A "normal" handler for contrast: patterns can be code.
	// ===================================================================
	//
	// Not every route is a file. "GET /hello/{name}" is a pattern with:
	//
	//   * a METHOD part: "GET" — this route also matches HEAD, and
	//     NOTHING ELSE. A POST here does not "fail" — the pattern simply
	//     does not match it, and the mux keeps looking for one that
	//     does... and finds our "/" catch-all, which names no method and
	//     therefore matches EVERY method. The FileServer receives the
	//     POST, looks for ./public/hello/rosen, and answers 404. Try it:
	//     a method-less pattern always shadows method-scoped ones for
	//     other methods.
	//
	//     The mux's dedicated "wrong method" answer, 405 Method Not
	//     Allowed, is what you get when a path matches ONLY method-
	//     scoped patterns: a bare mux with just "GET /hello/{name}"
	//     registered replies "405 + Allow: GET, HEAD" to a POST there.
	//   * a wildcard {name}: exactly ONE path segment ("/hello/a/b" does
	//     NOT match), readable inside the handler with r.PathValue.
	//
	// mux.HandleFunc takes a FUNCTION with this shape, while mux.Handle
	// takes a Handler value — HandleFunc exists purely so you don't have
	// to spell out the conversion: under the hood the function is wrapped
	// in http.HandlerFunc, a type whose only purpose is to give a plain
	// function a ServeHTTP method (a classic Go "adapter type").
	mux.HandleFunc("GET /hello/{name}", helloHandler)

	// ===================================================================
	// STEP 5 — Serving ONE file: http.ServeFile (plus a download header).
	// ===================================================================
	//
	// http.ServeFile is the single-file version of the FileServer: you
	// name the file (an OS path, relative to the CWD), it does the rest.
	// It re-uses the exact same internals, so Range requests, 304s and
	// MIME detection still work for this route too.
	//
	// Two documented safety behaviors of ServeFile:
	//
	//   * it rejects with 400 any request whose r.URL.Path contains a
	//     ".." path element — a seatbelt against callers who build file
	//     names from raw user input;
	//   * a relative name is interpreted from the current directory and
	//     "may ascend to parent directories" (e.g. "../config"). The
	//     lesson: the name argument is TRUSTED input, the URL is NOT.
	//
	// We also set Content-Disposition: attachment — the standard way to
	// tell a browser "save this instead of displaying it".
	// HEADS UP on ordering: headers must be set BEFORE the response body
	// starts, because the moment any byte of the body is written, the
	// header block is already gone over the wire.
	mux.HandleFunc("GET /download", downloadHandler)

	// ===================================================================
	// STEP 6 — URL-driven file choice + input validation.
	// ===================================================================
	//
	// Here the URL picks the file: /files/notes.txt serves the file
	// named "notes.txt" from ./public. This is exactly the shape of a
	// real "download this resource" endpoint, and exactly where real
	// programs get pwned: the path segment comes from the INTERNET, so
	// it gets treated as guilty until proven innocent.
	//
	// See the validation inside serveByName — and note that it is not
	// even strictly necessary here (ServeFile's ".." check would fire),
	// but defense in depth means never relying on one seatbelt.
	mux.HandleFunc("GET /files/{name}", serveByName)

	// ===================================================================
	// STEP 7 — Middleware: watch the file server work with your own eyes.
	// ===================================================================
	//
	// logRequests wraps the whole mux in one line per request: method,
	// path, resulting status code and duration. Load http://localhost:8080
	// in a browser and see the THREE requests the middleware reports
	// (/, /styles.css, /pixel.png). Then re-load with the devtools open
	// and see status codes 301, 304 and 404 appear as you try the URLs.
	//
	// Middleware is "func(Handler) Handler": a value that wraps behavior.
	// Nearly every Go web concern that is not "handle the request" —
	// logging, CORS, auth, rate limiting — is expressed exactly like the
	// little logRequests function below.
	handlerWithLogging := logRequests(mux)

	// ===================================================================
	// STEP 8 — The server itself: http.Server.
	// ===================================================================
	//
	// http.Server OWNS the TCP listener and the connection handling
	// loop. The zero value is a valid server; we set the three fields
	// that matter for learning:
	//
	//   Addr:    ":8080" = listen on port 8080 on ALL interfaces (Wi-Fi
	//            included — your phone can open this server). Use
	//            "localhost:8080" to restrict to this machine only.
	//   Handler: the mux, wrapped in our middleware. (Passing nil would
	//            make the server use http.DefaultServeMux.)
	//   ReadHeaderTimeout: gives slow clients 5 seconds to send request
	//            headers, or the connection is dropped. Without ANY
	//            read timeout a malicious client can hold connections
	//            open forever (the "slowloris" attack) and exhaust fds.
	//
	// ListenAndServe then BLOCKS the current goroutine forever (that is
	// why main never returns while the server runs) and it only ever
	// returns a non-nil error — port already in use, permissions...
	// log.Fatal(err) prints it and exits with status 1, the idiomatic
	// one-liner for "a server we can't start is a program we can't run".
	server := &http.Server{
		Addr:              ":8080",
		Handler:           handlerWithLogging,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("file server listening on http://localhost:8080  (Ctrl+C stops it)")
	log.Fatal(server.ListenAndServe())
}

// ----------------------------------------------------------------------
// The handlers from STEPS 4-6, as named functions.
//
// The signature is the whole contract: receive a ResponseWriter (the
// response being assembled) and a *Request (everything the client asked).
// w and r below always mean exactly that, all over the Go ecosystem.
// ----------------------------------------------------------------------

// helloHandler answers GET /hello/{name}. It contrasts with the file
// server: here Go code PRODUCES the response instead of reading a file.
func helloHandler(w http.ResponseWriter, r *http.Request) {
	// r.PathValue returns the value a mux wildcard captured by NAME.
	// It is already URL-decoded ("rosen%20jr" would arrive as "rosen jr").
	name := r.PathValue("name")

	// fmt.Fprintf writes formatted text INTO the response body.
	// We never called w.WriteHeader: the first write sends an implicit
	// "HTTP/1.1 200 OK" status line. The response literally is: status
	// 200, no special headers, this text.
	fmt.Fprintf(w, "Hello, %s! You sent %s %s.\n", name, r.Method, r.URL.Path)
}

// downloadHandler answers GET /download with public/notes.txt, but as an
// attachment: the browser will offer to SAVE it instead of displaying it.
func downloadHandler(w http.ResponseWriter, r *http.Request) {
	// w.Header() is the header MAP for this response. Headers set here
	// travel when the response starts — which, remember, happens at the
	// first Write (or explicit WriteHeader) below/inside ServeFile.
	w.Header().Set("Content-Disposition", `attachment; filename="notes.txt"`)

	// ServeFile streams the named file with all the FileServer smarts
	// (MIME from extension, 304 revalidation, Range support...).
	// The name is a constant chosen by US, not by the client: trusted.
	http.ServeFile(w, r, "./public/notes.txt")
}

// serveByName answers GET /files/{name}: one file picked from ./public by
// a URL segment. THE most important lesson in this file lives here:
// never feed a URL-derived string into a file path without checking it.
func serveByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Reject anything that is not a boring single file name.
	//
	// Why each check, concretely:
	//
	//   name == ""      a stray request like "/files/" would otherwise
	//                   open "./public/" — a DIRECTORY listing leak.
	//   contains "/"   the {name} wildcard stops at slashes, BUT the mux
	//                   matches paths "unescaped segment by segment": a
	//                   %2F in the URL becomes a real "/" in PathValue,
	//                   letting "/files/docs%2Fsecret.txt" pretend to be
	//                   two levels. After this check, that is a 400.
	//   contains ".."  the classic escape attempt ("../" walks out).
	//                   http.ServeFile would also reject ".." in the URL
	//                   with a 400 (its documented seatbelt), but when
	//                   the rule is "this must never happen", you say it
	//                   in YOUR code instead of hoping the next layer
	//                   catches it: defense in depth.
	//
	// Note what we do NOT do: we don't try to cleverly "clean" the
	// input and carry on (filepath.Join("./public", "..") is "." — the
	// CWD listing!). We refuse loudly, with http.Error, which writes a
	// small plain-text body plus the status code we pass.
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}

	// Only now is the string safe to become part of a path. "./public/" +
	// a slash-free, dotdot-free name can only name a file INSIDE
	// ./public (the top level of it — that's the product decision here).
	http.ServeFile(w, r, "./public/"+name)
}

// ----------------------------------------------------------------------
// Middleware machinery (STEP 7).
// ----------------------------------------------------------------------

// logRequests takes any handler and returns one that logs each request
// after it finishes. It doesn't know or care that a file server is
// inside: middleware works for EVERY handler, which is the point.
func logRequests(next http.Handler) http.Handler {
	// We must return a Handler (an interface). http.HandlerFunc adapts
	// the closure below into one — the same trick HandleFunc uses.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// statusRecorder (below) wraps w and RECORDS the status code
		// the inner handler sends, by intercepting the one method that
		// sets it. Everything else passes straight through.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r) // <- the actual request work happens here

		// After the response is done, we finally know how it went.
		// Watch this line while you click around: 200s, the mux's 301s,
		// the file server's 404s, 304s on If-Modified-Since, 206s on
		// Range, 405s when you try POST on a GET-scoped route...
		log.Printf("%s %-28s -> %d %s (in %v)",
			r.Method, r.URL.Path, rec.status, http.StatusText(rec.status),
			time.Since(start).Round(time.Microsecond))
	})
}

// statusRecorder wraps an http.ResponseWriter and remembers the status
// code written to it. This "wrap and override" is how you observe what a
// wrapped handler did with the response.
type statusRecorder struct {
	http.ResponseWriter // embedded interface: every method delegates for free

	// status defaults to 200: handlers that never call WriteHeader
	// (like most of ours) still produce a "200 OK" response.
	status int
}

// WriteHeader intercepts the one method we care about, records the code,
// and forwards the call so the response is unaffected.
func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}
