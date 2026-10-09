package tenantsync

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	tenantmigrations "github.com/timwmillard/golite/samples/multitenant/tenantdb/migrations"
	"github.com/timwmillard/golite/samples/multitenant/tenantdb/model"
)

func setup(t *testing.T) (*sql.DB, *tenant.DBs) {
	t.Helper()
	dir := t.TempDir()
	master, err := server.OpenDB(t.Context(), filepath.Join(dir, "master.db"), mastermigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	dbs := tenant.New(tenant.Config{Dir: filepath.Join(dir, "tenants"), Migrations: tenantmigrations.FS, IdleTimeout: -1})
	t.Cleanup(func() { dbs.Close() })
	return master, dbs
}

func syncJob(id int64) *river.Job[SyncArgs] {
	return &river.Job[SyncArgs]{JobRow: &rivertype.JobRow{}, Args: SyncArgs{TenantID: id}}
}

func tenantRow(t *testing.T, dbs *tenant.DBs, id int64) model.Tenant {
	t.Helper()
	var row model.Tenant
	err := dbs.Do(t.Context(), conv.FormatID(id), func(db *sql.DB) error {
		var err error
		row, err = model.New(db).GetTenant(t.Context())
		return err
	})
	if err != nil {
		t.Fatalf("read tenant row: %v", err)
	}
	return row
}

func TestSync_CreatesAndUpdates(t *testing.T) {
	master, dbs := setup(t)
	q := mastermodel.New(master)
	w := &SyncWorker{Master: master, Tenants: dbs}

	mt, err := q.CreateTenant(t.Context(), mastermodel.CreateTenantParams{Slug: "acme", Name: "Acme", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Work(t.Context(), syncJob(mt.ID)); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if got := tenantRow(t, dbs, mt.ID); got.Name != "Acme" || got.Slug != "acme" || got.MasterID != mt.ID {
		t.Errorf("after first sync, tenant row = %+v", got)
	}

	if _, err := q.UpdateTenant(t.Context(), mastermodel.UpdateTenantParams{Slug: "acme", Name: "Acme Inc"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Work(t.Context(), syncJob(mt.ID)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := tenantRow(t, dbs, mt.ID); got.Name != "Acme Inc" {
		t.Errorf("after rename, tenant name = %q", got.Name)
	}
}

// A sync that runs after its tenant was deleted (and its database removed)
// mustn't leave a new, empty database behind.
func TestSync_AfterDelete(t *testing.T) {
	master, dbs := setup(t)
	q := mastermodel.New(master)

	mt, err := q.CreateTenant(t.Context(), mastermodel.CreateTenantParams{Slug: "acme", Name: "Acme", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.DeleteTenant(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	id := conv.FormatID(mt.ID)
	if err := (&DeleteWorker{Tenants: dbs}).Work(t.Context(), &river.Job[DeleteArgs]{JobRow: &rivertype.JobRow{}, Args: DeleteArgs{TenantID: mt.ID}}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	err = (&SyncWorker{Master: master, Tenants: dbs}).Work(t.Context(), syncJob(mt.ID))
	var cancel *river.JobCancelError
	if !errors.As(err, &cancel) {
		t.Fatalf("sync of deleted tenant = %v, want JobCancel", err)
	}
	if _, _, err := dbs.Acquire(t.Context(), id); !errors.Is(err, tenant.ErrNotExist) {
		t.Errorf("Acquire after sync of deleted tenant = %v, want ErrNotExist", err)
	}
}
