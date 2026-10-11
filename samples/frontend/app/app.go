// Package app is the frontend: templ components (*.templ, compiled to
// *_templ.go), the handlers that render them, and the static files they
// load. Tailwind builds static/styles.css from the classes in the .templ
// files, starting from styles.css.
package app

import (
	"database/sql"
	"embed"
	"net/http"

	"github.com/timwmillard/golite/samples/frontend/model"
)

//go:embed static
var static embed.FS

// Register mounts the pages, the htmx endpoints they call and /static/ on
// mux.
func Register(mux *http.ServeMux, db *sql.DB) {
	h := &taskHandler{q: model.New(db)}

	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", h.index)
	app.HandleFunc("POST /tasks", h.create)
	app.HandleFunc("POST /tasks/{id}/toggle", h.toggle)
	app.HandleFunc("DELETE /tasks/{id}", h.delete)
	app.Handle("GET /static/", http.FileServerFS(static))

	// Rejects cross-site POSTs and DELETEs (CSRF) using the browser's
	// Sec-Fetch-Site and Origin headers, so the forms need no tokens.
	mux.Handle("/", http.NewCrossOriginProtection().Handler(app))
}
