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

	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/master/model"
	"github.com/timwmillard/golite/samples/multitenant/mirror"
)

// CompanyHandler implements the company operations of StrictServerInterface
// against the master database. Each company's database is
// company_<slug>.db; changes reach it through tenant.Sync jobs, inserted in
// the same transaction as the change.
type CompanyHandler struct {
	master *sql.DB
	q      *model.Queries
	dbs    *tenant.DBs
	sync   *tenant.Sync
	river  *river.Client[*sql.Tx]

	// notFound matches the 404 tenant.Middleware sends for an unknown
	// company on the routes below it.
	notFound NotFoundJSONResponse
}

func NewCompanyHandler(master *sql.DB, dbs *tenant.DBs, sync *tenant.Sync, riverClient *river.Client[*sql.Tx]) *CompanyHandler {
	return &CompanyHandler{
		master:   master,
		q:        model.New(master),
		dbs:      dbs,
		sync:     sync,
		river:    riverClient,
		notFound: NotFoundJSONResponse{Error: dbs.Name() + " not found"},
	}
}

// validSlug matches the slug pattern in the spec, which the generated
// server doesn't enforce. It also keeps slugs valid as tenant database ids.
var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

const badSlug = "slug must be lowercase letters, digits and '-'"

func (h *CompanyHandler) ListCompanies(ctx context.Context, request ListCompaniesRequestObject) (ListCompaniesResponseObject, error) {
	companies, err := h.q.ListCompanies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list companies: %w", err)
	}

	resp := make(ListCompanies200JSONResponse, len(companies))
	for i, c := range companies {
		resp[i] = toAPICompany(c)
	}
	return resp, nil
}

func (h *CompanyHandler) CreateCompany(ctx context.Context, request CreateCompanyRequestObject) (CreateCompanyResponseObject, error) {
	body := request.Body
	if !validSlug.MatchString(body.Slug) {
		return CreateCompany400JSONResponse{BadRequestJSONResponse{Error: badSlug}}, nil
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return CreateCompany400JSONResponse{BadRequestJSONResponse{Error: "name is required"}}, nil
	}

	var c model.Company
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = h.q.WithTx(tx).CreateCompany(ctx, model.CreateCompanyParams{
			Slug:      body.Slug,
			Name:      name,
			CreatedAt: time.Now().Unix(),
		})
		if err != nil {
			return err
		}
		if err := tenant.CreateTx(ctx, tx, h.river, c.Slug); err != nil {
			return err
		}
		return tenant.SyncTx(ctx, tx, h.river, mirror.Company, c.Slug, "")
	})
	if isUnique(err) {
		return CreateCompany409JSONResponse{ConflictJSONResponse{Error: "company already exists"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create company: %w", err)
	}

	// Create the database and copy the company into it now, rather than
	// waiting for the jobs, which retry if this fails.
	if err := h.dbs.Create(ctx, c.Slug); err != nil {
		slog.WarnContext(ctx, "Creating company database failed; left to tenant_create", "company", c.Slug, "err", err)
	} else if err := h.sync.Run(ctx, mirror.Company, c.Slug, ""); err != nil {
		slog.WarnContext(ctx, "Syncing company database failed; left to tenant_sync", "company", c.Slug, "err", err)
	}
	return CreateCompany201JSONResponse(toAPICompany(c)), nil
}

func (h *CompanyHandler) GetCompany(ctx context.Context, request GetCompanyRequestObject) (GetCompanyResponseObject, error) {
	c, err := h.q.GetCompanyBySlug(ctx, request.Company)
	if errors.Is(err, sql.ErrNoRows) {
		return GetCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get company: %w", err)
	}

	return GetCompany200JSONResponse(toAPICompany(c)), nil
}

func (h *CompanyHandler) UpdateCompany(ctx context.Context, request UpdateCompanyRequestObject) (UpdateCompanyResponseObject, error) {
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return UpdateCompany400JSONResponse{BadRequestJSONResponse{Error: "name can't be empty"}}, nil
	}

	var c model.Company
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = h.q.WithTx(tx).UpdateCompany(ctx, model.UpdateCompanyParams{Slug: request.Company, Name: name})
		if err != nil {
			return err
		}
		return tenant.SyncTx(ctx, tx, h.river, mirror.Company, c.Slug, "")
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update company: %w", err)
	}

	return UpdateCompany200JSONResponse(toAPICompany(c)), nil
}

func (h *CompanyHandler) DeleteCompany(ctx context.Context, request DeleteCompanyRequestObject) (DeleteCompanyResponseObject, error) {
	c, err := h.q.GetCompanyBySlug(ctx, request.Company)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get company: %w", err)
	}

	// Move the database to the trash, once requests in flight on it
	// finish, then delete the master row. In that order, a new company
	// can't be given the slug while the old database is still under it.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = h.dbs.Delete(ctx, c.Slug, func(ctx context.Context) error {
		n, err := h.q.DeleteCompany(ctx, c.ID)
		if err == nil && n == 0 {
			err = sql.ErrNoRows // deleted meanwhile
		}
		return err
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DeleteCompany404JSONResponse{h.notFound}, nil
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, tenant.ErrDeleting):
		return DeleteCompany409JSONResponse{ConflictJSONResponse{Error: "company is busy; try again shortly"}}, nil
	case err != nil:
		return nil, fmt.Errorf("delete company: %w", err)
	}

	return DeleteCompany204Response{}, nil
}

// inTx runs fn in a master database transaction, committing if it returns
// nil.
func (h *CompanyHandler) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
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

func isUnique(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
}
