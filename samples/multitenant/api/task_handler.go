package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/companydb/model"
)

// TaskHandler implements the task operations of StrictServerInterface
// against the company database that tenant.Middleware put in the request
// context. It holds no database itself.
type TaskHandler struct{}

// queries returns the queries for the request's company database.
func queries(ctx context.Context) *model.Queries {
	return model.New(tenant.DB(ctx))
}

func (h *TaskHandler) ListTasks(ctx context.Context, request ListTasksRequestObject) (ListTasksResponseObject, error) {
	var (
		tasks []model.Task
		err   error
	)
	if done := request.Params.Done; done != nil {
		tasks, err = queries(ctx).ListTasksByDone(ctx, conv.BoolInt(*done))
	} else {
		tasks, err = queries(ctx).ListTasks(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	resp := make(ListTasks200JSONResponse, len(tasks))
	for i, t := range tasks {
		resp[i] = toAPITask(t)
	}
	return resp, nil
}

func (h *TaskHandler) CreateTask(ctx context.Context, request CreateTaskRequestObject) (CreateTaskResponseObject, error) {
	body := request.Body
	title := strings.TrimSpace(body.Title)
	if title == "" {
		return CreateTask400JSONResponse{BadRequestJSONResponse{Error: "title is required"}}, nil
	}

	task, err := queries(ctx).CreateTask(ctx, model.CreateTaskParams{
		Title:     title,
		Notes:     conv.NullString(body.Notes),
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	return CreateTask201JSONResponse(toAPITask(task)), nil
}

func (h *TaskHandler) GetTask(ctx context.Context, request GetTaskRequestObject) (GetTaskResponseObject, error) {
	id, err := conv.ParseID(request.ID)
	if err != nil {
		return GetTask404JSONResponse{taskNotFound}, nil
	}

	task, err := queries(ctx).GetTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return GetTask404JSONResponse{taskNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}

	return GetTask200JSONResponse(toAPITask(task)), nil
}

func (h *TaskHandler) UpdateTask(ctx context.Context, request UpdateTaskRequestObject) (UpdateTaskResponseObject, error) {
	id, err := conv.ParseID(request.ID)
	if err != nil {
		return UpdateTask404JSONResponse{taskNotFound}, nil
	}
	body := request.Body

	// Read-modify-write in one transaction so concurrent PATCHes of
	// different fields don't overwrite each other.
	tx, err := tenant.DB(ctx).BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	q := model.New(tx)

	task, err := q.GetTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateTask404JSONResponse{taskNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}

	params := model.UpdateTaskParams{
		ID:          task.ID,
		Title:       task.Title,
		Notes:       task.Notes,
		Done:        task.Done,
		CompletedAt: task.CompletedAt,
	}
	if body.Title != nil {
		params.Title = strings.TrimSpace(*body.Title)
		if params.Title == "" {
			return UpdateTask400JSONResponse{BadRequestJSONResponse{Error: "title can't be empty"}}, nil
		}
	}
	if body.Notes != nil {
		params.Notes = sql.NullString{String: *body.Notes, Valid: *body.Notes != ""}
	}
	if body.Done != nil && conv.BoolInt(*body.Done) != task.Done {
		params.Done = conv.BoolInt(*body.Done)
		params.CompletedAt = sql.NullInt64{}
		if *body.Done {
			now := time.Now()
			params.CompletedAt = conv.NullUnix(&now)
		}
	}

	task, err = q.UpdateTask(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return UpdateTask200JSONResponse(toAPITask(task)), nil
}

func (h *TaskHandler) DeleteTask(ctx context.Context, request DeleteTaskRequestObject) (DeleteTaskResponseObject, error) {
	id, err := conv.ParseID(request.ID)
	if err != nil {
		return DeleteTask404JSONResponse{taskNotFound}, nil
	}

	n, err := queries(ctx).DeleteTask(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("delete task: %w", err)
	}
	if n == 0 {
		return DeleteTask404JSONResponse{taskNotFound}, nil
	}

	return DeleteTask204Response{}, nil
}

var taskNotFound = NotFoundJSONResponse{Error: "task not found"}
