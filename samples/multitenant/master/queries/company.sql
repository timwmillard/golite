-- name: ListCompanies :many
select *
from company
order by slug;

-- name: GetCompany :one
select *
from company
where id = ?;

-- name: GetCompanyBySlug :one
select *
from company
where slug = ?;

-- name: GetCompanyByDBKey :one
select *
from company
where db_key = ?;

-- name: DBKeyTaken :one
select exists (select 1 from company where db_key = ?);

-- name: CreateCompany :one
insert into company (slug, db_key, name, created_at)
values (?, ?, ?, ?)
returning *;

-- name: UpdateCompany :one
update company
set name = ?
where slug = ?
returning *;

-- name: DeleteCompany :execrows
delete from company
where id = ?;

-- name: UpdateSlug :one
update company
set slug = sqlc.arg(new_slug)
where slug = sqlc.arg(slug)
returning *;

-- name: RenameDBKey :execrows
update company
set db_key = sqlc.arg(new_db_key)
where db_key = sqlc.arg(db_key);
