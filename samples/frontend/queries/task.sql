-- name: ListTasks :many
select *
from task
order by id;

-- name: CreateTask :one
insert into task (title, created_at)
values (?, ?)
returning *;

-- name: ToggleTask :one
update task
set done = 1 - done
where id = ?
returning *;

-- name: DeleteTask :execrows
delete from task
where id = ?;
