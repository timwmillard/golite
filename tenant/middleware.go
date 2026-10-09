package tenant

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/timwmillard/golite/server"
)

// ErrNotFound is returned by a Resolver when the request names a tenant
// that doesn't exist. Middleware answers it with a 404.
var ErrNotFound = errors.New("tenant not found")

// Resolver returns the ID of the tenant r is for, or "" if r isn't for a
// tenant (e.g. a route on the master database). It's where the app checks
// the tenant exists and, if it has auth, that the caller may use it.
type Resolver func(r *http.Request) (string, error)

// Middleware resolves each request's tenant and puts its ID and database in
// the request context, for ID and DB, holding the database open until the
// handler returns. Requests the Resolver returns "" for
// pass through untouched. A Resolver error of ErrNotFound, or a tenant
// whose database doesn't exist, is a 404; any other error, or failing to
// open the database, is a 500 via server.ResponseError.
//
// With oapi-codegen's std-http server, pass it in
// StdHTTPServerOptions.Middlewares: those run after routing, so the
// Resolver can read path parameters with r.PathValue.
func (d *DBs) Middleware(resolve Resolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := resolve(r)
			if errors.Is(err, ErrNotFound) {
				server.WriteError(w, http.StatusNotFound, ErrNotFound.Error())
				return
			}
			if err != nil {
				server.ResponseError(w, r, err)
				return
			}
			if id == "" {
				next.ServeHTTP(w, r)
				return
			}

			db, release, err := d.Acquire(r.Context(), id)
			if errors.Is(err, ErrNotExist) {
				server.WriteError(w, http.StatusNotFound, ErrNotFound.Error())
				return
			}
			if err != nil {
				server.ResponseError(w, r, err)
				return
			}
			defer release()
			next.ServeHTTP(w, r.WithContext(WithDB(r.Context(), id, db)))
		})
	}
}

type ctxKey struct{}

type ctxValue struct {
	id string
	db *sql.DB
}

// WithDB returns ctx carrying tenant id and its database, as Middleware
// does. Use it to call tenant code from a job worker or a test, inside Do
// so the database stays open.
func WithDB(ctx context.Context, id string, db *sql.DB) context.Context {
	return context.WithValue(ctx, ctxKey{}, ctxValue{id: id, db: db})
}

// ID returns the tenant ID in ctx, or "" if there isn't one.
func ID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(ctxValue)
	return v.id
}

// DB returns the tenant database in ctx. It panics if there isn't one: a
// tenant handler reached without Middleware is a routing bug, not a
// request error.
func DB(ctx context.Context) *sql.DB {
	v, ok := ctx.Value(ctxKey{}).(ctxValue)
	if !ok {
		panic("tenant: no tenant database in context; is the route behind tenant.Middleware?")
	}
	return v.db
}
