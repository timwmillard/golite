-- Each company's database is company_<db_key>.db. db_key is the slug the
-- company was created with, and doesn't change when the slug does, so the
-- file keeps its name; renaming it is done offline, with -rename-company.
create table company (
    id         integer primary key autoincrement,
    slug       text    not null unique,
    db_key     text    not null unique,
    name       text    not null,
    created_at integer not null
);
