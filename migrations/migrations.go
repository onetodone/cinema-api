// Package migrations embeds the SQL schema migrations so every binary ships with them.
package migrations

import "embed"

// FS holds the goose SQL migrations.
//
//go:embed *.sql
var FS embed.FS
