package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
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
// company_<db_key>.db, where db_key is the slug it was created with and
// never changes; changes reach the database through tenant.Sync jobs,
// inserted in the same transaction as the change.
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
		q := h.q.WithTx(tx)
		dbKey, err := freeDBKey(ctx, q, body.Slug)
		if err != nil {
			return err
		}
		c, err = q.CreateCompany(ctx, model.CreateCompanyParams{
			Slug:      body.Slug,
			DbKey:     dbKey,
			Name:      name,
			CreatedAt: time.Now().Unix(),
		})
		if err != nil {
			return err
		}
		if err := tenant.CreateTx(ctx, h.river, tx, c.DbKey); err != nil {
			return err
		}
		return tenant.SyncTx(ctx, h.river, tx, mirror.Company, c.DbKey, "")
	})
	if isUnique(err) {
		return CreateCompany409JSONResponse{ConflictJSONResponse{Error: "company already exists"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create company: %w", err)
	}

	// Create the database now rather than waiting for the tenant_create
	// job, which retries if this fails.
	if err := h.dbs.Create(ctx, c.DbKey); err != nil {
		slog.WarnContext(ctx, "Creating company database failed; left to tenant_create", "company", c.Slug, "err", err)
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
		return tenant.SyncTx(ctx, h.river, tx, mirror.Company, c.DbKey, "")
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateCompany404JSONResponse{h.notFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update company: %w", err)
	}

	return UpdateCompany200JSONResponse(toAPICompany(c)), nil
}

// UpdateSlug changes a company's slug. Its database keeps its name, from
// db_key, so nothing moves.
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
		return tenant.SyncTx(ctx, h.river, tx, mirror.Company, c.DbKey, "")
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return UpdateSlug404JSONResponse{h.notFound}, nil
	case isUnique(err):
		return UpdateSlug409JSONResponse{ConflictJSONResponse{Error: "slug is taken"}}, nil
	case err != nil:
		return nil, fmt.Errorf("update slug: %w", err)
	}

	return UpdateSlug200JSONResponse(toAPICompany(c)), nil
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
	// can't be given the db_key while the old database is still under it.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = h.dbs.Delete(ctx, c.DbKey, func(ctx context.Context) error {
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

// freeDBKey returns the db_key for a new company with slug: the slug,
// unless a company that has since changed its slug still has it as its
// db_key, in which case slug-2, slug-3 and so on.
func freeDBKey(ctx context.Context, q *model.Queries, slug string) (string, error) {
	for n := 1; ; n++ {
		key := slug
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			key = slug[:min(len(slug), 64-len(suffix))] + suffix
		}
		taken, err := q.DBKeyTaken(ctx, key)
		if err != nil {
			return "", fmt.Errorf("check db_key: %w", err)
		}
		if !taken {
			return key, nil
		}
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
