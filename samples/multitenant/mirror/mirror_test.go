package mirror

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	tenantmigrations "github.com/timwmillard/golite/samples/multitenant/tenantdb/migrations"
	"github.com/timwmillard/golite/samples/multitenant/tenantdb/model"
)

func TestTenant(t *testing.T) {
	dir := t.TempDir()
	master, err := server.OpenDB(t.Context(), filepath.Join(dir, "master.db"), mastermigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	dbs := tenant.New(tenant.Config{Dir: filepath.Join(dir, "tenants"), Migrations: tenantmigrations.FS, IdleTimeout: -1})
	defer dbs.Close()

	q := mastermodel.New(master)
	mt, err := q.CreateTenant(t.Context(), mastermodel.CreateTenantParams{Slug: "acme", Name: "Acme", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := conv.FormatID(mt.ID)
	if err := dbs.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	exists := Exists(master)
	if ok, err := exists(t.Context(), id); !ok || err != nil {
		t.Fatalf("Exists = %v, %v; want true", ok, err)
	}
	if ok, err := exists(t.Context(), "999"); ok || err != nil {
		t.Fatalf("Exists(unknown) = %v, %v; want false", ok, err)
	}

	sync := All(master)[0].Sync
	run := func() model.Tenant {
		t.Helper()
		var row model.Tenant
		err := dbs.Do(t.Context(), id, func(db *sql.DB) error {
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := sync(context.Background(), tx, id, ""); err != nil {
				return err
			}
			if row, err = model.New(tx).GetTenant(t.Context()); err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		return row
	}

	if got := run(); got.Name != "Acme" || got.Slug != "acme" || got.MasterID != mt.ID {
		t.Errorf("after first sync, tenant row = %+v", got)
	}
	if _, err := q.UpdateTenant(t.Context(), mastermodel.UpdateTenantParams{Slug: "acme", Name: "Acme Inc"}); err != nil {
		t.Fatal(err)
	}
	if got := run(); got.Name != "Acme Inc" {
		t.Errorf("after rename, tenant name = %q", got.Name)
	}
}
