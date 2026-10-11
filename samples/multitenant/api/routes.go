package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"
)

// handlers implements StrictServerInterface: company operations on the master
// database, task operations on each company's own.
type handlers struct {
	*CompanyHandler
	*TaskHandler
}

// Register mounts the API's routes, plus the spec at GET /openapi.json, on
// mux. The task routes, /v1/companies/{company}/tasks..., run against that
// company's database from dbs; the rest use master.
func Register(mux *http.ServeMux, master *sql.DB, dbs *tenant.DBs, riverClient *river.Client[*sql.Tx]) {
	h := NewStrictHandlerWithOptions(handlers{
		CompanyHandler: NewCompanyHandler(master, dbs, riverClient),
		TaskHandler:    &TaskHandler{},
	}, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  server.RequestError,
		ResponseErrorHandlerFunc: server.ResponseError,
	})
	HandlerWithOptions(h, StdHTTPServerOptions{
		BaseRouter:       mux,
		ErrorHandlerFunc: server.RequestError,
		Middlewares:      []MiddlewareFunc{dbs.Middleware(resolveCompany())},
	})

	mux.HandleFunc("GET /openapi.json", serveSpec)
}

// resolveCompany returns the {company} slug in the path as the company's
// database id, without reading the master database: the database is
// company_<slug>.db, and tenant.Middleware answers 404 if there's no such
// file, which includes a company being deleted. So requests for one
// company never wait on master, or on each other. Only the task routes use
// the company's database; the others with {company} only touch the master
// database.
//
// This is also where an app could check the caller may use the company,
// against wherever its users live: master for a global login that spans
// companies, or the company's own database for users tied to one.
func resolveCompany() tenant.Resolver {
	return func(r *http.Request) (string, error) {
		if !strings.Contains(r.Pattern, "/v1/companies/{company}/tasks") {
			return "", nil
		}
		slug := r.PathValue("company")
		if !validSlug.MatchString(slug) {
			return "", tenant.ErrNotFound
		}
		return slug, nil
	}
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
