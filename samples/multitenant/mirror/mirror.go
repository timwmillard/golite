// Package mirror defines what of the master database is copied into each
// company's database, for tenant.Sync. Handlers insert a sync job, with
// tenant.InsertSyncTx, in the same transaction as each change to a mirrored
// master record.
package mirror

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/companydb/model"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
)

// Company mirrors a company's own master row into its database's one-row
// company table, so the database describes itself in a backup or export.
// Its sync jobs have no key: the record is the company itself.
const Company = "company"

// All returns every mirror, for tenant.SyncConfig.Mirrors.
func All(master *sql.DB) []tenant.Mirror {
	q := mastermodel.New(master)
	return []tenant.Mirror{
		{Name: Company, Sync: syncCompany(q)},
	}
}

// Exists reports whether a company is still in the master database, for
// tenant.SyncConfig.Exists.
func Exists(master *sql.DB) func(context.Context, string) (bool, error) {
	q := mastermodel.New(master)
	return func(ctx context.Context, companyID string) (bool, error) {
		id, err := conv.ParseID(companyID)
		if err != nil {
			return false, nil
		}
		_, err = q.GetCompany(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
}

func syncCompany(q *mastermodel.Queries) func(context.Context, *sql.Tx, string, string) error {
	return func(ctx context.Context, tx *sql.Tx, companyID, _ string) error {
		id, err := conv.ParseID(companyID)
		if err != nil {
			return err
		}
		t, err := q.GetCompany(ctx, id)
		if err != nil {
			// Including ErrNoRows: tenant.Sync checked it exists, so
			// it's just been deleted, and its delete job is coming.
			return fmt.Errorf("get company: %w", err)
		}

		return model.New(tx).UpsertCompany(ctx, model.UpsertCompanyParams{
			MasterID: t.ID,
			Slug:     t.Slug,
			Name:     t.Name,
			SyncedAt: time.Now().Unix(),
		})
	}
}
