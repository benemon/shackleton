//go:build ui

// Package ui serves the embedded web console. Assets are built by `make ui`
// before compiling with the ui tag; the console loads unauthenticated and
// holds the bearer token client-side — every API call it makes stays gated.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".") {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}
