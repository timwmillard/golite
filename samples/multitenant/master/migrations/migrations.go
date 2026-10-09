// Package migrations embeds the master database's SQL migrations.
package migrations

import "embed"

// FS holds pragmas.sql and the NNNN_name.sql migrations.
//
//go:embed *.sql
var FS embed.FS
