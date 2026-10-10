package mirror

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	companymigrations "github.com/timwmillard/golite/samples/multitenant/companydb/migrations"
	"github.com/timwmillard/golite/samples/multitenant/companydb/model"
	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
)

func TestCompany(t *testing.T) {
	dir := t.TempDir()
	master, err := server.OpenDB(t.Context(), filepath.Join(dir, "master.db"), mastermigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	dbs := tenant.New(tenant.Config{Name: "company", Dir: filepath.Join(dir, "companies"), Migrations: companymigrations.FS, IdleTimeout: -1})
	defer dbs.Close()

	q := mastermodel.New(master)
	mt, err := q.CreateCompany(t.Context(), mastermodel.CreateCompanyParams{Slug: "acme", Name: "Acme", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := conv.FormatID(mt.ID)
	if err := dbs.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	sync := All(master)[0].Sync
	run := func() model.Company {
		t.Helper()
		var row model.Company
		err := dbs.Do(t.Context(), id, func(db *sql.DB) error {
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := sync(context.Background(), tx, id, ""); err != nil {
				return err
			}
			if row, err = model.New(tx).GetCompany(t.Context()); err != nil {
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
		t.Errorf("after first sync, company row = %+v", got)
	}
	if _, err := q.UpdateCompany(t.Context(), mastermodel.UpdateCompanyParams{Slug: "acme", Name: "Acme Inc"}); err != nil {
		t.Fatal(err)
	}
	if got := run(); got.Name != "Acme Inc" {
		t.Errorf("after rename, company name = %q", got.Name)
	}
}
