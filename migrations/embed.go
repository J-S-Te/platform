// Package migrations 提供嵌入二进制文件的版本化 MySQL 迁移文件。
package migrations

import "embed"

// Files contains every versioned SQL migration in this directory.
//
//go:embed *.sql
var Files embed.FS
