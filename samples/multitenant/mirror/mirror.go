// Package mirror defines what of the master database is copied into each
// company's database, for tenant.Sync. Handlers insert a sync job, with
// tenant.InsertSyncTx, in the same transaction as each change to a mirrored
// master record.
package mirror

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/companydb/model"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
)

// Company mirrors a company's own master row into its database's one-row
// company table, so the database describes itself in a backup or export.
// Its sync jobs have no key: the record is the company itself, and the
// ref is its id.
const Company = "company"

// All returns every mirror, for tenant.SyncConfig.Mirrors.
func All(master *sql.DB) []tenant.Mirror {
	q := mastermodel.New(master)
	return []tenant.Mirror{
		{Name: Company, Sync: syncCompany(q)},
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
			// Including ErrNoRows: tenant.Sync checked it's registered,
			// so it's just been deleted, and its settle job is coming.
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
