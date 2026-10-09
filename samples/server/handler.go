package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
)

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
