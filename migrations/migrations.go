// Package migrations embeds the SQL schema migrations.
//
// Migrations are plain .sql files named NNN_description.sql and are applied in
// lexicographic order, each exactly once, inside the store's migration
// transaction. They are embedded in the binary so a deployed bot cannot drift
// from the schema its code expects.
//
// Rules for adding one:
//
//   - Never edit a migration that has shipped; add a new file instead.
//   - Keep them idempotent where practical (IF NOT EXISTS) so a partially
//     applied migration can be retried.
//   - Filenames sort as numbers, so use a fixed three-digit prefix.
package migrations

import "embed"

// FS holds every *.sql migration, ordered by file name.
//
//go:embed *.sql
var FS embed.FS
