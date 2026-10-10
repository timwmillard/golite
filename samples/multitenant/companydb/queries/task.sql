-- name: ListTasks :many
select *
from task
order by id;

-- name: ListTasksByDone :many
select *
from task
where done = ?
order by id;

-- name: GetTask :one
select *
from task
where id = ?;

-- name: CreateTask :one
insert into task (title, notes, created_at)
values (?, ?, ?)
returning *;

-- name: UpdateTask :one
update task
set
    title = ?,
    notes = ?,
    done = ?,
    completed_at = ?
where id = ?
returning *;

-- name: DeleteTask :execrows
delete from task
where id = ?;
