package app

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"

	"github.com/timwmillard/golite/samples/frontend/model"
)

// taskHandler serves the task list page, and the fragments htmx swaps
// into it as tasks are added, toggled and deleted.
type taskHandler struct {
	q *model.Queries
}

func (h *taskHandler) index(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.q.ListTasks(r.Context())
	if err != nil {
		server.ResponseError(w, r, fmt.Errorf("list tasks: %w", err))
		return
	}
	render(w, r, http.StatusOK, indexPage(tasks))
}

// create appends the new task to the list (the form's hx-target), or puts
// an error under the form.
func (h *taskHandler) create(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		w.Header().Set("HX-Retarget", "#form-error")
		w.Header().Set("HX-Reswap", "outerHTML")
		render(w, r, http.StatusUnprocessableEntity, formError("Title is required", false))
		return
	}

	task, err := h.q.CreateTask(r.Context(), model.CreateTaskParams{
		Title:     title,
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		server.ResponseError(w, r, fmt.Errorf("create task: %w", err))
		return
	}
	render(w, r, http.StatusOK, createdTask(task))
}

// toggle replaces the task's row with its new state.
func (h *taskHandler) toggle(w http.ResponseWriter, r *http.Request) {
	id, err := conv.ParseID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	task, err := h.q.ToggleTask(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		server.ResponseError(w, r, fmt.Errorf("toggle task: %w", err))
		return
	}
	render(w, r, http.StatusOK, taskItem(task))
}

// delete responds with nothing, which htmx swaps in for the task's row.
func (h *taskHandler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := conv.ParseID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	n, err := h.q.DeleteTask(r.Context(), id)
	if err != nil {
		server.ResponseError(w, r, fmt.Errorf("delete task: %w", err))
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// render writes c as an HTML response. It renders into a buffer first so
// a failure part way through can still become a 500.
func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		server.ResponseError(w, r, fmt.Errorf("render: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
