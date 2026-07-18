// Package dashboard serves the single embedded HTML page (DESIGN §9):
// server-rendered JSON + minimal vanilla JS, no framework, no build step.
package dashboard

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler serves the page.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}
}
