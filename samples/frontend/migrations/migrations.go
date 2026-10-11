// Package migrations embeds the app's SQL migrations into the binary.
package migrations

import "embed"

// FS holds pragmas.sql and the NNNN_name.sql migrations.
//
//go:embed *.sql
var FS embed.FS
