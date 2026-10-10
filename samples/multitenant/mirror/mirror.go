// Package mirror defines what of the master database is copied into each
// tenant's database, for tenant.Sync. Handlers insert a sync job, with
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

	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	"github.com/timwmillard/golite/samples/multitenant/tenantdb/model"
)

// Tenant mirrors a tenant's own master row into its database's one-row
// tenant table, so the database describes itself in a backup or export.
// Its sync jobs have no key: the record is the tenant itself.
const Tenant = "tenant"

// All returns every mirror, for tenant.SyncConfig.Mirrors.
func All(master *sql.DB) []tenant.Mirror {
	q := mastermodel.New(master)
	return []tenant.Mirror{
		{Name: Tenant, Sync: syncTenant(q)},
	}
}

// Exists reports whether a tenant is still in the master database, for
// tenant.SyncConfig.Exists.
func Exists(master *sql.DB) func(context.Context, string) (bool, error) {
	q := mastermodel.New(master)
	return func(ctx context.Context, tenantID string) (bool, error) {
		id, err := conv.ParseID(tenantID)
		if err != nil {
			return false, nil
		}
		_, err = q.GetTenant(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
}

func syncTenant(q *mastermodel.Queries) func(context.Context, *sql.Tx, string, string) error {
	return func(ctx context.Context, tx *sql.Tx, tenantID, _ string) error {
		id, err := conv.ParseID(tenantID)
		if err != nil {
			return err
		}
		t, err := q.GetTenant(ctx, id)
		if err != nil {
			// Including ErrNoRows: tenant.Sync checked it exists, so
			// it's just been deleted, and its delete job is coming.
			return fmt.Errorf("get tenant: %w", err)
		}

		return model.New(tx).UpsertTenant(ctx, model.UpsertTenantParams{
			MasterID: t.ID,
			Slug:     t.Slug,
			Name:     t.Name,
			SyncedAt: time.Now().Unix(),
		})
	}
}
