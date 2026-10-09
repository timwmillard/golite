// Package tenantsync keeps each tenant's database in step with the master
// database, using River jobs on the master database as an outbox: a handler
// changes a tenant's master row and inserts the job in one transaction, so
// the job exists if and only if the change committed, and River retries it
// until the tenant database matches. A crash between the master commit and
// the tenant write can't lose the write.
package tenantsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/tenant"

	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	"github.com/timwmillard/golite/samples/multitenant/tenantdb/model"
)

// SyncArgs copies a tenant's master row into its database, creating the
// database if needed. Insert one whenever the row changes.
//
// It isn't a unique job: River can't dedupe without counting running jobs,
// and an edit made while a sync is running would then be dropped. Each sync
// re-reads the latest master row, so extras are cheap and harmless.
type SyncArgs struct {
	TenantID int64 `json:"tenant_id"`
}

func (SyncArgs) Kind() string { return "tenant_sync" }

// DeleteArgs removes a deleted tenant's database. Insert it in the
// transaction that deletes the master row.
type DeleteArgs struct {
	TenantID int64 `json:"tenant_id"`
}

func (DeleteArgs) Kind() string { return "tenant_delete" }

// AddWorkers registers the workers for SyncArgs and DeleteArgs.
func AddWorkers(workers *river.Workers, master *sql.DB, dbs *tenant.DBs) {
	river.AddWorker(workers, &SyncWorker{Master: master, Tenants: dbs})
	river.AddWorker(workers, &DeleteWorker{Tenants: dbs})
}

type SyncWorker struct {
	river.WorkerDefaults[SyncArgs]

	Master  *sql.DB
	Tenants *tenant.DBs
}

func (w *SyncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	id := conv.FormatID(job.Args.TenantID)
	if err := w.Tenants.Create(ctx, id); err != nil {
		return err
	}

	// Read the master row while holding the tenant database, so a
	// DeleteWorker for the tenant either waits for this to finish, or ran
	// first and we see the row gone below.
	var gone bool
	err := w.Tenants.Do(ctx, id, func(db *sql.DB) error {
		t, err := mastermodel.New(w.Master).GetTenant(ctx, job.Args.TenantID)
		if errors.Is(err, sql.ErrNoRows) {
			gone = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("get tenant: %w", err)
		}

		return model.New(db).UpsertTenant(ctx, model.UpsertTenantParams{
			MasterID: t.ID,
			Slug:     t.Slug,
			Name:     t.Name,
			SyncedAt: time.Now().Unix(),
		})
	})
	if err != nil {
		return err
	}

	if gone {
		// Deleted before (or while) we ran, and Create above may have
		// recreated its database: remove it again.
		if err := w.Tenants.Delete(ctx, id); err != nil {
			return err
		}
		return river.JobCancel(fmt.Errorf("tenant %d no longer exists", job.Args.TenantID))
	}
	return nil
}

type DeleteWorker struct {
	river.WorkerDefaults[DeleteArgs]

	Tenants *tenant.DBs
}

// Work waits for in-flight requests on the tenant to finish (they resolved
// it before its master row went), then removes its database. If they take
// longer than the job's timeout, River retries.
func (w *DeleteWorker) Work(ctx context.Context, job *river.Job[DeleteArgs]) error {
	return w.Tenants.Delete(ctx, conv.FormatID(job.Args.TenantID))
}
