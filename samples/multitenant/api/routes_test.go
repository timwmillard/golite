package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	companymigrations "github.com/timwmillard/golite/samples/multitenant/companydb/migrations"
	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
)

// Task requests must never touch the master database, so companies don't
// queue on it: they work with master closed.
func TestTaskRoutesDontUseMaster(t *testing.T) {
	dir := t.TempDir()
	master, err := server.OpenDB(t.Context(), filepath.Join(dir, "master.db"), mastermigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	dbs := tenant.New(tenant.Config{Name: "company", Dir: filepath.Join(dir, "companies"), Migrations: companymigrations.FS, IdleTimeout: -1})
	defer dbs.Close()
	if err := dbs.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	Register(mux, master, dbs, nil, nil)
	master.Close()

	tests := []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/companies/acme/tasks", `{"title":"Buy milk"}`, http.StatusCreated},
		{"GET", "/v1/companies/acme/tasks", "", http.StatusOK},
		{"GET", "/v1/companies/acme/tasks/1", "", http.StatusOK},
		{"GET", "/v1/companies/nope/tasks", "", http.StatusNotFound},
		{"GET", "/v1/companies/Bad_Slug/tasks", "", http.StatusNotFound},
		// A company route does use master, so it fails: the check works.
		{"GET", "/v1/companies/acme", "", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
		if w.Code != tt.status {
			t.Errorf("%s %s: status %d, want %d: %s", tt.method, tt.path, w.Code, tt.status, w.Body)
		}
	}
}
