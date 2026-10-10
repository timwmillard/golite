-- autoincrement so a deleted company's id, and so its database file name,
-- is never reused for a new company.
create table company (
    id         integer primary key autoincrement,
    slug       text    not null unique,
    name       text    not null,
    created_at integer not null
);
