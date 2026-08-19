//go:build !ui

// Package ui serves the embedded web console; without the ui build tag the
// binary carries no assets and answers 404.
package ui

import "net/http"

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "web console not built into this binary", http.StatusNotFound)
	})
}
