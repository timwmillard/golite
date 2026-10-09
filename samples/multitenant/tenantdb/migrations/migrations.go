// Package migrations embeds each tenant database's SQL migrations.
package migrations

import "embed"

// FS holds pragmas.sql and the NNNN_name.sql migrations.
//
//go:embed *.sql
var FS embed.FS
