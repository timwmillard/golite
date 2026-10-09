package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/timwmillard/golite/server"
)

// Register mounts the API's routes, plus the spec at GET /openapi.json, on
// mux.
func Register(mux *http.ServeMux, db *sql.DB) {
	// The generated default error handlers write err.Error() as
	// text/plain, which for response errors leaks database details to the
	// client; golite's send JSON and log the real error instead.
	h := NewStrictHandlerWithOptions(NewTaskHandler(db), nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  server.RequestError,
		ResponseErrorHandlerFunc: server.ResponseError,
	})
	HandlerWithOptions(h, StdHTTPServerOptions{
		BaseRouter:       mux,
		ErrorHandlerFunc: server.RequestError,
	})

	mux.HandleFunc("GET /openapi.json", serveSpec)
}

func serveSpec(w http.ResponseWriter, r *http.Request) {
	spec, err := GetSwagger()
	if err != nil {
		server.ResponseError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(spec)
}
