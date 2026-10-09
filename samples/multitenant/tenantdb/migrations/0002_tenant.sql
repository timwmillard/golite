-- The tenant's own copy of its master row, kept up to date by the
-- tenantsync jobs, so its database describes itself in a backup or export.
-- There's only ever one row.
create table tenant (
    id        integer primary key check (id = 1),
    master_id integer not null,
    slug      text    not null,
    name      text    not null,
    synced_at integer not null
);
