package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/master/model"
	"github.com/timwmillard/golite/samples/multitenant/mirror"
)

// TenantHandler implements the tenant operations of StrictServerInterface
// against the master database. Changes reach each tenant's own database
// through tenant.Sync jobs, inserted in the same transaction as the change.
type TenantHandler struct {
	master *sql.DB
	q      *model.Queries
	dbs    *tenant.DBs
	river  *river.Client[*sql.Tx]
}

func NewTenantHandler(master *sql.DB, dbs *tenant.DBs, riverClient *river.Client[*sql.Tx]) *TenantHandler {
	return &TenantHandler{master: master, q: model.New(master), dbs: dbs, river: riverClient}
}

// validSlug matches CreateTenantRequest.slug's pattern in the spec, which
// the generated server doesn't enforce.
var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (h *TenantHandler) ListTenants(ctx context.Context, request ListTenantsRequestObject) (ListTenantsResponseObject, error) {
	tenants, err := h.q.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}

	resp := make(ListTenants200JSONResponse, len(tenants))
	for i, t := range tenants {
		resp[i] = toAPITenant(t)
	}
	return resp, nil
}

func (h *TenantHandler) CreateTenant(ctx context.Context, request CreateTenantRequestObject) (CreateTenantResponseObject, error) {
	body := request.Body
	if !validSlug.MatchString(body.Slug) {
		return CreateTenant400JSONResponse{BadRequestJSONResponse{Error: "slug must be lowercase letters, digits and '-'"}}, nil
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return CreateTenant400JSONResponse{BadRequestJSONResponse{Error: "name is required"}}, nil
	}

	var t model.Tenant
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = h.q.WithTx(tx).CreateTenant(ctx, model.CreateTenantParams{
			Slug:      body.Slug,
			Name:      name,
			CreatedAt: time.Now().Unix(),
		})
		if err != nil {
			return err
		}
		return tenant.InsertSyncTx(ctx, h.river, tx, mirror.Tenant, conv.FormatID(t.ID), "")
	})
	if sqliteErr := (sqlite3.Error{}); errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
		return CreateTenant409JSONResponse{ConflictJSONResponse{Error: "tenant already exists"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create tenant: %w", err)
	}

	// The sync job creates the database too, but not until a worker picks
	// it up; create it now so the tenant is usable as soon as we respond.
	// If this fails the job still will, so the tenant was still created.
	if err := h.dbs.Create(ctx, conv.FormatID(t.ID)); err != nil {
		slog.WarnContext(ctx, "Create tenant database failed; left to tenant_sync", "tenant", t.Slug, "err", err)
	}

	return CreateTenant201JSONResponse(toAPITenant(t)), nil
}

func (h *TenantHandler) GetTenant(ctx context.Context, request GetTenantRequestObject) (GetTenantResponseObject, error) {
	t, err := h.q.GetTenantBySlug(ctx, request.Tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return GetTenant404JSONResponse{tenantNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}

	return GetTenant200JSONResponse(toAPITenant(t)), nil
}

func (h *TenantHandler) UpdateTenant(ctx context.Context, request UpdateTenantRequestObject) (UpdateTenantResponseObject, error) {
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return UpdateTenant400JSONResponse{BadRequestJSONResponse{Error: "name can't be empty"}}, nil
	}

	var t model.Tenant
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = h.q.WithTx(tx).UpdateTenant(ctx, model.UpdateTenantParams{Slug: request.Tenant, Name: name})
		if err != nil {
			return err
		}
		return tenant.InsertSyncTx(ctx, h.river, tx, mirror.Tenant, conv.FormatID(t.ID), "")
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateTenant404JSONResponse{tenantNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update tenant: %w", err)
	}

	return UpdateTenant200JSONResponse(toAPITenant(t)), nil
}

func (h *TenantHandler) DeleteTenant(ctx context.Context, request DeleteTenantRequestObject) (DeleteTenantResponseObject, error) {
	// Once the master row is gone no new request resolves to the tenant;
	// the job removes its database when the ones in flight finish.
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		t, err := h.q.WithTx(tx).DeleteTenant(ctx, request.Tenant)
		if err != nil {
			return err
		}
		return tenant.InsertDeleteTx(ctx, h.river, tx, conv.FormatID(t.ID))
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteTenant404JSONResponse{tenantNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete tenant: %w", err)
	}

	return DeleteTenant204Response{}, nil
}

// inTx runs fn in a master database transaction, committing if it returns
// nil.
func (h *TenantHandler) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := h.master.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

var tenantNotFound = NotFoundJSONResponse{Error: tenant.ErrNotFound.Error()}
