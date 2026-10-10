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
// against the master database. Changes reach each company's own database
// through tenant.Sync jobs, inserted in the same transaction as the change.
type CompanyHandler struct {
	master *sql.DB
	q      *model.Queries
	dbs    *tenant.DBs
	river  *river.Client[*sql.Tx]

	// notFound matches the 404 tenant.Middleware sends for an unknown
	// company on the routes below it.
	notFound NotFoundJSONResponse
}

func NewCompanyHandler(master *sql.DB, dbs *tenant.DBs, riverClient *river.Client[*sql.Tx]) *CompanyHandler {
	return &CompanyHandler{
		master:   master,
		q:        model.New(master),
		dbs:      dbs,
		river:    riverClient,
		notFound: NotFoundJSONResponse{Error: dbs.Name() + " not found"},
	}
}

// validSlug matches CreateCompanyRequest.slug's pattern in the spec, which
// the generated server doesn't enforce.
var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (h *CompanyHandler) ListCompanies(ctx context.Context, request ListCompaniesRequestObject) (ListCompaniesResponseObject, error) {
	companies, err := h.q.ListCompanies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list companies: %w", err)
	}

	resp := make(ListCompanies200JSONResponse, len(companies))
	for i, t := range companies {
		resp[i] = toAPICompany(t)
	}
	return resp, nil
}

func (h *CompanyHandler) CreateCompany(ctx context.Context, request CreateCompanyRequestObject) (CreateCompanyResponseObject, error) {
	body := request.Body
	if !validSlug.MatchString(body.Slug) {
		return CreateCompany400JSONResponse{BadRequestJSONResponse{Error: "slug must be lowercase letters, digits and '-'"}}, nil
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return CreateCompany400JSONResponse{BadRequestJSONResponse{Error: "name is required"}}, nil
	}

	var t model.Company
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = h.q.WithTx(tx).CreateCompany(ctx, model.CreateCompanyParams{
			Slug:      body.Slug,
			Name:      name,
			CreatedAt: time.Now().Unix(),
		})
		if err != nil {
			return err
		}
		return tenant.InsertSyncTx(ctx, h.river, tx, mirror.Company, conv.FormatID(t.ID), "")
	})
	if sqliteErr := (sqlite3.Error{}); errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
		return CreateCompany409JSONResponse{ConflictJSONResponse{Error: "company already exists"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create company: %w", err)
	}

	// The sync job creates the database too, but not until a worker picks
	// it up; create it now so the company is usable as soon as we respond.
	// If this fails the job still will, so the company was still created.
	if err := h.dbs.Create(ctx, conv.FormatID(t.ID)); err != nil {
		slog.WarnContext(ctx, "Create company database failed; left to tenant_sync", "company", t.Slug, "err", err)
	}

	return CreateCompany201JSONResponse(toAPICompany(t)), nil
}

func (h *CompanyHandler) GetCompany(ctx context.Context, request GetCompanyRequestObject) (GetCompanyResponseObject, error) {
	t, err := h.q.GetCompanyBySlug(ctx, request.Company)
	if errors.Is(err, sql.ErrNoRows) {
		return GetCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get company: %w", err)
	}

	return GetCompany200JSONResponse(toAPICompany(t)), nil
}

func (h *CompanyHandler) UpdateCompany(ctx context.Context, request UpdateCompanyRequestObject) (UpdateCompanyResponseObject, error) {
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return UpdateCompany400JSONResponse{BadRequestJSONResponse{Error: "name can't be empty"}}, nil
	}

	var t model.Company
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = h.q.WithTx(tx).UpdateCompany(ctx, model.UpdateCompanyParams{Slug: request.Company, Name: name})
		if err != nil {
			return err
		}
		return tenant.InsertSyncTx(ctx, h.river, tx, mirror.Company, conv.FormatID(t.ID), "")
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update company: %w", err)
	}

	return UpdateCompany200JSONResponse(toAPICompany(t)), nil
}

func (h *CompanyHandler) DeleteCompany(ctx context.Context, request DeleteCompanyRequestObject) (DeleteCompanyResponseObject, error) {
	// Once the master row is gone no new request resolves to the company;
	// the job removes its database when the ones in flight finish.
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		t, err := h.q.WithTx(tx).DeleteCompany(ctx, request.Company)
		if err != nil {
			return err
		}
		return tenant.InsertDeleteTx(ctx, h.river, tx, conv.FormatID(t.ID))
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
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
