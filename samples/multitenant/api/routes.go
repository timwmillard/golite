package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/master/model"
)

// handlers implements StrictServerInterface: tenant operations on the master
// database, task operations on each tenant's own.
type handlers struct {
	*TenantHandler
	*TaskHandler
}

// Register mounts the API's routes, plus the spec at GET /openapi.json, on
// mux. Routes below /v1/tenants/{tenant}/ run against that tenant's
// database from dbs; the rest use master.
func Register(mux *http.ServeMux, master *sql.DB, dbs *tenant.DBs, riverClient *river.Client[*sql.Tx]) {
	h := NewStrictHandlerWithOptions(handlers{
		TenantHandler: NewTenantHandler(master, dbs, riverClient),
		TaskHandler:   &TaskHandler{},
	}, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  server.RequestError,
		ResponseErrorHandlerFunc: server.ResponseError,
	})
	HandlerWithOptions(h, StdHTTPServerOptions{
		BaseRouter:       mux,
		ErrorHandlerFunc: server.RequestError,
		Middlewares:      []MiddlewareFunc{dbs.Middleware(resolveTenant(model.New(master)))},
	})

	mux.HandleFunc("GET /openapi.json", serveSpec)
}

// resolveTenant maps the {tenant} slug in the path to the tenant's ID,
// which names its database. /v1/tenants/{tenant} itself also has the slug
// but only touches the master database, so it only resolves routes below
// a tenant. This is where an app would also check the caller may
// access the tenant.
func resolveTenant(q *model.Queries) tenant.Resolver {
	return func(r *http.Request) (string, error) {
		if !strings.Contains(r.Pattern, "/v1/tenants/{tenant}/") {
			return "", nil
		}

		t, err := q.GetTenantBySlug(r.Context(), r.PathValue("tenant"))
		if errors.Is(err, sql.ErrNoRows) {
			return "", tenant.ErrNotFound
		}
		if err != nil {
			return "", fmt.Errorf("resolve tenant: %w", err)
		}
		return conv.FormatID(t.ID), nil
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
