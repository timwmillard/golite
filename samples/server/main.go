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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/samples/server/migrations"
	"github.com/timwmillard/golite/server"
)

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

	db, err := server.OpenDB(ctx, filepath.Join(*dataDir, "app.db"), migrations.FS)
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

	// GET /health is registered by server.New since cfg.DB is set.
	h := &handlers{db: db, river: srv.River}
	srv.Mux.HandleFunc("GET /api/greetings", h.listGreetings)
	srv.Mux.HandleFunc("POST /api/greetings", h.createGreeting)

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
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
		server.ResponseError(w, r, fmt.Errorf("list greetings: %w", err))
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
			server.ResponseError(w, r, fmt.Errorf("scan greeting: %w", err))
			return
		}
		g.ID = conv.FormatID(id)
		g.CreatedAt = conv.Unix(createdAt)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		server.ResponseError(w, r, fmt.Errorf("list greetings: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) createGreeting(w http.ResponseWriter, r *http.Request) {
	var req GreetArgs
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		server.RequestError(w, r, errors.New("invalid JSON body"))
		return
	}
	if req.Name == "" {
		server.RequestError(w, r, errors.New("name is required"))
		return
	}

	res, err := h.river.Insert(r.Context(), req, nil)
	if err != nil {
		server.ResponseError(w, r, fmt.Errorf("enqueue greet job: %w", err))
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": conv.FormatID(res.Job.ID)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
