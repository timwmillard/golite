create table task (
    id         integer primary key,
    title      text    not null,
    done       integer not null default 0,
    created_at integer not null
);
