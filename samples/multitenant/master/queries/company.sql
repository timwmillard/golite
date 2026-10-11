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

-- name: CompanyExists :one
select exists (select 1 from company where slug = ?);

-- name: CreateCompany :one
insert into company (slug, name, created_at)
values (?, ?, ?)
returning *;

-- name: UpdateCompany :one
update company
set name = ?
where slug = ?
returning *;

-- name: DeleteCompany :execrows
delete from company
where id = ?;

-- name: UpdateSlug :execrows
update company
set slug = sqlc.arg(new_slug)
where slug = sqlc.arg(slug);
