// Package migrations embeds the goose migration files, so the compiled binary carries
// its own schema and can apply it from a distroless image that has no .sql files on
// disk.
package migrations

import "embed"

// FS holds the migration files. config.go passes it to goose with SetBaseFS and "."
// as the directory, so every *.sql at this package's root is a migration and each
// file's own comment names the task it belongs to.
//
//go:embed *.sql
var FS embed.FS
