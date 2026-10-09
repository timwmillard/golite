-- name: ListTenants :many
select *
from tenant
order by slug;

-- name: GetTenant :one
select *
from tenant
where id = ?;

-- name: GetTenantBySlug :one
select *
from tenant
where slug = ?;

-- name: CreateTenant :one
insert into tenant (slug, name, created_at)
values (?, ?, ?)
returning *;

-- name: UpdateTenant :one
update tenant
set name = ?
where slug = ?
returning *;

-- name: DeleteTenant :one
delete from tenant
where slug = ?
returning *;
