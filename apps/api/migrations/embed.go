// Package migrations embeds the goose SQL migrations into the binary.
//
// Shipping the migrations inside the artifact rather than alongside it means
// the schema a build expects and the code that expects it can never be
// separately versioned: there is one image, and `api migrate up` run from that
// image applies exactly the migrations it was built with.
package migrations

import "embed"

// FS holds the migration files. Only .sql is embedded, so this file — and any
// future Go migration helper next to it — is not mistaken for a migration.
//
//go:embed *.sql
var FS embed.FS
