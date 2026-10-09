-- name: UpsertTenant :exec
insert into tenant (id, master_id, slug, name, synced_at)
values (1, ?, ?, ?, ?)
on conflict (id) do update
set
    master_id = excluded.master_id,
    slug = excluded.slug,
    name = excluded.name,
    synced_at = excluded.synced_at;

-- name: GetTenant :one
select *
from tenant
where id = 1;
