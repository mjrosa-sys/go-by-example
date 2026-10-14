// webdev/fileserver/no-listing — turning directory listings OFF.
//
// When http.FileServer opens a directory that has no index.html, it
// generates an HTML listing of the directory's contents and serves that
// (try /docs/ on the previous example). Handy in development; in
// production it is an information leak — it hands strangers a map of
// your files.
//
// The standard library has no setting for this. The idiomatic recipe
// (this one is a classic, found in the wild everywhere) is to wrap the
// http.FileSystem interface: intercept Open(), and when the requested
// path is a directory WITHOUT an index.html, report "file not found".
// The FileServer then answers 404, exactly as if the directory did not
// exist.
//
// It also teaches a bigger lesson: because the file server only knows
// the FileSystem INTERFACE, we can change what "opening a file" MEANS
// without touching the file server at all.
//
// Run it from THIS directory:
//
//	go run .
//
// then open http://localhost:8082 and try:
//
//	/               -> public/index.html  (root has index.html: unaffected)
//	/about/         -> about/index.html   (directory WITH index: unaffected)
//	/docs/          -> 404 page not found (would have listed files)
//	/docs/secret.txt-> 200 — the file ITSELF is still served!
//
// That last line is the security moral of this example: hiding listings
// is obscurity, not access control. Knowing a name still gets you the
// file. Real protection = authorization (checking who asks), not hoping
// nobody guesses.
package main

import (
	"io/fs" // fs.ErrNotExist: the SENTINEL error the file server maps to 404
	"log"
	"net/http"
)

// hiddenDirFS is a FileSystem wrapper: it stores the wrapped one in a
// field and adds behavior. This is composition over inheritance — Go's
// only inheritance-shaped move is "embed a value and delegate",
// and embedding an INTERFACE (not a struct) delegates every method
// while still letting us override just one.
type hiddenDirFS struct {
	inner http.FileSystem // anything that can Open files: http.Dir, http.FS(...), ...
}

// Open implements http.FileSystem. The file server calls it for every
// request path (and once more for each directory's index.html lookup).
func (h hiddenDirFS) Open(name string) (http.File, error) {
	// First, delegate: let the wrapped file system do the real work of
	// finding and opening the thing.
	f, err := h.inner.Open(name)
	if err != nil {
		// Genuine failure (missing, permission, ...) — pass the error
		// through untouched. The file server maps it to a status code
		// by matching against sentinel errors:
		//     fs.ErrNotExist  -> 404 page not found
		//     fs.ErrPermission-> 403 Forbidden
		//     anything else   -> 500 Internal Server Error
		// That last line matters: return some random error here and you
		// will serve 500s. Error handling is part of the CONTRACT.
		return nil, err
	}

	// The file opened — but is it a directory?
	info, err := f.Stat()
	if err != nil {
		f.Close() // don't leak the handle we already opened
		return nil, err
	}

	if info.IsDir() {
		// A directory. Decide its fate by looking for an index.html
		// inside it. name may or may not end in "/" ("/docs" vs
		// "/docs/") — appending "/index.html" produces either
		// "/docs/index.html" or "/docs//index.html", and http.Dir
		// cleans the doubled slash for us (path.Clean runs on every
		// Open). If you wrap a different FileSystem, make sure IT
		// normalizes paths too.
		index, err := h.inner.Open(name + "/index.html")
		if err != nil {
			// No index.html in there: lie to the file server. We
			// return fs.ErrNotExist — the exact sentinel the file
			// server translates into a clean "404 page not found" —
			// so the directory looks nonexistent. Nobody gets a
			// listing; the files inside still answer direct requests.
			f.Close()
			return nil, fs.ErrNotExist
		}
		// An index.html exists: close our probe and serve normally.
		// (The file server itself will open the index again and serve
		// it as the directory's content — visit /about/.)
		index.Close()
	}

	// Either a plain file, or a directory WITH an index.html: in both
	// cases the file server should behave exactly as before.
	return f, nil
}

func main() {
	mux := http.NewServeMux()

	// The wiring is one line, because the whole trick lives in the
	// type above: wrap http.Dir in our hiddenDirFS, hand THAT to the
	// file server. http.Dir provides files from disk; hiddenDirFS
	// changes one rule on top.
	mux.Handle("/", http.FileServer(hiddenDirFS{http.Dir("./public")}))

	server := &http.Server{
		Addr:    ":8082",
		Handler: mux,
	}

	log.Println("no-listing file server listening on http://localhost:8082")
	log.Println("try: http://localhost:8082/docs/      (404: listing disabled)")
	log.Println("try: http://localhost:8082/docs/secret.txt  (200: hiding != protecting)")
	log.Fatal(server.ListenAndServe())
}
