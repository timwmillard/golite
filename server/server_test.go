package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/riverqueue/river"
)

type pingArgs struct{}

func (pingArgs) Kind() string { return "ping" }

type pingWorker struct {
	river.WorkerDefaults[pingArgs]
	done chan struct{}
}

func (w *pingWorker) Work(ctx context.Context, job *river.Job[pingArgs]) error {
	close(w.done)
	return nil
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	srv, err := New(t.Context(), Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(ctx) }()

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRun_WorksRiverJobs(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	worker := &pingWorker{done: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)

	srv, err := New(t.Context(), Config{
		Addr:            "127.0.0.1:0",
		DB:              db,
		Workers:         workers,
		RiverUI:         true,
		RiverUIUsername: "admin",
		RiverUIPassword: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := srv.River.Insert(t.Context(), pingArgs{}, nil); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(ctx) }()

	select {
	case <-worker.done:
	case <-time.After(10 * time.Second):
		t.Fatal("job was not worked")
	}

	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/riverui/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /riverui/ = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestHealth(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	srv, err := New(t.Context(), Config{DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusOK {
		t.Errorf("healthy /health = %d, want %d", rec.Code, http.StatusOK)
	}

	db.Close()
	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("closed db /health = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHealth_Disabled(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	srv, err := New(t.Context(), Config{DB: db, DisableHealth: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The app can register its own without a duplicate-pattern panic.
	srv.Mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("/health = %d, want app's handler", rec.Code)
	}
}
