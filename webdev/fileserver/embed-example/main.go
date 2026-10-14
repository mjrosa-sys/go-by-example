// webdev/fileserver/embed-example — baking the files INTO the binary.
//
// main.go (the previous example) serves files from a directory on disk.
// The disk dependency is a deployment liability: ship the binary and
// forget the assets folder, and the site 404s. Go's answer since 1.16 is
// //go:embed: files compiled INTO the executable. The result is a
// single, self-contained binary you can scp anywhere and run — the
// classic "Go as a static site deployable" story.
//
// Run it from THIS directory:
//
//	go run .
//
// then open http://localhost:8081. To prove the point, build and move
// the binary somewhere else entirely before running it:
//
//	go build -o /tmp/app .
//	cd / && /tmp/app          # still serves everything, no ./assets needed
package main

import (
	"embed" // provides embed.FS, the type that holds embedded files at run time
	"io/fs" // the generic "file system" interface (fs.FS) and fs.Sub
	"log"   // timestamped logging; Fatal exits the program
	"net/http"
)

// The line below is a COMPILER DIRECTIVE (it must start with "//go:embed"
// with NO space after "//" and sit directly above the variable):
//
//	//go:embed assets
//	var assets embed.FS
//
// Meaning: at build time, walk the assets/ directory tree and store its
// files inside the binary, reachable through the variable `assets`.
//
// Rules worth knowing:
//
//   - patterns are relative to THIS source file's directory, always use
//     "/" separators (yes, even on Windows);
//   - naming a DIRECTORY embeds the whole tree recursively — but skips
//     files beginning with "." or "_" (so ".git" never sneaks in); use
//     the "all:assets" prefix if you DO want those;
//   - the variable must be at package scope and typed string, []byte,
//     or embed.FS (FS for multiple files; string/[]byte for exactly one);
//   - imports must resolve: symlinks, other modules, and paths leaving
//     the module are refused at compile time.
//
// Note the type of `assets`: embed.FS is read-only, and it implements
// the io/fs interfaces (fs.FS, fs.ReadDirFS, ...). That's the golden
// design idea of modern Go I/O: ANYTHING that can open paths can be a
// "file system" — a zip, a tar, memory, a test fixture, embedded files.
// Every consumer takes the interface, so every producer just works.
//
//go:embed assets
var assets embed.FS

func main() {
	// assets' root is THIS package's directory, so files live under
	// "assets/..." inside the FS: Open("index.html") would MISS.
	//
	// fs.Sub re-roots a file system: it returns a view of `assets` where
	// the "assets" folder itself is the root. Now Open("index.html")
	// maps to what was assets/index.html. Errors only if the named
	// subdirectory doesn't exist in the parent FS.
	content, err := fs.Sub(assets, "assets")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// FileServerFS is the fs.FS-flavored twin of http.FileServer:
	//
	//     http.FileServer(http.Dir("./public"))   // serves from DISK
	//     http.FileServerFS(content)              // serves from ANY fs.FS
	//
	// Internally FileServerFS is a one-liner adapter:
	// FileServer(http.FS(content)) — http.FS converts an fs.FS into the
	// older http.FileSystem interface the file server consumes.
	//
	// Everything else from main.go still applies, byte for byte:
	// index.html handling, directory listings (embed.FS implements
	// fs.ReadDirFS, so /docs/ lists fine), MIME types, Range requests,
	// Last-Modified (embedded files get a fixed modtime), 404s. The
	// ONLY difference is where bytes come from.
	mux.Handle("/", http.FileServerFS(content))

	// No timeouts set this time (see main.go for the tour of Server
	// fields) — keep examples focused on ONE idea each.
	server := &http.Server{
		Addr:    ":8081",
		Handler: mux,
	}

	log.Println("embedded file server listening on http://localhost:8081")
	log.Fatal(server.ListenAndServe())
}
