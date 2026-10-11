# Tenant databases

Each tenant gets its own SQLite file, `<Config.Name>_<id>.db`, in one
folder:

```
companies/
  company_acme.db
  company_mybusiness.db
  .trash/
    20261011T013733.256543000Z_company_oldco.db
```

or, for an app with thousands of tenants:

```
firms/
  firm_e505a392-54f0-434a-a253-df54e17f8c38.db
  firm_a6650b16-3d8e-44d8-b993-964aeda61895.db
```

The **id** is the app's choice: a unique value of the tenant's that never
changes while the app runs, such as a UUID column or a slug (the
multitenant sample uses the company's slug). It may be 1-64 letters,
digits, `-` and `_`. It doesn't have to be the master table's integer id,
but it **must be the one in the tenant's URLs** (see Requests).

There's no registry table. The id itself is the link between the master
database and the file, so jobs, requests and `MigrateAll` (which lists the
folder) all find a tenant's database the same way.

## Requests

Separate databases exist for performance. SQLite allows one writer per
file, so with one shared database every tenant would wait on every other
tenant's writes. With one file each, tenants don't block each other. The
master database is only written to for onboarding and tenant changes, which
are rare.

That only holds if tenant requests **never touch the master database**.
Master is opened with one connection, so a single master read per request
would put every tenant's requests back in one queue, behind River's polling
and job writes too. So:

- The Resolver maps the request to the id from the request alone, e.g.
  `r.PathValue("company")`, after checking its format. That's why the id in
  the URL must be the file name.
- Whether the tenant exists is answered by its file. `Middleware` answers
  404 if there's no file, or if it's being deleted (deletes move the file
  away before removing the master row, and creates make it right after the
  master commit).
- Auth is the app's call. A global login that spans tenants naturally
  lives in master, and users tied to one tenant can live in its own
  database. golite doesn't need either.
- Handlers use only `tenant.DB(ctx)`. Anything they need from master is
  mirrored into the tenant database by sync jobs.

Only management routes (create, update or delete a tenant) and background
jobs read master.

The folder can be replicated as is, e.g. with Litestream, and copied down
read-only for analysis. Each database can also describe itself: the
sample mirrors the company's master row into a one-row `company` table.

## Lifecycle

| Step | How |
|---|---|
| Create | In the master transaction that adds the tenant, `tenant.CreateTx(ctx, tx, river, id)` and a `SyncTx` for each record to copy in; after commit, `dbs.Create(ctx, id)` then `sync.Run(...)` for those records. The jobs do both if those calls fail. Only `Create` makes files. Requests, syncs and `MigrateAll` get `ErrNotExist` otherwise. |
| Sync | `tenant.SyncTx(ctx, tx, river, mirror, id, key)` in the master transaction that changes a mirrored record. To have it done before responding, also `sync.Run(ctx, mirror, id, key)` after commit. |
| Delete | `dbs.Delete(ctx, id, commit)`, where `commit` deletes the master row. |
| Rename | Offline only: `dbs.Rename(ctx, from, to, commit)`, with the app and Litestream stopped. |
| Clean up | `dbs.PurgeTrash(age)` removes trashed databases older than `age`. |
| Upkeep | `go dbs.Maintain(ctx, trashAge)` at startup: `PurgeTrash`, then `MigrateAll`. `dbs.Close` stops it and waits; it then returns `ErrClosed`. |

## Delete

`Delete`:

1. Claims the id, so `Acquire` fails with `ErrDeleting` (404 from the
   middleware).
2. Waits, until ctx is done, for requests and jobs using the database to
   release it.
3. Closes the database and moves the `.db`, `-wal` and `-shm` files into
   `.trash/`, all under one timestamped name, so the trashed copy still
   opens as a database.
4. Calls `commit` to delete the master row. If that fails, it moves the
   files back.

The order matters when ids can be reused, e.g. a slug. The master row,
whose unique constraint reserves the id, only goes once the old data has
left the file name. So a new tenant given the same id always starts with
an empty database. A tenant with no file still gets `commit`, so a delete
that failed partway can just be run again.

Restoring a deleted tenant means moving its files back out of `.trash/`,
dropping the timestamp prefix, and restoring its master row.

## Rename (offline)

Renames are rare, so they aren't supported while the app is running.
Litestream holds a read transaction on every database, so the WAL can't be
checkpointed away for a clean single-file move, and a renamed file starts
a new replica path in S3 anyway.

```sh
# stop the app and Litestream, then:
multitenantserver -rename-company acme:acme-inc
```

In the sample this changes the company's slug: the slug is the file name,
so it's the one thing about a company that can't change online.

`Rename` refuses a database in use (`ErrInUse`), or one with a non-empty
WAL, which means another process has it open. It moves the file, calls
`commit` to update the master record, and moves the file back if `commit`
fails. If it crashes after the move, running the same rename again sees
the file already at the new name and just commits.

## Jobs

| Job | Does | When the tenant's not in master (`SyncConfig.Exists`) |
|---|---|---|
| `tenant_create` | `Create`. | Cancels. If the tenant went between the check and the create, puts the new empty file in the trash. |
| `tenant_sync` | `Sync.Run`: opens the database, begins a transaction, checks `Exists`, runs the mirror and commits. | Cancels. |

A sync that finds no file while the tenant is in master retries: the
create job hasn't run yet. (`Sync.Run` returns `ErrNotExist` then, and
`ErrNotFound` if the tenant isn't in master.) A sync during
a delete snoozes for a second. Syncs of one tenant take turns on its
single connection, and `Delete` waits for a running one, so a sync can't
write into a database that's being trashed.

## Rules for apps

- Where the app calls `Acquire` or `Do` itself, treat `tenant.IsNotFound(err)`
  as "no such tenant", as `Middleware` does.
- In tests that insert `CreateTx`/`SyncTx` jobs without working them,
  register `tenant.AddStubWorkers`; test mirrors with `Sync.Run`.
- Insert `CreateTx` and `SyncTx` jobs in the same master transaction as
  the change they belong to.
- Delete tenants only with `dbs.Delete` and a `commit` that removes the
  master row. Don't delete the row first.
- Never use a tenant database while holding a master transaction. Syncs
  hold a tenant database while reading master, and with one connection to
  each, the opposite order could deadlock.
- Don't change a tenant's id while the app is running.
