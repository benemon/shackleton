//go:build ui

// Package ui serves the embedded web console. Assets are built by `make ui`
// before compiling with the ui tag; the console loads unauthenticated and
// holds the bearer token client-side — every API call it makes stays gated.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}
