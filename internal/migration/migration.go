// Package migration executes versioned MySQL schema migrations.
package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	metadataTableName = "platform_schema_migration"
	migrationLockName = "basic-platform:schema-migration"
	migrationLockWait = 30
)

var migrationFilePattern = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.sql$`)

// Item is one immutable, ordered SQL migration.
type Item struct {
	Version  uint64
	Name     string
	SQL      string
	Checksum [sha256.Size]byte
}

// Applied contains the identity of a migration successfully recorded by Run.
type Applied struct {
	Version uint64
	Name    string
}

// Load reads and validates versioned migration files from source.
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

// Run applies every pending migration in version order. It uses a MySQL advisory lock to prevent
// concurrent application instances from modifying the same schema at the same time. MySQL DDL can
// perform implicit commits, so each statement is deliberately executed independently; migrations
// are written with CREATE TABLE IF NOT EXISTS and idempotent seed statements for safe retry.
func Run(ctx context.Context, db *sql.DB, source fs.FS) ([]Applied, error) {
	items, err := Load(source)
	if err != nil {
		return nil, err
	}
	if err := ensureMetadataTable(ctx, db); err != nil {
		return nil, err
	}
	lockConnection, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open migration lock connection: %w", err)
	}
	defer lockConnection.Close()

	if err := acquireLock(ctx, lockConnection); err != nil {
		return nil, err
	}
	defer releaseLock(lockConnection)

	applied, err := readApplied(ctx, db)
	if err != nil {
		return nil, err
	}

	result := make([]Applied, 0, len(items))
	for _, item := range items {
		if checksum, exists := applied[item.Version]; exists {
			if checksum != item.Checksum {
				return nil, fmt.Errorf("migration %06d_%s checksum differs from applied version; create a new migration instead of editing an applied file", item.Version, item.Name)
			}
			continue
		}

		statements, err := SplitStatements(item.SQL)
		if err != nil {
			return nil, fmt.Errorf("parse migration %06d_%s: %w", item.Version, item.Name, err)
		}
		for statementIndex, statement := range statements {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return nil, fmt.Errorf("execute migration %06d_%s statement %d: %w", item.Version, item.Name, statementIndex+1, err)
			}
		}

		if _, err := db.ExecContext(
			ctx,
			"INSERT INTO "+metadataTableName+" (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
			item.Version,
			item.Name,
			item.Checksum[:],
			time.Now().UTC(),
		); err != nil {
			return nil, fmt.Errorf("record migration %06d_%s: %w", item.Version, item.Name, err)
		}
		result = append(result, Applied{Version: item.Version, Name: item.Name})
	}

	return result, nil
}

func ensureMetadataTable(ctx context.Context, db *sql.DB) error {
	statement := `CREATE TABLE IF NOT EXISTS platform_schema_migration (
		version BIGINT UNSIGNED NOT NULL,
		name VARCHAR(255) NOT NULL,
		checksum BINARY(32) NOT NULL,
		applied_at DATETIME(3) NOT NULL,
		PRIMARY KEY (version)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create migration metadata table: %w", err)
	}
	return nil
}

func acquireLock(ctx context.Context, connection *sql.Conn) error {
	var acquired int
	if err := connection.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", migrationLockName, migrationLockWait).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if acquired != 1 {
		return errors.New("migration lock was not acquired within the configured wait period")
	}
	return nil
}

func releaseLock(connection *sql.Conn) {
	_, _ = connection.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", migrationLockName)
}

func readApplied(ctx context.Context, db *sql.DB) (map[uint64][sha256.Size]byte, error) {
	rows, err := db.QueryContext(ctx, "SELECT version, checksum FROM "+metadataTableName)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[uint64][sha256.Size]byte)
	for rows.Next() {
		var version uint64
		var rawChecksum []byte
		if err := rows.Scan(&version, &rawChecksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		if len(rawChecksum) != sha256.Size {
			return nil, fmt.Errorf("migration %06d has an invalid checksum length", version)
		}

		var checksum [sha256.Size]byte
		copy(checksum[:], rawChecksum)
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

// SplitStatements separates a migration script into SQL statements without splitting semicolons
// inside SQL string literals or quoted identifiers. Stored procedures and DELIMITER directives are
// intentionally unsupported; migrations must use ordinary DDL/DML statements only.
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
