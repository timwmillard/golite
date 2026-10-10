// Package tenant gives each tenant of a multi-tenant app its own SQLite
// database file. Databases are opened lazily, migrated when opened, and
// closed again when there are too many open or they've been idle, so an app
// can have thousands of tenants while only the active ones hold a file
// handle. Which tenants exist is the app's concern (typically a table in a
// master database); this package only manages their files.
//
//	dbs := tenant.New(tenant.Config{
//		Dir:        filepath.Join(dataDir, "tenants"),
//		Migrations: tenantmigrations.FS,
//	})
//	defer dbs.Close()
//
//	// Requests: put the tenant's *sql.DB in the context for tenant.DB.
//	mw := dbs.Middleware(func(r *http.Request) (string, error) {
//		return lookupTenantID(r) // e.g. from r.PathValue("tenant")
//	})
//
//	// When a tenant is added.
//	err := dbs.Create(ctx, id)
//
//	// Jobs and scripts: use the database for the length of fn.
//	err := dbs.Do(ctx, id, func(db *sql.DB) error { ... })
//
// A database is only closed when nothing is using it: every use goes
// through Acquire (or Do or Middleware, which call it) and holds the
// database until it's released.
package tenant

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/timwmillard/golite/server"
)

// Config configures DBs. Only Dir is required.
type Config struct {
	// Name is what the app calls a tenant, such as "company", "club" or
	// "account". It names the database files (<name>_<id>.db) and appears
	// in errors, including Middleware's 404 ("<name> not found"). Changing
	// it orphans existing files. Defaults to "tenant"; like an id, it may
	// only hold letters, digits, '-' and '_'.
	Name string

	// Dir holds the tenant databases, as <Name>_<id>.db. It's created if
	// needed.
	Dir string

	// Migrations, if set, are applied (see package migrate) each time a
	// tenant's database is opened, so a tenant closed for being idle picks
	// up new migrations when next used. Reopening costs well under a
	// millisecond when there's nothing to apply.
	Migrations fs.FS

	// MaxOpen is how many tenant databases may be open at once. Opening one
	// more closes the least recently used idle one. It's a soft limit: if
	// every open database is in use, the next is opened anyway and the
	// extra closed once released, rather than making requests wait.
	// Defaults to 256; each open database costs about three file
	// descriptors plus SQLite's page cache.
	MaxOpen int

	// IdleTimeout closes a database once it's gone unused this long.
	// Defaults to 10 minutes; negative disables it.
	IdleTimeout time.Duration
}

// ErrClosed is returned by Acquire after Close.
var ErrClosed = errors.New("tenant: DBs is closed")

// ErrNotExist is returned by Acquire for a tenant whose database hasn't been
// created.
var ErrNotExist = errors.New("tenant: database does not exist")

// ErrDeleting is returned by Acquire while the tenant is being deleted.
var ErrDeleting = errors.New("tenant: database is being deleted")

// validID is what an ID must look like to be used in a file name.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DBs opens, caches and closes one database per tenant ID.
type DBs struct {
	cfg Config

	mu      sync.Mutex
	entries map[string]*entry // open, opening or being deleted; nil once closed
	idle    *list.List        // entries with no users, most recently used first

	stop chan struct{} // closed by Close to stop the idle sweeper
	done chan struct{} // closed when the sweeper has stopped
}

// entry is one tenant database. Fields other than ready, db and err are
// guarded by DBs.mu; db and err are set once, before ready is closed.
type entry struct {
	id    string
	ready chan struct{}
	db    *sql.DB
	err   error

	users    int           // Acquires not yet released
	lastUsed time.Time     // when users last dropped to 0
	elem     *list.Element // position in DBs.idle while users == 0

	deleting bool
	unused   chan struct{} // closed when users drops to 0 during a Delete

	// cold marks a database MigrateAll opened just to migrate: it's closed
	// when released rather than kept as most recently used, unless a real
	// Acquire uses it meanwhile.
	cold bool
}

// mode is how acquire is being used.
type mode int

const (
	modeUse     mode = iota // Acquire: the database must exist
	modeCreate              // Create: create it if needed
	modeMigrate             // MigrateAll: create it if needed, don't keep it open
)

// New returns a DBs and, unless IdleTimeout is negative, starts a goroutine
// that closes idle databases; Close stops it. It panics if Name isn't
// valid.
func New(cfg Config) *DBs {
	if cfg.Name == "" {
		cfg.Name = "tenant"
	}
	if !validID.MatchString(cfg.Name) {
		panic(fmt.Sprintf("tenant: invalid Config.Name %q", cfg.Name))
	}
	if cfg.MaxOpen <= 0 {
		cfg.MaxOpen = 256
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}

	d := &DBs{
		cfg:     cfg,
		entries: map[string]*entry{},
		idle:    list.New(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if cfg.IdleTimeout > 0 {
		go d.sweep(cfg.IdleTimeout)
	} else {
		close(d.done)
	}
	return d
}

// Name returns Config.Name: what the app calls a tenant.
func (d *DBs) Name() string {
	return d.cfg.Name
}

// Path returns the file path of tenant id's database. It fails if id isn't
// 1-64 letters, digits, '-' or '_', so it's always a plain file in Dir.
func (d *DBs) Path(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("tenant: invalid id %q", id)
	}
	return filepath.Join(d.cfg.Dir, d.cfg.Name+"_"+id+".db"), nil
}

// Acquire returns tenant id's database, opening and migrating it if
// needed, and a release func to call when done with it. The database won't
// be closed before release is called, so call it, and don't use the
// database after. release may be called more than once.
//
// It fails with ErrNotExist if the database hasn't been made with Create,
// so a request that races a Delete can't bring back an empty database.
//
// Concurrent Acquires of the same tenant share one open; a slow open of
// one tenant doesn't block the others. A failed open is retried by the
// next Acquire.
func (d *DBs) Acquire(ctx context.Context, id string) (db *sql.DB, release func(), err error) {
	return d.acquire(ctx, id, modeUse)
}

// Create creates tenant id's database, if it doesn't exist, and migrates
// it. It leaves the database open but idle.
func (d *DBs) Create(ctx context.Context, id string) error {
	_, release, err := d.acquire(ctx, id, modeCreate)
	if err != nil {
		return err
	}
	release()
	return nil
}

// MigrateAll applies pending migrations to each tenant in ids, creating any
// database that doesn't exist yet. Tenants are opened only to be migrated
// and closed again, one at a time, so a run over thousands of tenants
// doesn't push the tenants in actual use out of the open set; tenants
// already open were migrated when they opened and are skipped.
//
// Tenants are migrated whenever they're opened anyway, so this isn't needed
// for correctness. It's for running in the background after a deploy, so
// tenants that aren't being used get new migrations, and a failing
// migration shows up then rather than on some tenant's first request days
// later. It keeps going past a tenant that fails, and returns all the
// failures; it stops early only if ctx is done, after finishing the tenant
// it's on.
func (d *DBs) MigrateAll(ctx context.Context, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		_, release, err := d.acquire(ctx, id, modeMigrate)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", d.cfg.Name, id, err))
			continue
		}
		release()
	}
	return errors.Join(errs...)
}

func (d *DBs) acquire(ctx context.Context, id string, mode mode) (db *sql.DB, release func(), err error) {
	path, err := d.Path(id)
	if err != nil {
		return nil, nil, err
	}

	d.mu.Lock()
	if d.entries == nil {
		d.mu.Unlock()
		return nil, nil, ErrClosed
	}
	e, ok := d.entries[id]
	if ok && e.deleting {
		d.mu.Unlock()
		return nil, nil, ErrDeleting
	}
	if ok && mode == modeMigrate {
		// Open (or being opened), so migrated by whoever opened it. Leave
		// its place in the idle list alone.
		d.mu.Unlock()
		return nil, func() {}, nil
	}
	if ok {
		d.use(e)
		e.cold = false
		d.mu.Unlock()

		select {
		case <-e.ready:
		case <-ctx.Done():
			d.release(e)
			return nil, nil, ctx.Err()
		}
		if e.err != nil {
			d.release(e)
			return nil, nil, e.err
		}
		return e.db, d.releaseFunc(e), nil
	}

	e = &entry{id: id, ready: make(chan struct{}), cold: mode == modeMigrate}
	d.entries[id] = e
	d.use(e)
	var evicted []*sql.DB
	if !e.cold {
		// A cold database is closed again straight after, so it goes
		// over MaxOpen briefly instead of closing one that's in use.
		evicted = d.evict()
	}
	d.mu.Unlock()
	closeAll(evicted)

	// Delete can't remove the file between this check and the open: it
	// waits for e's users, which include us.
	if _, err := os.Stat(path); mode == modeUse && errors.Is(err, fs.ErrNotExist) {
		e.err = ErrNotExist
	} else {
		// Detach from ctx so a cancelled request can't leave a
		// half-applied migration for the Acquires waiting on this open.
		e.db, e.err = server.OpenDB(context.WithoutCancel(ctx), path, d.cfg.Migrations)
	}
	if e.err != nil {
		// Forget it now, not when the last waiter releases, so the next
		// Acquire retries.
		d.mu.Lock()
		if d.entries[id] == e {
			delete(d.entries, id)
		}
		d.mu.Unlock()
	}
	close(e.ready)
	if e.err != nil {
		d.release(e)
		return nil, nil, e.err
	}
	return e.db, d.releaseFunc(e), nil
}

// Do calls fn with tenant id's database, which stays open until fn
// returns.
func (d *DBs) Do(ctx context.Context, id string, fn func(*sql.DB) error) error {
	db, release, err := d.Acquire(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	return fn(db)
}

// use records a new user of e. d.mu must be held.
func (d *DBs) use(e *entry) {
	e.users++
	if e.elem != nil {
		d.idle.Remove(e.elem)
		e.elem = nil
	}
}

func (d *DBs) releaseFunc(e *entry) func() {
	var once sync.Once
	return func() { once.Do(func() { d.release(e) }) }
}

// release drops a user of e. When the last goes, e becomes idle and any
// databases over MaxOpen are closed, or if e is cold, it's closed.
func (d *DBs) release(e *entry) {
	d.mu.Lock()
	e.users--
	if e.users > 0 {
		d.mu.Unlock()
		return
	}

	var evicted []*sql.DB
	switch {
	case e.deleting:
		close(e.unused)
	case d.entries[e.id] != e:
		// Failed to open, or closed: nothing to track.
	case e.cold:
		delete(d.entries, e.id)
		evicted = []*sql.DB{e.db}
	default:
		e.lastUsed = time.Now()
		e.elem = d.idle.PushFront(e)
		evicted = d.evict()
	}
	d.mu.Unlock()
	closeAll(evicted)
}

// evict removes least recently used idle entries until no more than
// MaxOpen are open, and returns their databases for the caller to close
// once d.mu is released. d.mu must be held.
func (d *DBs) evict() []*sql.DB {
	var evicted []*sql.DB
	for len(d.entries) > d.cfg.MaxOpen && d.idle.Len() > 0 {
		evicted = append(evicted, d.removeIdle(d.idle.Back()))
	}
	return evicted
}

// closeIdle closes databases unused since before cutoff.
func (d *DBs) closeIdle(cutoff time.Time) {
	d.mu.Lock()
	var evicted []*sql.DB
	for el := d.idle.Back(); el != nil && el.Value.(*entry).lastUsed.Before(cutoff); el = d.idle.Back() {
		evicted = append(evicted, d.removeIdle(el))
	}
	d.mu.Unlock()
	closeAll(evicted)
}

// removeIdle forgets the idle entry at el and returns its database. d.mu
// must be held.
func (d *DBs) removeIdle(el *list.Element) *sql.DB {
	e := d.idle.Remove(el).(*entry)
	e.elem = nil
	delete(d.entries, e.id)
	return e.db
}

func (d *DBs) sweep(timeout time.Duration) {
	defer close(d.done)
	t := time.NewTicker(max(timeout/4, time.Second))
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			d.closeIdle(now.Add(-timeout))
		case <-d.stop:
			return
		}
	}
}

func closeAll(dbs []*sql.DB) {
	for _, db := range dbs {
		// An error closing a database we're done with leaves nothing to
		// recover; SQLite has already committed its writes.
		_ = db.Close()
	}
}

// Delete closes tenant id's database and removes its file along with
// SQLite's -wal and -shm files. If the database is in use it first waits,
// until ctx is done, for the users to release it; meanwhile Acquire fails
// with ErrDeleting. Stop routing work to the tenant before deleting it, or
// a later Acquire will create a new, empty database.
func (d *DBs) Delete(ctx context.Context, id string) error {
	path, err := d.Path(id)
	if err != nil {
		return err
	}

	d.mu.Lock()
	if d.entries == nil {
		d.mu.Unlock()
		return ErrClosed
	}
	e, ok := d.entries[id]
	if ok && e.deleting {
		d.mu.Unlock()
		return ErrDeleting
	}
	if !ok {
		// Hold the id while the files go, so Acquire can't recreate them.
		e = &entry{id: id, ready: make(chan struct{})}
		close(e.ready)
		d.entries[id] = e
	}
	e.deleting = true
	e.unused = make(chan struct{})
	if e.users == 0 {
		close(e.unused)
	}
	if e.elem != nil {
		d.idle.Remove(e.elem)
		e.elem = nil
	}
	d.mu.Unlock()

	select {
	case <-e.unused:
	case <-ctx.Done():
		d.cancelDelete(e)
		return ctx.Err()
	}

	err = nil
	if e.db != nil {
		if cerr := e.db.Close(); cerr != nil {
			err = fmt.Errorf("close tenant database %s: %w", path, cerr)
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if rerr := os.Remove(path + suffix); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove tenant database: %w", rerr))
		}
	}

	d.mu.Lock()
	if d.entries != nil && d.entries[id] == e {
		delete(d.entries, id)
	}
	d.mu.Unlock()
	return err
}

// cancelDelete undoes Delete's claim on e when it gives up waiting.
func (d *DBs) cancelDelete(e *entry) {
	d.mu.Lock()
	e.deleting = false
	var evicted []*sql.DB
	if e.users == 0 && d.entries[e.id] == e {
		if e.db == nil {
			delete(d.entries, e.id) // Delete's placeholder
		} else {
			e.lastUsed = time.Now()
			e.elem = d.idle.PushFront(e)
			evicted = d.evict()
		}
	}
	d.mu.Unlock()
	closeAll(evicted)
}

// Stats describes the databases a DBs holds open.
type Stats struct {
	Open  int // open, opening or being deleted
	InUse int // acquired and not yet released
}

// Stats returns how many databases are open and in use.
func (d *DBs) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Stats{Open: len(d.entries), InUse: len(d.entries) - d.idle.Len()}
}

// Close stops the idle sweeper and closes every database, including ones
// still in use, so call it after the server and job workers have stopped.
// Acquire fails with ErrClosed after.
func (d *DBs) Close() error {
	d.mu.Lock()
	if d.entries == nil {
		d.mu.Unlock()
		return nil
	}
	entries := d.entries
	d.entries = nil
	d.idle.Init()
	close(d.stop)
	d.mu.Unlock()
	<-d.done

	var errs []error
	for _, e := range entries {
		<-e.ready
		if e.db != nil && !e.deleting {
			errs = append(errs, e.db.Close())
		}
	}
	return errors.Join(errs...)
}
