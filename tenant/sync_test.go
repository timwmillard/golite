package tenant

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

var syncMigrations = fstest.MapFS{
	"0001_init.sql": {Data: []byte(`create table copy (key text primary key, value text not null)`)},
}

// fakeMaster stands in for the master database.
type fakeMaster struct {
	mu      sync.Mutex
	tenants map[string]bool
	values  map[string]string // key -> value; missing means deleted
}

func (m *fakeMaster) exists(_ context.Context, tenantID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tenants[tenantID], nil
}

func (m *fakeMaster) value(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	return v, ok
}

func (m *fakeMaster) set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
}

// copyMirror mirrors fakeMaster.values into the copy table. If pause is
// set, it's called between reading the master and writing the tenant.
func copyMirror(m *fakeMaster, pause func(key string)) Mirror {
	return Mirror{
		Name: "copy",
		Sync: func(ctx context.Context, tx *sql.Tx, tenantID, key string) error {
			v, ok := m.value(key)
			if pause != nil {
				pause(key)
			}
			if !ok {
				_, err := tx.ExecContext(ctx, `delete from copy where key = ?`, key)
				return err
			}
			_, err := tx.ExecContext(ctx, `insert into copy (key, value) values (?, ?)
				on conflict (key) do update set value = excluded.value`, key, v)
			return err
		},
	}
}

func newSync(t *testing.T, pause func(string)) (*Sync, *DBs, *fakeMaster) {
	t.Helper()
	d := newDBs(t, Config{Migrations: syncMigrations})
	m := &fakeMaster{tenants: map[string]bool{"acme": true}, values: map[string]string{}}
	s, err := NewSync(SyncConfig{DBs: d, Exists: m.exists, Mirrors: []Mirror{copyMirror(m, pause)}})
	if err != nil {
		t.Fatal(err)
	}
	return s, d, m
}

func runSync(s *Sync, args SyncArgs) error {
	return (&syncWorker{s: s}).Work(context.Background(), &river.Job[SyncArgs]{JobRow: &rivertype.JobRow{}, Args: args})
}

func copied(t *testing.T, d *DBs, key string) (string, bool) {
	t.Helper()
	var v string
	err := d.Do(t.Context(), "acme", func(db *sql.DB) error {
		return db.QueryRow(`select value from copy where key = ?`, key).Scan(&v)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, true
}

func TestSync_CreatesUpdatesDeletes(t *testing.T) {
	s, d, m := newSync(t, nil)
	args := SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}

	m.set("k", "v1")
	if err := runSync(s, args); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if v, _ := copied(t, d, "k"); v != "v1" {
		t.Errorf("after create, copy = %q, want v1", v)
	}

	m.set("k", "v2")
	if err := runSync(s, args); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if v, _ := copied(t, d, "k"); v != "v2" {
		t.Errorf("after update, copy = %q, want v2", v)
	}

	m.mu.Lock()
	delete(m.values, "k")
	m.mu.Unlock()
	if err := runSync(s, args); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok := copied(t, d, "k"); ok {
		t.Error("record deleted from master still in tenant")
	}
}

// Two syncs of the same tenant overlapping must not leave the older value:
// the second waits for the first, then reads the master afresh.
func TestSync_ConcurrentSyncsDontWriteStale(t *testing.T) {
	readV1 := make(chan struct{})
	resume := make(chan struct{})
	var first sync.Once
	pause := func(string) {
		first.Do(func() {
			close(readV1)
			<-resume
		})
	}
	s, d, m := newSync(t, pause)
	args := SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}

	m.set("k", "v1")
	errA := make(chan error)
	go func() { errA <- runSync(s, args) }()
	<-readV1 // A has read v1 and is paused before writing

	m.set("k", "v2") // the edit that enqueues B
	errB := make(chan error)
	go func() { errB <- runSync(s, args) }()
	time.Sleep(50 * time.Millisecond) // B would have read v2 and written by now, if it could

	close(resume)
	if err := <-errA; err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("sync B: %v", err)
	}
	if v, _ := copied(t, d, "k"); v != "v2" {
		t.Errorf("copy = %q after both syncs, want the latest, v2", v)
	}
}

func TestSync_TenantGone(t *testing.T) {
	s, d, m := newSync(t, nil)
	m.set("k", "v1")
	if err := runSync(s, SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	delete(m.tenants, "acme")
	m.mu.Unlock()

	err := runSync(s, SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"})
	var cancel *river.JobCancelError
	if !errors.As(err, &cancel) {
		t.Fatalf("sync of deleted tenant = %v, want JobCancel", err)
	}
	if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Acquire after sync of deleted tenant = %v, want ErrNotExist", err)
	}

	// Including when its database was already removed: no empty one left.
	if err := runSync(s, SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}); !errors.As(err, &cancel) {
		t.Fatalf("second sync of deleted tenant = %v, want JobCancel", err)
	}
	if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("sync recreated a deleted tenant's database: %v", err)
	}
}

func TestSync_UnknownMirror(t *testing.T) {
	s, _, _ := newSync(t, nil)
	err := runSync(s, SyncArgs{Mirror: "nope", Tenant: "acme"})
	var cancel *river.JobCancelError
	if !errors.As(err, &cancel) {
		t.Fatalf("sync with unknown mirror = %v, want JobCancel", err)
	}
}

func TestDeleteWorker(t *testing.T) {
	_, d, _ := newSync(t, nil)
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	err := (&deleteWorker{dbs: d}).Work(t.Context(), &river.Job[DeleteArgs]{JobRow: &rivertype.JobRow{}, Args: DeleteArgs{Tenant: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Acquire after delete job = %v, want ErrNotExist", err)
	}
}

func TestNewSync_Validates(t *testing.T) {
	d := newDBs(t, Config{})
	exists := func(context.Context, string) (bool, error) { return true, nil }
	noop := func(context.Context, *sql.Tx, string, string) error { return nil }

	bad := []SyncConfig{
		{Exists: exists},
		{DBs: d},
		{DBs: d, Exists: exists, Mirrors: []Mirror{{Name: "", Sync: noop}}},
		{DBs: d, Exists: exists, Mirrors: []Mirror{{Name: "a"}}},
		{DBs: d, Exists: exists, Mirrors: []Mirror{{Name: "a", Sync: noop}, {Name: "a", Sync: noop}}},
	}
	for i, cfg := range bad {
		if _, err := NewSync(cfg); err == nil {
			t.Errorf("config %d: NewSync succeeded, want error", i)
		}
	}

	s, err := NewSync(SyncConfig{DBs: d, Exists: exists, Mirrors: []Mirror{{Name: "a", Sync: noop}}})
	if err != nil {
		t.Fatal(err)
	}
	s.AddWorkers(river.NewWorkers()) // panics on a bad worker
}
