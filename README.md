# go-by-example

## About

Snippets and code examples regarding different topics using Go. Snippets about concurrency, network programming, standard go libraries examples etc.

Written and verified against **Go 1.26.5** — every example compiles (`go vet` / `go build` clean), runs, and its documented outputs are real observations, not copies from tutorials.

## How to use this repository

- One module at the root (`go.mod`), so every folder is a runnable example.
- **Every example is meant to be read top to bottom**: the code comments ARE the lesson.
- Each topic folder has a `README.md` explaining the concept, the key characteristics, the use cases, and a lab of experiments to run.
- Run examples from inside their folder (relative paths are resolved from the current working directory):

```bash
cd webdev/fileserver
go run .
```

## Topics

| Folder | What you learn |
|---|---|
| [`webdev/fileserver/`](webdev/fileserver/README.md) | serving files over HTTP: `http.FileServer`, `http.Dir`, `StripPrefix`, `ServeFile`, `//go:embed`, mux patterns, middleware |

(Add new topics as rows here.)
