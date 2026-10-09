package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

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
