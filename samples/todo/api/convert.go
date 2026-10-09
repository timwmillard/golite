package api

import (
	"database/sql"
	"time"

	golite "github.com/timwmillard/golite/api"

	"github.com/timwmillard/golite/samples/todo/db/model"
)

func toAPITask(t model.Task) Task {
	return Task{
		ID:          golite.FormatID(t.ID),
		Title:       t.Title,
		Notes:       golite.NullStringPtr(t.Notes),
		Done:        t.Done != 0,
		CreatedAt:   time.Unix(t.CreatedAt, 0).UTC(),
		CompletedAt: unixPtr(t.CompletedAt),
	}
}

// boolInt converts a bool to SQLite's 0/1 integer.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// unixPtr reads a nullable epoch-seconds column as a time, nil when NULL.
func unixPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}
