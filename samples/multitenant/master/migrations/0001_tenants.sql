-- autoincrement so a deleted tenant's id, and so its database file name,
-- is never reused for a new tenant.
create table tenant (
    id         integer primary key autoincrement,
    slug       text    not null unique,
    name       text    not null,
    created_at integer not null
);
