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

-- name: CreateCompany :one
insert into company (slug, name, created_at)
values (?, ?, ?)
returning *;

-- name: UpdateCompany :one
update company
set name = ?
where slug = ?
returning *;

-- name: DeleteCompany :one
delete from company
where slug = ?
returning *;

-- name: ListCompanyIDs :many
select id
from company
order by id;
