// Package migrations embeds the SQL migration files into the binary, so the
// compiled service and the `migrate` subcommand never depend on a
// migrations/ directory being present on disk at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
