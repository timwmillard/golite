package api

import (
	"database/sql"
	"net/http"

	golite "github.com/timwmillard/golite/api"
)

// Register mounts the API's routes, plus the spec at GET /openapi.json, on
// mux.
func Register(mux *http.ServeMux, db *sql.DB) {
	h := NewStrictHandlerWithOptions(NewTaskHandler(db), nil, StrictHTTPServerOptions{
		// The generated defaults write err.Error() as text/plain, which
		// for response errors leaks database details to the client.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			golite.WriteError(w, http.StatusBadRequest, err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			golite.InternalError(w, r, "API request failed", err)
		},
	})
	HandlerWithOptions(h, StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			golite.WriteError(w, http.StatusBadRequest, err.Error())
		},
	})

	mux.HandleFunc("GET /openapi.json", serveSpec)
}

func serveSpec(w http.ResponseWriter, r *http.Request) {
	spec, err := GetSwagger()
	if err != nil {
		golite.InternalError(w, r, "Load OpenAPI spec", err)
		return
	}
	golite.WriteJSON(w, http.StatusOK, spec)
}
