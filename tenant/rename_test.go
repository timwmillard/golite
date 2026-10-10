package tenant

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func count(t *testing.T, d *DBs, id string) int {
	t.Helper()
	var n int
	err := d.Do(t.Context(), id, func(db *sql.DB) error {
		return db.QueryRow(`select count(*) from things`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n
}

func TestRename(t *testing.T) {
	d := newDBs(t, Config{})
	db, release := acquire(t, d, "old")
	if _, err := db.Exec(`insert into things (id) values (1), (2)`); err != nil {
		t.Fatal(err)
	}
	release()

	committed := false
	err := d.Rename(t.Context(), "old", "new", func(context.Context) error {
		committed = true
		return nil
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if !committed {
		t.Error("commit not called")
	}
	if !isClosed(db) {
		t.Error("old DB handle still open")
	}
	if n := count(t, d, "new"); n != 2 {
		t.Errorf("new has %d rows, want the 2 written before the rename", n)
	}
	if _, _, err := d.Acquire(t.Context(), "old"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Acquire(old) after Rename = %v, want ErrNotExist", err)
	}
	old, _ := d.Path("old")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(old + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind", old+suffix)
		}
	}
}

func TestRename_Errors(t *testing.T) {
	d := newDBs(t, Config{})
	for _, id := range []string{"a", "b"} {
		if err := d.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.Rename(t.Context(), "a", "b", nil); !errors.Is(err, ErrExists) {
		t.Errorf("Rename onto an open database = %v, want ErrExists", err)
	}
	d.closeIdle(time.Now())
	if err := d.Rename(t.Context(), "a", "b", nil); !errors.Is(err, ErrExists) {
		t.Errorf("Rename onto an existing file = %v, want ErrExists", err)
	}
	if err := d.Rename(t.Context(), "missing", "c", nil); !errors.Is(err, ErrNotExist) {
		t.Errorf("Rename of a missing database = %v, want ErrNotExist", err)
	}
	if err := d.Rename(t.Context(), "a", "a", nil); err == nil {
		t.Error("Rename to itself succeeded")
	}
	if err := d.Rename(t.Context(), "a", "../x", nil); err == nil {
		t.Error("Rename to an invalid id succeeded")
	}

	// Nothing was left claimed.
	for _, id := range []string{"a", "b"} {
		_, release, err := d.Acquire(t.Context(), id)
		if err != nil {
			t.Fatalf("Acquire(%s) after failed Renames: %v", id, err)
		}
		release()
	}
	if s := d.Stats(); s.InUse != 0 {
		t.Errorf("Stats = %+v, want nothing in use", s)
	}
}

func TestRename_CommitFailsMovesBack(t *testing.T) {
	d := newDBs(t, Config{})
	db, release := acquire(t, d, "old")
	db.Exec(`insert into things (id) values (1)`)
	release()

	boom := errors.New("boom")
	if err := d.Rename(t.Context(), "old", "new", func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Rename = %v, want the commit error", err)
	}
	if n := count(t, d, "old"); n != 1 {
		t.Errorf("old has %d rows after a failed Rename, want 1", n)
	}
	if _, _, err := d.Acquire(t.Context(), "new"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Acquire(new) after a failed Rename = %v, want ErrNotExist", err)
	}
}

// If the process dies after moving the file but before commit, the next
// Rename finds the file already moved and just commits.
func TestRename_FinishesAfterCrash(t *testing.T) {
	d := newDBs(t, Config{})
	db, release := acquire(t, d, "old")
	db.Exec(`insert into things (id) values (1)`)
	release()
	d.closeIdle(time.Now())

	from, _ := d.Path("old")
	to, _ := d.Path("new")
	if err := os.Rename(from, to); err != nil { // the move that "crashed"
		t.Fatal(err)
	}

	committed := false
	if err := d.Rename(t.Context(), "old", "new", func(context.Context) error { committed = true; return nil }); err != nil {
		t.Fatalf("Rename after crash: %v", err)
	}
	if !committed {
		t.Error("commit not called")
	}
	if n := count(t, d, "new"); n != 1 {
		t.Errorf("new has %d rows, want 1", n)
	}
}

func TestRename_WaitsForUsers(t *testing.T) {
	d := newDBs(t, Config{})
	db, release := acquire(t, d, "old")

	renamed := make(chan error)
	go func() { renamed <- d.Rename(t.Context(), "old", "new", nil) }()

	// Until released, Rename waits and both names are refused.
	waitFor(t, func() bool {
		_, r, err := d.Acquire(t.Context(), "old")
		if err == nil {
			r() // Rename hasn't started yet
		}
		return errors.Is(err, ErrRenaming)
	})
	if _, _, err := d.Acquire(t.Context(), "new"); !errors.Is(err, ErrRenaming) {
		t.Errorf("Acquire(new) during Rename = %v, want ErrRenaming", err)
	}
	if _, err := db.Exec(`insert into things (id) values (1)`); err != nil {
		t.Fatalf("DB closed while still acquired: %v", err)
	}

	release()
	if err := <-renamed; err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if n := count(t, d, "new"); n != 1 {
		t.Errorf("new has %d rows, want the one written while Rename waited", n)
	}
}

func TestRename_GivesUpWithContext(t *testing.T) {
	d := newDBs(t, Config{})
	db, release := acquire(t, d, "old")
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := d.Rename(ctx, "old", "new", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Rename of in-use tenant = %v, want DeadlineExceeded", err)
	}
	if isClosed(db) {
		t.Error("DB closed by a Rename that gave up")
	}
	_, r, err := d.Acquire(t.Context(), "old")
	if err != nil {
		t.Fatalf("Acquire(old) after a Rename gave up: %v", err)
	}
	r()
	if _, _, err := d.Acquire(t.Context(), "new"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Acquire(new) = %v, want ErrNotExist", err)
	}
}
