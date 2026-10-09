package api

import (
	"github.com/timwmillard/golite/conv"

	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	"github.com/timwmillard/golite/samples/multitenant/tenantdb/model"
)

func toAPITenant(t mastermodel.Tenant) Tenant {
	return Tenant{
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
