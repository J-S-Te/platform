// Package infrastructure provides MySQL-backed identity repositories.
package infrastructure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

const consoleApplicationCode = "platform"

// MySQLRepository persists identity credentials and browser-session state in the platform schema.
type MySQLRepository struct {
	database *sql.DB
}

// NewMySQLRepository creates an identity repository using the application's shared MySQL pool.
func NewMySQLRepository(database *sql.DB) (*MySQLRepository, error) {
	if database == nil {
		return nil, errors.New("identity MySQL database must not be nil")
	}
	return &MySQLRepository{database: database}, nil
}

// FindLoginAccount returns the local account and password credential for a supplied username.
// It intentionally does not filter status values because the application layer must distinguish an
// existing account lock from the generic unauthenticated cases without leaking other account state.
func (repository *MySQLRepository) FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error) {
	const query = `
SELECT
    tenant.id, tenant.name, tenant.code, tenant.status,
    user.id, user.display_name, user.status,
    account.id, account.username, account.status, account.locked_until,
    credential.password_hash, credential.hash_algorithm, credential.algorithm_params,
    credential.status, credential.expires_at
FROM iam_account AS account
JOIN iam_tenant AS tenant
  ON tenant.id = account.tenant_id
JOIN iam_user AS user
  ON user.id = account.user_id AND user.tenant_id = account.tenant_id
JOIN iam_password_credential AS credential
  ON credential.account_id = account.id
WHERE account.username = ?
  AND account.auth_source = 'LOCAL'
LIMIT 1`

	var (
		account             domain.LoginAccount
		lockedUntil         sql.NullTime
		credentialExpiresAt sql.NullTime
	)
	err := repository.database.QueryRowContext(ctx, query, accountName).Scan(
		&account.TenantID, &account.TenantName, &account.TenantCode, &account.TenantStatus,
		&account.UserID, &account.UserName, &account.UserStatus,
		&account.AccountID, &account.AccountName, &account.AccountStatus, &lockedUntil,
		&account.PasswordHash, &account.HashAlgorithm, &account.AlgorithmParams,
		&account.CredentialStatus, &credentialExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.LoginAccount{}, application.ErrUnauthenticated
	}
	if err != nil {
		return domain.LoginAccount{}, fmt.Errorf("query local account credential: %w", err)
	}
	if lockedUntil.Valid {
		value := lockedUntil.Time.UTC()
		account.LockedUntil = &value
	}
	if credentialExpiresAt.Valid {
		value := credentialExpiresAt.Time.UTC()
		account.CredentialExpiry = &value
	}
	return account, nil
}

// RecordFailedPasswordAttempt updates only the credential counters. Account lockout policy is P1,
// while this P0 persistence keeps the required information for that later policy.
func (repository *MySQLRepository) RecordFailedPasswordAttempt(ctx context.Context, accountID string, attemptedAt time.Time) error {
	const query = `
UPDATE iam_password_credential
SET failed_attempts = failed_attempts + 1,
    last_failed_at = ?,
    updated_at = ?
WHERE account_id = ?`
	if _, err := repository.database.ExecContext(ctx, query, attemptedAt, attemptedAt, accountID); err != nil {
		return fmt.Errorf("increment failed password attempts: %w", err)
	}
	return nil
}

// CreateSessionForLogin atomically resets credential failures, records a successful account login
// and inserts iam_session. Its status conditions prevent a concurrent disable or lock operation
// from creating a new usable session after the password has been verified.
func (repository *MySQLRepository) CreateSessionForLogin(ctx context.Context, account domain.LoginAccount, session domain.Session) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin login transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	const updateAccount = `
UPDATE iam_account AS account
JOIN iam_user AS user
  ON user.id = account.user_id AND user.tenant_id = account.tenant_id
JOIN iam_tenant AS tenant
  ON tenant.id = account.tenant_id
SET account.last_login_at = ?, account.updated_at = ?
WHERE account.id = ?
  AND account.tenant_id = ?
  AND account.status = 'ACTIVE'
  AND user.status = 'ACTIVE'
  AND tenant.status = 'ACTIVE'
  AND (account.locked_until IS NULL OR account.locked_until <= ?)`
	result, err := transaction.ExecContext(ctx, updateAccount, session.CreatedAt, session.CreatedAt,
		account.AccountID, account.TenantID, session.CreatedAt)
	if err != nil {
		return fmt.Errorf("update account successful login: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read successful login account update count: %w", err)
	}
	if updated != 1 {
		return application.ErrUnauthenticated
	}

	const updateCredential = `
UPDATE iam_password_credential
SET failed_attempts = 0,
    last_failed_at = NULL,
    updated_at = ?
WHERE account_id = ?
  AND status = 'ACTIVE'
  AND (expires_at IS NULL OR expires_at > ?)`
	result, err = transaction.ExecContext(ctx, updateCredential, session.CreatedAt, account.AccountID, session.CreatedAt)
	if err != nil {
		return fmt.Errorf("reset successful password credential state: %w", err)
	}
	updated, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read successful login credential update count: %w", err)
	}
	if updated != 1 {
		return application.ErrUnauthenticated
	}

	const insertSession = `
INSERT INTO iam_session (
    id, tenant_id, account_id, ip_address, user_agent,
    created_at, last_seen_at, expires_at, status
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'ACTIVE')`
	if _, err := transaction.ExecContext(ctx, insertSession,
		session.ID, session.TenantID, session.AccountID, session.IPAddress, session.UserAgent,
		session.CreatedAt, session.CreatedAt, session.ExpiresAt,
	); err != nil {
		return fmt.Errorf("insert browser session: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit login transaction: %w", err)
	}
	return nil
}

// FindPrincipalBySession verifies current persisted session and account state, then loads the
// console application's active role and permission summary.
func (repository *MySQLRepository) FindPrincipalBySession(ctx context.Context, sessionID string, now time.Time) (domain.Principal, error) {
	const principalQuery = `
SELECT
    session.id,
    tenant.id, tenant.name, tenant.code,
    user.id, user.display_name, user.employee_no,
    account.id, account.username, account.account_type
FROM iam_session AS session
JOIN iam_tenant AS tenant
  ON tenant.id = session.tenant_id AND tenant.status = 'ACTIVE'
JOIN iam_account AS account
  ON account.id = session.account_id
 AND account.tenant_id = session.tenant_id
 AND account.status = 'ACTIVE'
JOIN iam_user AS user
  ON user.id = account.user_id
 AND user.tenant_id = session.tenant_id
 AND user.status = 'ACTIVE'
WHERE session.id = ?
  AND session.status = 'ACTIVE'
  AND session.revoked_at IS NULL
  AND session.expires_at > ?`

	var (
		principal   domain.Principal
		userCode    sql.NullString
		accountName sql.NullString
		accountCode sql.NullString
	)
	err := repository.database.QueryRowContext(ctx, principalQuery, sessionID, now).Scan(
		&principal.SessionID,
		&principal.Tenant.ID, &principal.Tenant.Name, &principal.Tenant.Code,
		&principal.User.ID, &principal.User.Name, &userCode,
		&principal.Account.ID, &accountName, &accountCode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Principal{}, application.ErrUnauthenticated
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("query active session principal: %w", err)
	}
	if userCode.Valid {
		principal.User.Code = userCode.String
	}
	if accountName.Valid {
		principal.Account.Name = accountName.String
	}
	if accountCode.Valid {
		principal.Account.Code = accountCode.String
	}

	roles, err := repository.findRoles(ctx, principal.Tenant.ID, principal.User.ID, now)
	if err != nil {
		return domain.Principal{}, err
	}
	permissions, err := repository.findPermissionCodes(ctx, principal.Tenant.ID, principal.User.ID, now)
	if err != nil {
		return domain.Principal{}, err
	}
	principal.Roles = roles
	principal.PermissionCodes = permissions
	return principal, nil
}

// RefreshSession extends an existing active session after its JWT and current account state have
// already been checked by authentication middleware.
func (repository *MySQLRepository) RefreshSession(ctx context.Context, sessionID string, refreshedAt, expiresAt time.Time) error {
	const query = `
UPDATE iam_session
SET last_seen_at = ?, expires_at = ?
WHERE id = ?
  AND status = 'ACTIVE'
  AND revoked_at IS NULL
  AND expires_at > ?`
	result, err := repository.database.ExecContext(ctx, query, refreshedAt, expiresAt, sessionID, refreshedAt)
	if err != nil {
		return fmt.Errorf("update session expiry: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read refreshed session update count: %w", err)
	}
	if updated != 1 {
		return application.ErrUnauthenticated
	}
	return nil
}

// RevokeSession marks the active current session as revoked. The API never deletes session rows.
func (repository *MySQLRepository) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time, reason string) error {
	const query = `
UPDATE iam_session
SET status = 'REVOKED', revoked_at = ?, revoke_reason = ?
WHERE id = ?
  AND status = 'ACTIVE'
  AND revoked_at IS NULL`
	result, err := repository.database.ExecContext(ctx, query, revokedAt, reason, sessionID)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read revoked session update count: %w", err)
	}
	if updated != 1 {
		return application.ErrUnauthenticated
	}
	return nil
}

func (repository *MySQLRepository) findRoles(ctx context.Context, tenantID, userID string, now time.Time) ([]domain.ReferenceName, error) {
	const query = `
SELECT DISTINCT role.id, role.name, role.code
FROM authz_role_binding AS binding
JOIN platform_application AS application
  ON application.id = binding.application_id
 AND application.tenant_id = binding.tenant_id
 AND application.code = ?
 AND application.status = 'ACTIVE'
JOIN authz_role AS role
  ON role.id = binding.role_id
 AND role.tenant_id = binding.tenant_id
 AND role.application_id = binding.application_id
 AND role.status = 'ACTIVE'
WHERE binding.tenant_id = ?
  AND binding.subject_type = 'USER'
  AND binding.subject_id = ?
  AND binding.status = 'ACTIVE'
  AND (binding.valid_from IS NULL OR binding.valid_from <= ?)
  AND (binding.valid_until IS NULL OR binding.valid_until > ?)
ORDER BY role.code`

	rows, err := repository.database.QueryContext(ctx, query, consoleApplicationCode, tenantID, userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("query principal roles: %w", err)
	}
	defer rows.Close()

	roles := make([]domain.ReferenceName, 0)
	for rows.Next() {
		var role domain.ReferenceName
		if err := rows.Scan(&role.ID, &role.Name, &role.Code); err != nil {
			return nil, fmt.Errorf("scan principal role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate principal roles: %w", err)
	}
	return roles, nil
}

func (repository *MySQLRepository) findPermissionCodes(ctx context.Context, tenantID, userID string, now time.Time) ([]string, error) {
	const query = `
SELECT DISTINCT permission.code
FROM authz_role_binding AS binding
JOIN platform_application AS application
  ON application.id = binding.application_id
 AND application.tenant_id = binding.tenant_id
 AND application.code = ?
 AND application.status = 'ACTIVE'
JOIN authz_role AS role
  ON role.id = binding.role_id
 AND role.tenant_id = binding.tenant_id
 AND role.application_id = binding.application_id
 AND role.status = 'ACTIVE'
JOIN authz_role_permission AS role_permission
  ON role_permission.role_id = role.id
 AND role_permission.effect = 'ALLOW'
JOIN authz_permission AS permission
  ON permission.id = role_permission.permission_id
 AND permission.tenant_id = binding.tenant_id
 AND permission.application_id = binding.application_id
 AND permission.status = 'ACTIVE'
WHERE binding.tenant_id = ?
  AND binding.subject_type = 'USER'
  AND binding.subject_id = ?
  AND binding.status = 'ACTIVE'
  AND (binding.valid_from IS NULL OR binding.valid_from <= ?)
  AND (binding.valid_until IS NULL OR binding.valid_until > ?)
ORDER BY permission.code`

	rows, err := repository.database.QueryContext(ctx, query, consoleApplicationCode, tenantID, userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("query principal permissions: %w", err)
	}
	defer rows.Close()

	permissions := make([]string, 0)
	for rows.Next() {
		var permissionCode string
		if err := rows.Scan(&permissionCode); err != nil {
			return nil, fmt.Errorf("scan principal permission: %w", err)
		}
		permissions = append(permissions, permissionCode)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate principal permissions: %w", err)
	}
	return permissions, nil
}
