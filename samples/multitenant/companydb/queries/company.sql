-- name: UpsertCompany :exec
insert into company (id, master_id, slug, name, synced_at)
values (1, ?, ?, ?, ?)
on conflict (id) do update
set
    master_id = excluded.master_id,
    slug = excluded.slug,
    name = excluded.name,
    synced_at = excluded.synced_at;

-- name: GetCompany :one
select *
from company
where id = 1;
