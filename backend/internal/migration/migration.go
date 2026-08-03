// Package migration 负责按版本执行 MySQL 架构迁移，并校验已发布迁移不可被篡改。
package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	metadataTableName = "platform_schema_migration"
	migrationLockName = "basic-platform:schema-migration"
	migrationLockWait = 30
)

var migrationFilePattern = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.sql$`)

// Item 表示一份按版本排序且发布后不可修改的 SQL 迁移；Checksum 用来阻止直接改写历史文件。
type Item struct {
	Version  uint64
	Name     string
	SQL      string
	Checksum [sha256.Size]byte
}

// Applied 仅返回本次新落库的迁移，调用方可据此记录发布日志，而不会把历史版本重复上报。
type Applied struct {
	Version uint64
	Name    string
}

// Load 在连接数据库前先校验文件命名、版本连续性、重复版本和空内容，避免执行到一半才发现发布包不完整。
func Load(source fs.FS) ([]Item, error) {
	paths, err := fs.Glob(source, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migration files: %w", err)
	}
	if len(paths) == 0 {
		return nil, errors.New("no migration files found")
	}

	items := make([]Item, 0, len(paths))
	seenVersions := make(map[uint64]string, len(paths))
	for _, filePath := range paths {
		baseName := path.Base(filePath)
		matches := migrationFilePattern.FindStringSubmatch(baseName)
		if matches == nil {
			return nil, fmt.Errorf("migration file %q must use 000001_descriptive_name.sql format", baseName)
		}

		version, err := strconv.ParseUint(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse migration version from %q: %w", baseName, err)
		}
		if previous, exists := seenVersions[version]; exists {
			return nil, fmt.Errorf("migration version %d is duplicated by %q and %q", version, previous, baseName)
		}

		content, err := fs.ReadFile(source, filePath)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", baseName, err)
		}
		if len(strings.TrimSpace(string(content))) == 0 {
			return nil, fmt.Errorf("migration %q is empty", baseName)
		}

		item := Item{
			Version:  version,
			Name:     matches[2],
			SQL:      string(content),
			Checksum: sha256.Sum256(content),
		}
		seenVersions[version] = baseName
		items = append(items, item)
	}

	sort.Slice(items, func(left, right int) bool {
		return items[left].Version < items[right].Version
	})

	for index, item := range items {
		expectedVersion := uint64(index + 1)
		if item.Version != expectedVersion {
			return nil, fmt.Errorf("migration versions must be contiguous: expected %06d, found %06d", expectedVersion, item.Version)
		}
	}

	return items, nil
}

// Run 按版本执行所有待应用迁移。MySQL 建议锁必须绑定到同一条物理连接，因此整个流程放在
// Connection 回调内，防止多个发布实例同时修改同一 schema。MySQL DDL 可能隐式提交，无法用
// 一个事务可靠回滚整份文件，所以语句逐条执行，迁移文件本身必须采用可安全重试的 DDL/DML。
func Run(ctx context.Context, database *gorm.DB, source fs.FS) ([]Applied, error) {
	items, err := Load(source)
	if err != nil {
		return nil, err
	}
	if err := ensureMetadataTable(ctx, database); err != nil {
		return nil, err
	}

	var result []Applied
	if err := database.WithContext(ctx).Connection(func(lockDatabase *gorm.DB) error {
		if err := acquireLock(ctx, lockDatabase); err != nil {
			return err
		}
		defer releaseLock(lockDatabase)

		applied, err := readApplied(ctx, lockDatabase)
		if err != nil {
			return err
		}

		result = make([]Applied, 0, len(items))
		for _, item := range items {
			if checksum, exists := applied[item.Version]; exists {
				// 已执行版本只能跳过，不能“以文件为准”重跑；否则不同实例可能面对不同数据库结构。
				if checksum != item.Checksum {
					return fmt.Errorf(
						"migration %06d_%s checksum differs from applied version (database=%x, file=%x); create a new migration instead of editing an applied file",
						item.Version,
						item.Name,
						checksum,
						item.Checksum,
					)
				}
				continue
			}

			statements, err := SplitStatements(item.SQL)
			if err != nil {
				return fmt.Errorf("parse migration %06d_%s: %w", item.Version, item.Name, err)
			}
			for statementIndex, statement := range statements {
				if err := lockDatabase.WithContext(ctx).Exec(statement).Error; err != nil {
					return fmt.Errorf("execute migration %06d_%s statement %d: %w", item.Version, item.Name, statementIndex+1, err)
				}
			}

			if err := lockDatabase.WithContext(ctx).Exec(
				"INSERT INTO "+metadataTableName+" (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
				item.Version,
				item.Name,
				item.Checksum[:],
				time.Now().UTC(),
			).Error; err != nil {
				return fmt.Errorf("record migration %06d_%s: %w", item.Version, item.Name, err)
			}
			result = append(result, Applied{Version: item.Version, Name: item.Name})
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("run migrations with dedicated lock connection: %w", err)
	}

	return result, nil
}

func ensureMetadataTable(ctx context.Context, database *gorm.DB) error {
	statement := `CREATE TABLE IF NOT EXISTS platform_schema_migration (
		version BIGINT UNSIGNED NOT NULL,
		name VARCHAR(255) NOT NULL,
		checksum BINARY(32) NOT NULL,
		applied_at DATETIME(3) NOT NULL,
		PRIMARY KEY (version)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if err := database.WithContext(ctx).Exec(statement).Error; err != nil {
		return fmt.Errorf("create migration metadata table: %w", err)
	}
	return nil
}

func acquireLock(ctx context.Context, database *gorm.DB) error {
	var acquired int
	if err := database.WithContext(ctx).Raw("SELECT GET_LOCK(?, ?)", migrationLockName, migrationLockWait).Scan(&acquired).Error; err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if acquired != 1 {
		return errors.New("migration lock was not acquired within the configured wait period")
	}
	return nil
}

func releaseLock(database *gorm.DB) {
	// 即使原请求已取消也要尽力释放会话锁；沿用已取得锁的专用连接，不能切回连接池中的另一连接。
	_ = database.WithContext(context.Background()).Exec("SELECT RELEASE_LOCK(?)", migrationLockName).Error
}

type appliedMigrationRecord struct {
	Version  uint64
	Checksum []byte
}

func readApplied(ctx context.Context, database *gorm.DB) (map[uint64][sha256.Size]byte, error) {
	var records []appliedMigrationRecord
	if err := database.WithContext(ctx).Raw("SELECT version, checksum FROM " + metadataTableName).Scan(&records).Error; err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}

	applied := make(map[uint64][sha256.Size]byte, len(records))
	for _, record := range records {
		if len(record.Checksum) != sha256.Size {
			return nil, fmt.Errorf("migration %06d has an invalid checksum length", record.Version)
		}

		var checksum [sha256.Size]byte
		copy(checksum[:], record.Checksum)
		applied[record.Version] = checksum
	}
	return applied, nil
}

// SplitStatements 是受限 SQL 切分器：字符串、反引号标识符和注释内的分号不会被误切。
// 这里有意不支持存储过程和 DELIMITER，迁移必须保持为普通 DDL/DML，才能维持逐语句重试边界。
func SplitStatements(script string) ([]string, error) {
	const (
		normal = iota
		singleQuoted
		doubleQuoted
		backtickQuoted
		lineComment
		blockComment
	)

	state := normal
	var builder strings.Builder
	statements := make([]string, 0)

	for index := 0; index < len(script); index++ {
		current := script[index]
		next := byte(0)
		if index+1 < len(script) {
			next = script[index+1]
		}

		switch state {
		case normal:
			switch current {
			case '\'':
				state = singleQuoted
				builder.WriteByte(current)
			case '"':
				state = doubleQuoted
				builder.WriteByte(current)
			case '`':
				state = backtickQuoted
				builder.WriteByte(current)
			case '#':
				state = lineComment
				builder.WriteByte(current)
			case '-':
				if next == '-' && (index+2 == len(script) || script[index+2] == ' ' || script[index+2] == '\t' || script[index+2] == '\r' || script[index+2] == '\n') {
					state = lineComment
				}
				builder.WriteByte(current)
			case '/':
				if next == '*' {
					state = blockComment
				}
				builder.WriteByte(current)
			case ';':
				statement := strings.TrimSpace(builder.String())
				if statement != "" {
					statements = append(statements, statement)
				}
				builder.Reset()
			default:
				builder.WriteByte(current)
			}
		case singleQuoted:
			builder.WriteByte(current)
			if current == '\\' && next != 0 {
				index++
				builder.WriteByte(next)
				continue
			}
			if current == '\'' {
				if next == '\'' {
					index++
					builder.WriteByte(next)
					continue
				}
				state = normal
			}
		case doubleQuoted:
			builder.WriteByte(current)
			if current == '\\' && next != 0 {
				index++
				builder.WriteByte(next)
				continue
			}
			if current == '"' {
				if next == '"' {
					index++
					builder.WriteByte(next)
					continue
				}
				state = normal
			}
		case backtickQuoted:
			builder.WriteByte(current)
			if current == '`' {
				if next == '`' {
					index++
					builder.WriteByte(next)
					continue
				}
				state = normal
			}
		case lineComment:
			builder.WriteByte(current)
			if current == '\n' {
				state = normal
			}
		case blockComment:
			builder.WriteByte(current)
			if current == '*' && next == '/' {
				index++
				builder.WriteByte(next)
				state = normal
			}
		}
	}

	if state == singleQuoted || state == doubleQuoted || state == backtickQuoted || state == blockComment {
		return nil, errors.New("unterminated SQL quote or comment")
	}

	statement := strings.TrimSpace(builder.String())
	if statement != "" {
		statements = append(statements, statement)
	}
	if len(statements) == 0 {
		return nil, errors.New("migration has no executable statements")
	}
	return statements, nil
}
