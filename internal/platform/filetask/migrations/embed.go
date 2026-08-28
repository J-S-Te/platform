// Package migrations 提供 File Gateway 独立 schema 的嵌入迁移文件。
package migrations

import "embed"

// Files 不被平台主迁移二进制嵌入，避免 File Gateway schema 跟随基础平台数据库发布。
//
//go:embed *.sql
var Files embed.FS
