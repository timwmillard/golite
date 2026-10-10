package api

import (
	"github.com/timwmillard/golite/conv"

	"github.com/timwmillard/golite/samples/multitenant/companydb/model"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
)

func toAPICompany(t mastermodel.Company) Company {
	return Company{
		Slug:      t.Slug,
		Name:      t.Name,
		CreatedAt: conv.Unix(t.CreatedAt),
	}
}

func toAPITask(t model.Task) Task {
	return Task{
		ID:          conv.FormatID(t.ID),
		Title:       t.Title,
		Notes:       conv.StringPtr(t.Notes),
		Done:        conv.Bool(t.Done),
		CreatedAt:   conv.Unix(t.CreatedAt),
		CompletedAt: conv.UnixPtr(t.CompletedAt),
	}
}
