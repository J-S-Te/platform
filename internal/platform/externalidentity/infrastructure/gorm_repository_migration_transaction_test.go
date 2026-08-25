package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestLegacyExternalLoginNameMigrationCommitsWithoutOverwritingExistingPassword(t *testing.T) {
	scenario := &legacyMigrationScenario{
		desiredLoginName:  "13800138000",
		accountUsername:   "EXT-01M0LEGACY",
		identityAccountNo: "EXT-01M0LEGACY",
		credentialExists:  true,
		credentialHash:    "customer-changed-password-hash",
		credentialChange:  false,
	}
	database := newLegacyMigrationTestDatabase(t, scenario)
	command := legacyMigrationCommand()
	identity := legacyMigrationIdentity()
	account := legacyMigrationAccount()

	err := database.Transaction(func(tx *gorm.DB) error {
		migrated, migrateErr := migrateLegacyExternalLoginName(tx, command, &identity, account)
		if migrateErr != nil {
			return migrateErr
		}
		if !migrated {
			return errors.New("legacy login name was not migrated")
		}
		// 已改密客户的凭据必须原样保留；收敛函数只能在凭据不存在时插入。
		return ensureExternalPasswordCredential(tx, command, account.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !scenario.committed || scenario.rolledBack {
		t.Fatalf("transaction state: committed=%t rolled_back=%t", scenario.committed, scenario.rolledBack)
	}
	if scenario.accountUsername != scenario.desiredLoginName || scenario.identityAccountNo != scenario.desiredLoginName {
		t.Fatalf("migrated names: account=%q identity=%q", scenario.accountUsername, scenario.identityAccountNo)
	}
	if scenario.credentialHash != "customer-changed-password-hash" || scenario.credentialChange {
		t.Fatalf("existing credential was modified: hash=%q must_change=%t", scenario.credentialHash, scenario.credentialChange)
	}
	if scenario.credentialWrites != 0 {
		t.Fatalf("existing credential received %d write operations", scenario.credentialWrites)
	}
}

func TestLegacyExternalLoginNameMigrationRollsBackWhenIdentityUpdateLosesRace(t *testing.T) {
	scenario := &legacyMigrationScenario{
		desiredLoginName:       "13800138000",
		accountUsername:        "EXT-01M0LEGACY",
		identityAccountNo:      "EXT-01M0LEGACY",
		identityUpdateConflict: true,
	}
	database := newLegacyMigrationTestDatabase(t, scenario)
	command := legacyMigrationCommand()
	identity := legacyMigrationIdentity()
	account := legacyMigrationAccount()

	err := database.Transaction(func(tx *gorm.DB) error {
		_, migrateErr := migrateLegacyExternalLoginName(tx, command, &identity, account)
		return migrateErr
	})
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("migration error = %v, want conflict", err)
	}
	if scenario.committed || !scenario.rolledBack {
		t.Fatalf("transaction state: committed=%t rolled_back=%t", scenario.committed, scenario.rolledBack)
	}
	if scenario.accountUsername != "EXT-01M0LEGACY" || scenario.identityAccountNo != "EXT-01M0LEGACY" {
		t.Fatalf("rollback did not restore names: account=%q identity=%q", scenario.accountUsername, scenario.identityAccountNo)
	}
}

func TestLegacyExternalLoginNameMigrationRejectsLoginNameOwnedByAnotherAccount(t *testing.T) {
	scenario := &legacyMigrationScenario{
		desiredLoginName:  "13800138000",
		accountUsername:   "EXT-01M0LEGACY",
		identityAccountNo: "EXT-01M0LEGACY",
		loginNameConflict: true,
	}
	database := newLegacyMigrationTestDatabase(t, scenario)
	command := legacyMigrationCommand()
	identity := legacyMigrationIdentity()
	account := legacyMigrationAccount()

	err := database.Transaction(func(tx *gorm.DB) error {
		_, migrateErr := migrateLegacyExternalLoginName(tx, command, &identity, account)
		return migrateErr
	})
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("migration error = %v, want conflict", err)
	}
	if scenario.committed || !scenario.rolledBack {
		t.Fatalf("transaction state: committed=%t rolled_back=%t", scenario.committed, scenario.rolledBack)
	}
	if scenario.accountUsername != "EXT-01M0LEGACY" || scenario.identityAccountNo != "EXT-01M0LEGACY" {
		t.Fatalf("conflicting login name changed state: account=%q identity=%q", scenario.accountUsername, scenario.identityAccountNo)
	}
}

func legacyMigrationCommand() application.ProvisionCommand {
	return application.ProvisionCommand{
		Principal:      appctx.Principal{TenantID: "tenant-1", OAuthClientID: "crm-client"},
		AccountNo:      "13800138000",
		MobileDigest:   []byte("verified-mobile-digest"),
		CredentialID:   "credential-new",
		PasswordDigest: []byte("initial-password-hash"),
		PasswordParams: []byte(`{"memory":65536}`),
		OccurredAt:     time.Date(2026, time.August, 25, 9, 0, 0, 0, time.UTC),
	}
}

func legacyMigrationIdentity() identityModel {
	return identityModel{
		ID:             "identity-1",
		TenantID:       "tenant-1",
		PlatformUserID: "user-1",
		LoginAccountID: "account-1",
		AccountNo:      "EXT-01M0LEGACY",
	}
}

func legacyMigrationAccount() externalLoginAccount {
	return externalLoginAccount{
		ID:          "account-1",
		UserID:      "user-1",
		Username:    "EXT-01M0LEGACY",
		AccountType: "HUMAN",
		AuthSource:  "LOCAL",
		Status:      activeStatus,
	}
}

type legacyMigrationScenario struct {
	desiredLoginName       string
	accountUsername        string
	identityAccountNo      string
	credentialExists       bool
	credentialHash         string
	credentialChange       bool
	credentialWrites       int
	identityUpdateConflict bool
	loginNameConflict      bool
	committed              bool
	rolledBack             bool
}

type legacyMigrationDriver struct{ scenario *legacyMigrationScenario }
type legacyMigrationConn struct{ scenario *legacyMigrationScenario }

type legacyMigrationTx struct {
	scenario                  *legacyMigrationScenario
	originalAccountUsername   string
	originalIdentityAccountNo string
}

type legacyMigrationResult int64

type legacyMigrationRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

var legacyMigrationDriverCounter uint64

func newLegacyMigrationTestDatabase(t *testing.T, scenario *legacyMigrationScenario) *gorm.DB {
	t.Helper()
	driverName := fmt.Sprintf("external-identity-legacy-migration-test-%d", atomic.AddUint64(&legacyMigrationDriverCounter, 1))
	sql.Register(driverName, &legacyMigrationDriver{scenario: scenario})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open legacy migration test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open legacy migration GORM database: %v", err)
	}
	return database
}

func (driver *legacyMigrationDriver) Open(string) (driver.Conn, error) {
	return &legacyMigrationConn{scenario: driver.scenario}, nil
}

func (connection *legacyMigrationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by legacy migration test driver")
}

func (*legacyMigrationConn) Close() error { return nil }

func (connection *legacyMigrationConn) Begin() (driver.Tx, error) {
	return connection.beginTransaction(), nil
}

func (connection *legacyMigrationConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return connection.beginTransaction(), nil
}

func (connection *legacyMigrationConn) beginTransaction() *legacyMigrationTx {
	return &legacyMigrationTx{
		scenario:                  connection.scenario,
		originalAccountUsername:   connection.scenario.accountUsername,
		originalIdentityAccountNo: connection.scenario.identityAccountNo,
	}
}

func (transaction *legacyMigrationTx) Commit() error {
	transaction.scenario.committed = true
	return nil
}

func (transaction *legacyMigrationTx) Rollback() error {
	transaction.scenario.accountUsername = transaction.originalAccountUsername
	transaction.scenario.identityAccountNo = transaction.originalIdentityAccountNo
	transaction.scenario.rolledBack = true
	return nil
}

func (result legacyMigrationResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result legacyMigrationResult) RowsAffected() (int64, error) { return int64(result), nil }

func (connection *legacyMigrationConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	normalized := strings.ToLower(query)
	switch {
	case strings.HasPrefix(strings.TrimSpace(normalized), "update `iam_account`"):
		connection.scenario.accountUsername = connection.scenario.desiredLoginName
		return legacyMigrationResult(1), nil
	case strings.HasPrefix(strings.TrimSpace(normalized), "update `iam_external_identity`"):
		if connection.scenario.identityUpdateConflict {
			return legacyMigrationResult(0), nil
		}
		connection.scenario.identityAccountNo = connection.scenario.desiredLoginName
		return legacyMigrationResult(1), nil
	case strings.Contains(normalized, "iam_password_credential"):
		connection.scenario.credentialWrites++
		return legacyMigrationResult(1), nil
	default:
		return nil, fmt.Errorf("unexpected legacy migration exec: %s", query)
	}
}

func (connection *legacyMigrationConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	switch {
	case strings.Contains(normalized, "from `iam_account`"):
		if connection.scenario.loginNameConflict {
			return &legacyMigrationRows{columns: []string{"id"}, values: [][]driver.Value{{"account-owned-by-another-user"}}}, nil
		}
		return &legacyMigrationRows{columns: []string{"id"}}, nil
	case strings.Contains(normalized, "from `iam_external_identity`"):
		return &legacyMigrationRows{columns: []string{"id"}}, nil
	case strings.Contains(normalized, "from `iam_password_credential`"):
		if !connection.scenario.credentialExists {
			return &legacyMigrationRows{columns: []string{"id"}}, nil
		}
		return &legacyMigrationRows{columns: []string{"id"}, values: [][]driver.Value{{"credential-existing"}}}, nil
	default:
		return nil, fmt.Errorf("unexpected legacy migration query: %s", query)
	}
}

func (rows *legacyMigrationRows) Columns() []string { return rows.columns }
func (*legacyMigrationRows) Close() error           { return nil }

func (rows *legacyMigrationRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}
