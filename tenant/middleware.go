package tenant

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/timwmillard/golite/server"
)

// ErrNotFound means a tenant doesn't exist. A Resolver returns it when the
// request names a tenant that doesn't exist, and Middleware answers it with
// a 404 of {"error": "<Config.Name> not found"}. Sync.Run fails with it
// for a tenant that isn't in the master database.
var ErrNotFound = errors.New("tenant not found")

// IsNotFound reports whether err means the tenant doesn't exist, as far
// as a request is concerned: ErrNotFound, ErrNotExist (no database), or
// ErrDeleting (being deleted). Middleware answers these with a 404; use it
// to do the same where an app calls Acquire or Do itself.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotExist) || errors.Is(err, ErrDeleting)
}

// Resolver returns the ID of the tenant r is for, or "" if r isn't for a
// tenant (e.g. a route on the master database). It can also be where the
// app checks, if it has auth, that the caller may use the tenant; where
// users and sessions live (master, or the tenant's database) is up to the
// app.
//
// Take the ID from the request itself, such as the {tenant} path value,
// and leave checking the tenant exists to Middleware, which answers 404 if
// its database file doesn't. Don't read the master database just to find
// the ID: every tenant's requests would queue on it, which is what separate
// databases are there to avoid.
type Resolver func(r *http.Request) (string, error)

// Middleware resolves each request's tenant and puts its ID and database in
// the request context, for ID and DB, holding the database open until the
// handler returns. Requests the Resolver returns "" for
// pass through untouched. A Resolver error of ErrNotFound, or a tenant
// whose database doesn't exist or is being deleted, is a 404. Any other
// error, or failing to open the database, is a 500 via
// server.ResponseError.
//
// With oapi-codegen's std-http server, pass it in
// StdHTTPServerOptions.Middlewares: those run after routing, so the
// Resolver can read path parameters with r.PathValue.
func (d *DBs) Middleware(resolve Resolver) func(http.Handler) http.Handler {
	notFound := d.cfg.Name + " not found"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := resolve(r)
			if IsNotFound(err) {
				server.WriteError(w, http.StatusNotFound, notFound)
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
			switch {
			case IsNotFound(err):
				server.WriteError(w, http.StatusNotFound, notFound)
				return
			case err != nil:
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
