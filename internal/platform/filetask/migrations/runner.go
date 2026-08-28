package migrations

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	platformmigration "github.com/J-S-Te/Basic-Platform/internal/migration"
	"gorm.io/gorm"
)

// Run 在独立元数据表和同一条 MySQL 连接 advisory lock 中执行 File Gateway 迁移。
func Run(ctx context.Context, database *gorm.DB) error {
	if database == nil {
		return fmt.Errorf("file gateway migration database is nil")
	}
	if err := database.WithContext(ctx).Exec(`CREATE TABLE IF NOT EXISTS file_gateway_schema_migration (version BIGINT UNSIGNED NOT NULL, name VARCHAR(255) NOT NULL, checksum BINARY(32) NOT NULL, applied_at DATETIME(3) NOT NULL, PRIMARY KEY (version)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`).Error; err != nil {
		return err
	}
	entries, err := fs.Glob(Files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	if len(entries) == 0 {
		return fmt.Errorf("no file gateway migrations found")
	}
	return database.WithContext(ctx).Connection(func(connection *gorm.DB) error {
		var locked int
		if err := connection.WithContext(ctx).Raw("SELECT GET_LOCK(?, ?)", "basic-platform:file-gateway-schema", 30).Scan(&locked).Error; err != nil {
			return fmt.Errorf("acquire file gateway migration lock: %w", err)
		}
		if locked != 1 {
			return fmt.Errorf("file gateway migration lock was not acquired")
		}
		defer func() {
			_ = connection.WithContext(context.Background()).Exec("SELECT RELEASE_LOCK(?)", "basic-platform:file-gateway-schema").Error
		}()
		for index, name := range entries {
			parts := strings.SplitN(path.Base(name), "_", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid file gateway migration %q", name)
			}
			version, err := strconv.ParseUint(parts[0], 10, 64)
			if err != nil || version != 100+uint64(index) {
				return fmt.Errorf("file gateway migration versions must be contiguous from 000100")
			}
			content, err := fs.ReadFile(Files, name)
			if err != nil {
				return err
			}
			checksum := sha256.Sum256(content)
			var stored struct {
				Checksum []byte
			}
			if err := connection.WithContext(ctx).Raw("SELECT checksum FROM file_gateway_schema_migration WHERE version = ?", version).Scan(&stored).Error; err != nil {
				return err
			}
			if len(stored.Checksum) == sha256.Size {
				if string(stored.Checksum) != string(checksum[:]) {
					return fmt.Errorf("file gateway migration %d checksum differs", version)
				}
				continue
			}
			statements, err := platformmigration.SplitStatements(string(content))
			if err != nil {
				return fmt.Errorf("parse file gateway migration %d: %w", version, err)
			}
			for statementIndex, statement := range statements {
				if err := connection.WithContext(ctx).Exec(statement).Error; err != nil {
					return fmt.Errorf("execute file gateway migration %d statement %d: %w", version, statementIndex+1, err)
				}
			}
			if err := connection.WithContext(ctx).Exec("INSERT INTO file_gateway_schema_migration (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)", version, strings.TrimSuffix(parts[1], ".sql"), checksum[:], time.Now().UTC()).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
