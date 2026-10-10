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

// CompanyHandler implements the company operations of StrictServerInterface
// against the master database. Each company's database is named after its
// slug and tracked in tenant.Sync's registry under the company's id, which
// never changes; changes reach the database through tenant.Sync jobs,
// inserted in the same transaction as the change.
type CompanyHandler struct {
	master *sql.DB
	q      *model.Queries
	sync   *tenant.Sync
	river  *river.Client[*sql.Tx]

	// notFound matches the 404 tenant.Middleware sends for an unknown
	// company on the routes below it.
	notFound NotFoundJSONResponse
}

func NewCompanyHandler(master *sql.DB, dbs *tenant.DBs, companySync *tenant.Sync, riverClient *river.Client[*sql.Tx]) *CompanyHandler {
	return &CompanyHandler{
		master:   master,
		q:        model.New(master),
		sync:     companySync,
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
		ref := conv.FormatID(c.ID)
		if err := tenant.CreateTx(ctx, h.river, tx, ref, c.Slug); err != nil {
			return err
		}
		return tenant.SyncTx(ctx, h.river, tx, mirror.Company, ref, "")
	})
	if isUnique(err) {
		return CreateCompany409JSONResponse{ConflictJSONResponse{Error: "company already exists"}}, nil
	}
	if errors.Is(err, tenant.ErrIDTaken) {
		return CreateCompany409JSONResponse{ConflictJSONResponse{Error: "slug was in use until recently; try again shortly"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create company: %w", err)
	}

	h.settle(ctx, c)
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
		return tenant.SyncTx(ctx, h.river, tx, mirror.Company, conv.FormatID(c.ID), "")
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update company: %w", err)
	}

	return UpdateCompany200JSONResponse(toAPICompany(c)), nil
}

// UpdateSlug changes a company's slug, and so the name of its database
// file. The new slug works straight away: requests find the database
// through the registry, which keeps the old file name until the rename.
func (h *CompanyHandler) UpdateSlug(ctx context.Context, request UpdateSlugRequestObject) (UpdateSlugResponseObject, error) {
	slug := request.Body.Slug
	if !validSlug.MatchString(slug) {
		return UpdateSlug400JSONResponse{BadRequestJSONResponse{Error: badSlug}}, nil
	}

	var c model.Company
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = h.q.WithTx(tx).UpdateSlug(ctx, model.UpdateSlugParams{Slug: request.Company, NewSlug: slug})
		if err != nil {
			return err
		}
		ref := conv.FormatID(c.ID)
		if err := tenant.RenameTx(ctx, h.river, tx, ref, slug); err != nil {
			return err
		}
		return tenant.SyncTx(ctx, h.river, tx, mirror.Company, ref, "")
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return UpdateSlug404JSONResponse{h.notFound}, nil
	case isUnique(err):
		return UpdateSlug409JSONResponse{ConflictJSONResponse{Error: "slug is taken"}}, nil
	case errors.Is(err, tenant.ErrIDTaken):
		return UpdateSlug409JSONResponse{ConflictJSONResponse{Error: "slug was in use until recently; try again shortly"}}, nil
	case errors.Is(err, tenant.ErrRenamePending):
		return UpdateSlug409JSONResponse{ConflictJSONResponse{Error: "the last slug change is still being applied; try again shortly"}}, nil
	case err != nil:
		return nil, fmt.Errorf("update slug: %w", err)
	}

	h.settle(ctx, c)
	return UpdateSlug200JSONResponse(toAPICompany(c)), nil
}

func (h *CompanyHandler) DeleteCompany(ctx context.Context, request DeleteCompanyRequestObject) (DeleteCompanyResponseObject, error) {
	// Once the master row is gone no new request resolves to the company;
	// its settle job removes the database when the ones in flight finish.
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		c, err := h.q.WithTx(tx).DeleteCompany(ctx, request.Company)
		if err != nil {
			return err
		}
		return tenant.DeleteTx(ctx, h.river, tx, conv.FormatID(c.ID))
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete company: %w", err)
	}

	return DeleteCompany204Response{}, nil
}

// settle creates or renames c's database now rather than waiting for its
// tenant_settle job, which retries if this fails. It waits a few seconds
// at most for requests in flight on the database.
func (h *CompanyHandler) settle(ctx context.Context, c model.Company) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.sync.Settle(ctx, conv.FormatID(c.ID)); err != nil {
		slog.WarnContext(ctx, "Settling company database failed; left to tenant_settle", "company", c.Slug, "err", err)
	}
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
