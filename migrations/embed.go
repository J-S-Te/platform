// Package migrations 提供嵌入二进制文件的版本化 MySQL 迁移文件。
package migrations

import "embed"

// Files 包含此目录中的全部版本化 SQL 迁移。
//
//go:embed *.sql
var Files embed.FS
