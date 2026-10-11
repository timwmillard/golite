-- Each company's database is company_<slug>.db, so requests find it from
-- the slug in the URL without reading this table. The slug can only be
-- changed offline, with -rename-company, which renames the file too.
create table company (
    id         integer primary key autoincrement,
    slug       text    not null unique,
    name       text    not null,
    created_at integer not null
);
