// Command server is an example app wiring up the golite packages: config
// from .env / env vars / flags, a colored logger, SQLite with migrations, a
// River job queue with RiverUI, and a small JSON API.
//
//	go run ./samples/server -log-level debug
//	curl -X POST localhost:7880/api/greetings -d '{"name":"Tim"}'
//	curl localhost:7880/api/greetings
//	open http://localhost:7880/riverui/
package main

import (
	"context"
	"database/sql"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/api"
	"github.com/timwmillard/golite/migrate"
	"github.com/timwmillard/golite/server"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Defaults go in the struct; .env, env vars and flags override them.
	cfg := server.Config{Port: 7880, RiverUI: true}
	dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
	if err := cfg.Parse(); err != nil {
		return err
	}
	log := cfg.Logger

	db, err := openDB(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	workers := river.NewWorkers()
	river.AddWorker(workers, &GreetWorker{DB: db, Logger: log})
	cfg.DB, cfg.Workers = db, workers

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}

	h := &handlers{db: db, river: srv.River}
	srv.Mux.HandleFunc("GET /health", h.health)
	srv.Mux.HandleFunc("GET /api/greetings", h.listGreetings)
	srv.Mux.HandleFunc("POST /api/greetings", h.createGreeting)

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}

func openDB(ctx context.Context, dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "app.db"))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite allows one writer at a time; a single connection avoids
	// SQLITE_BUSY between the app and River.
	db.SetMaxOpenConns(1)

	migrations, _ := fs.Sub(migrationsFS, "migrations") // only errors on an invalid path
	if err := migrate.Apply(ctx, db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return db, nil
}

// GreetArgs is a River job that stores a greeting for Name.
type GreetArgs struct {
	Name string `json:"name"`
}

func (GreetArgs) Kind() string { return "greet" }

type GreetWorker struct {
	river.WorkerDefaults[GreetArgs]

	DB     *sql.DB
	Logger *slog.Logger
}

func (w *GreetWorker) Work(ctx context.Context, job *river.Job[GreetArgs]) error {
	msg := fmt.Sprintf("Hello, %s!", job.Args.Name)
	if _, err := w.DB.ExecContext(ctx,
		`insert into greetings (name, message, created_at) values (?, ?, ?)`,
		job.Args.Name, msg, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("insert greeting: %w", err)
	}
	w.Logger.Info("Greeted", "name", job.Args.Name, "job_id", job.ID)
	return nil
}

type handlers struct {
	db    *sql.DB
	river *river.Client[*sql.Tx]
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	if err := h.db.PingContext(r.Context()); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type greeting struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *handlers) listGreetings(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(),
		`select id, name, message, created_at from greetings order by id desc limit 100`)
	if err != nil {
		slog.ErrorContext(r.Context(), "List greetings", "err", err)
		api.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	out := []greeting{}
	for rows.Next() {
		var (
			g         greeting
			id        int64
			createdAt int64
		)
		if err := rows.Scan(&id, &g.Name, &g.Message, &createdAt); err != nil {
			slog.ErrorContext(r.Context(), "Scan greeting", "err", err)
			api.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		g.ID = api.FormatID(id)
		g.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(r.Context(), "List greetings", "err", err)
		api.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	api.WriteJSON(w, http.StatusOK, out)
}

func (h *handlers) createGreeting(w http.ResponseWriter, r *http.Request) {
	var req GreetArgs
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		api.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	res, err := h.river.Insert(r.Context(), req, nil)
	if err != nil {
		slog.ErrorContext(r.Context(), "Enqueue greet job", "err", err)
		api.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	api.WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": api.FormatID(res.Job.ID)})
}
