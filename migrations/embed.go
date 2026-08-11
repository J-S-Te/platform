// Package migrations exposes the versioned MySQL migration files embedded in the binary.
package migrations

import "embed"

// Files contains every versioned SQL migration in this directory.
//
//go:embed *.sql
var Files embed.FS
