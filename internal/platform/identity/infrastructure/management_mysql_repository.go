package infrastructure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	mysql "github.com/go-sql-driver/mysql"
)

// ListUsers reads tenant-scoped users and leaves mobile masking to the application layer.
func (repository *MySQLRepository) ListUsers(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.User], error) {
	where, arguments := userFilter(tenantID, query)
	var total int64
	if err := repository.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM iam_user "+where, arguments...).Scan(&total); err != nil {
		return application.PageResult[domain.User]{}, fmt.Errorf("count users: %w", err)
	}
	arguments = append(arguments, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := repository.database.QueryContext(ctx, `SELECT id, tenant_id, employee_no, display_name, email, mobile_ciphertext, status, version, created_at, updated_at
FROM iam_user `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return application.PageResult[domain.User]{}, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	items := make([]domain.User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return application.PageResult[domain.User]{}, err
		}
		items = append(items, user)
	}
	if err := rows.Err(); err != nil {
		return application.PageResult[domain.User]{}, fmt.Errorf("iterate users: %w", err)
	}
	return application.PageResult[domain.User]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// CreateUser persists a natural person but intentionally does not create a login account.
func (repository *MySQLRepository) CreateUser(ctx context.Context, write application.UserWrite) (domain.User, error) {
	_, err := repository.database.ExecContext(ctx, `INSERT INTO iam_user
(id, tenant_id, employee_no, display_name, email, mobile_ciphertext, mobile_hash, employment_status, status, version, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, 'EMPLOYED', ?, 1, UTC_TIMESTAMP(3), ?, UTC_TIMESTAMP(3), ?)`,
		write.ID, write.TenantID, nullableString(write.EmployeeNo), write.DisplayName, nullableString(write.Email), nullableBytes(write.MobileCiphertext), nullableBytes(write.MobileHash), write.Status, write.OperatorID, write.OperatorID)
	if err != nil {
		return domain.User{}, mapWriteError(err, "create user")
	}
	return repository.GetUser(ctx, write.TenantID, write.ID)
}

// GetUser retrieves exactly one tenant-scoped user.
func (repository *MySQLRepository) GetUser(ctx context.Context, tenantID, userID string) (domain.User, error) {
	row := repository.database.QueryRowContext(ctx, `SELECT id, tenant_id, employee_no, display_name, email, mobile_ciphertext, status, version, created_at, updated_at
FROM iam_user WHERE tenant_id = ? AND id = ?`, tenantID, userID)
	user, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, application.ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// UpdateUser applies an optimistic-lock update. Optional pointers are intentionally rendered as
// a fixed set of assignments so no client-controlled identifier can affect SQL structure.
func (repository *MySQLRepository) UpdateUser(ctx context.Context, input application.UserUpdateInput, mobileCiphertext, mobileHash []byte) (domain.User, error) {
	assignments := []string{"display_name = ?", "updated_at = UTC_TIMESTAMP(3)", "updated_by = ?", "version = version + 1"}
	arguments := []any{input.DisplayName, input.OperatorID}
	if input.EmployeeNo != nil {
		assignments = append(assignments, "employee_no = ?")
		arguments = append(arguments, nullableText(*input.EmployeeNo))
	}
	if input.Email != nil {
		assignments = append(assignments, "email = ?")
		arguments = append(arguments, nullableText(*input.Email))
	}
	if input.Status != nil {
		assignments = append(assignments, "status = ?")
		arguments = append(arguments, *input.Status)
	}
	if input.UpdateMobile {
		assignments = append(assignments, "mobile_ciphertext = ?", "mobile_hash = ?")
		arguments = append(arguments, nullableBytes(mobileCiphertext), nullableBytes(mobileHash))
	}
	arguments = append(arguments, input.TenantID, input.UserID, input.Version)
	result, err := repository.database.ExecContext(ctx, "UPDATE iam_user SET "+strings.Join(assignments, ", ")+" WHERE tenant_id = ? AND id = ? AND version = ?", arguments...)
	if err != nil {
		return domain.User{}, mapWriteError(err, "update user")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.User{}, fmt.Errorf("read updated user count: %w", err)
	}
	if changed == 0 {
		return domain.User{}, repository.versionedUserError(ctx, input.TenantID, input.UserID)
	}
	return repository.GetUser(ctx, input.TenantID, input.UserID)
}

func (repository *MySQLRepository) versionedUserError(ctx context.Context, tenantID, userID string) error {
	var exists int
	err := repository.database.QueryRowContext(ctx, "SELECT 1 FROM iam_user WHERE tenant_id = ? AND id = ?", tenantID, userID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return application.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check user after update: %w", err)
	}
	return application.ErrVersionConflict
}

func (repository *MySQLRepository) ListAccounts(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Account], error) {
	where, arguments := accountFilter(tenantID, query)
	var total int64
	if err := repository.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM iam_account "+where, arguments...).Scan(&total); err != nil {
		return application.PageResult[domain.Account]{}, fmt.Errorf("count accounts: %w", err)
	}
	arguments = append(arguments, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := repository.database.QueryContext(ctx, `SELECT id, tenant_id, user_id, COALESCE(username, id), status, last_login_at, version, created_at, updated_at
FROM iam_account `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return application.PageResult[domain.Account]{}, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return application.PageResult[domain.Account]{}, err
		}
		items = append(items, account)
	}
	if err := rows.Err(); err != nil {
		return application.PageResult[domain.Account]{}, fmt.Errorf("iterate accounts: %w", err)
	}
	return application.PageResult[domain.Account]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *MySQLRepository) UpdateAccount(ctx context.Context, input application.AccountUpdateInput) (domain.Account, error) {
	result, err := repository.database.ExecContext(ctx, `UPDATE iam_account SET status = ?, updated_at = UTC_TIMESTAMP(3), updated_by = ?, version = version + 1
WHERE tenant_id = ? AND id = ? AND version = ?`, input.Status, input.OperatorID, input.TenantID, input.AccountID, input.Version)
	if err != nil {
		return domain.Account{}, mapWriteError(err, "update account")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Account{}, fmt.Errorf("read updated account count: %w", err)
	}
	if changed == 0 {
		var exists int
		err = repository.database.QueryRowContext(ctx, "SELECT 1 FROM iam_account WHERE tenant_id = ? AND id = ?", input.TenantID, input.AccountID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Account{}, application.ErrNotFound
		}
		if err != nil {
			return domain.Account{}, fmt.Errorf("check account after update: %w", err)
		}
		return domain.Account{}, application.ErrVersionConflict
	}
	return repository.getAccount(ctx, input.TenantID, input.AccountID)
}

func (repository *MySQLRepository) getAccount(ctx context.Context, tenantID, accountID string) (domain.Account, error) {
	row := repository.database.QueryRowContext(ctx, `SELECT id, tenant_id, user_id, COALESCE(username, id), status, last_login_at, version, created_at, updated_at
FROM iam_account WHERE tenant_id = ? AND id = ?`, tenantID, accountID)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Account{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("get account: %w", err)
	}
	return account, nil
}

func (repository *MySQLRepository) ListOrgUnits(ctx context.Context, tenantID, keyword, status string, query application.PageRequest) (application.PageResult[domain.OrgUnit], error) {
	where, arguments := organizationFilter(tenantID, keyword, status)
	var total int64
	if err := repository.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM iam_org_unit "+where, arguments...).Scan(&total); err != nil {
		return application.PageResult[domain.OrgUnit]{}, fmt.Errorf("count organization units: %w", err)
	}
	arguments = append(arguments, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := repository.database.QueryContext(ctx, `SELECT id, tenant_id, parent_id, code, name, org_type, path, depth, sort_order, status, version
FROM iam_org_unit `+where+` ORDER BY path, sort_order, code LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return application.PageResult[domain.OrgUnit]{}, fmt.Errorf("list organization units: %w", err)
	}
	defer rows.Close()
	items := make([]domain.OrgUnit, 0)
	for rows.Next() {
		orgUnit, err := scanOrgUnit(rows)
		if err != nil {
			return application.PageResult[domain.OrgUnit]{}, err
		}
		items = append(items, orgUnit)
	}
	if err := rows.Err(); err != nil {
		return application.PageResult[domain.OrgUnit]{}, fmt.Errorf("iterate organization units: %w", err)
	}
	return application.PageResult[domain.OrgUnit]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *MySQLRepository) CreateOrgUnit(ctx context.Context, orgUnit domain.OrgUnit, operatorID string) (domain.OrgUnit, error) {
	tx, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return domain.OrgUnit{}, fmt.Errorf("begin organization transaction: %w", err)
	}
	defer rollback(tx)
	path, depth := "/"+orgUnit.ID+"/", uint(1)
	if orgUnit.ParentID != nil {
		var parentPath string
		var parentDepth uint
		err = tx.QueryRowContext(ctx, "SELECT path, depth FROM iam_org_unit WHERE tenant_id = ? AND id = ? AND status = 'ACTIVE'", orgUnit.TenantID, *orgUnit.ParentID).Scan(&parentPath, &parentDepth)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.OrgUnit{}, application.ErrNotFound
		}
		if err != nil {
			return domain.OrgUnit{}, fmt.Errorf("get organization parent: %w", err)
		}
		path, depth = parentPath+orgUnit.ID+"/", parentDepth+1
	}
	orgUnit.Path, orgUnit.Depth = path, depth
	_, err = tx.ExecContext(ctx, `INSERT INTO iam_org_unit
(id, tenant_id, parent_id, code, name, org_type, path, depth, sort_order, status, version, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, UTC_TIMESTAMP(3), ?, UTC_TIMESTAMP(3), ?)`, orgUnit.ID, orgUnit.TenantID, nullableString(orgUnit.ParentID), orgUnit.Code, orgUnit.Name, orgUnit.OrgType, orgUnit.Path, orgUnit.Depth, orgUnit.SortOrder, orgUnit.Status, operatorID, operatorID)
	if err != nil {
		return domain.OrgUnit{}, mapWriteError(err, "create organization unit")
	}
	if err := tx.Commit(); err != nil {
		return domain.OrgUnit{}, fmt.Errorf("commit organization transaction: %w", err)
	}
	return orgUnit, nil
}

func (repository *MySQLRepository) ListPositions(ctx context.Context, tenantID, keyword, status string, query application.PageRequest) (application.PageResult[domain.Position], error) {
	where, arguments := positionFilter(tenantID, keyword, status)
	var total int64
	if err := repository.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM iam_position "+where, arguments...).Scan(&total); err != nil {
		return application.PageResult[domain.Position]{}, fmt.Errorf("count positions: %w", err)
	}
	arguments = append(arguments, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := repository.database.QueryContext(ctx, `SELECT id, tenant_id, org_unit_id, code, name, status, version FROM iam_position `+where+` ORDER BY org_unit_id, code LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return application.PageResult[domain.Position]{}, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Position, 0)
	for rows.Next() {
		position, err := scanPosition(rows)
		if err != nil {
			return application.PageResult[domain.Position]{}, err
		}
		items = append(items, position)
	}
	if err := rows.Err(); err != nil {
		return application.PageResult[domain.Position]{}, fmt.Errorf("iterate positions: %w", err)
	}
	return application.PageResult[domain.Position]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *MySQLRepository) CreatePosition(ctx context.Context, position domain.Position, operatorID string) (domain.Position, error) {
	result, err := repository.database.ExecContext(ctx, `INSERT INTO iam_position
(id, tenant_id, org_unit_id, code, name, status, version, created_at, created_by, updated_at, updated_by)
SELECT ?, ?, ?, ?, ?, ?, 1, UTC_TIMESTAMP(3), ?, UTC_TIMESTAMP(3), ?
WHERE EXISTS (SELECT 1 FROM iam_org_unit WHERE id = ? AND tenant_id = ? AND status = 'ACTIVE')`, position.ID, position.TenantID, position.OrgUnitID, position.Code, position.Name, position.Status, operatorID, operatorID, position.OrgUnitID, position.TenantID)
	if err != nil {
		return domain.Position{}, mapWriteError(err, "create position")
	}
	created, err := result.RowsAffected()
	if err != nil {
		return domain.Position{}, fmt.Errorf("read position create count: %w", err)
	}
	if created == 0 {
		return domain.Position{}, application.ErrNotFound
	}
	return position, nil
}

func (repository *MySQLRepository) ListMemberships(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Membership], error) {
	where, arguments := membershipFilter(tenantID, query)
	var total int64
	if err := repository.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM iam_membership m JOIN iam_user u ON u.id = m.user_id AND u.tenant_id = m.tenant_id JOIN iam_org_unit o ON o.id = m.org_unit_id AND o.tenant_id = m.tenant_id `+where, arguments...).Scan(&total); err != nil {
		return application.PageResult[domain.Membership]{}, fmt.Errorf("count memberships: %w", err)
	}
	arguments = append(arguments, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := repository.database.QueryContext(ctx, membershipSelect+where+` ORDER BY m.created_at DESC, m.id DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return application.PageResult[domain.Membership]{}, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Membership, 0)
	for rows.Next() {
		membership, err := scanMembership(rows)
		if err != nil {
			return application.PageResult[domain.Membership]{}, err
		}
		items = append(items, membership)
	}
	if err := rows.Err(); err != nil {
		return application.PageResult[domain.Membership]{}, fmt.Errorf("iterate memberships: %w", err)
	}
	return application.PageResult[domain.Membership]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *MySQLRepository) CreateMembership(ctx context.Context, input application.MembershipCreateInput, id string) (domain.Membership, error) {
	tx, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return domain.Membership{}, fmt.Errorf("begin membership transaction: %w", err)
	}
	defer rollback(tx)
	if err := ensureMembershipReferences(ctx, tx, input); err != nil {
		return domain.Membership{}, err
	}
	isPrimary := input.MembershipType == domain.MembershipPrimary
	if isPrimary {
		if err := ensureNoOtherPrimary(ctx, tx, input.TenantID, input.UserID, ""); err != nil {
			return domain.Membership{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO iam_membership
(id, tenant_id, user_id, org_unit_id, position_id, membership_type, is_primary, valid_from, valid_until, status, version, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'ACTIVE', 1, UTC_TIMESTAMP(3), ?, UTC_TIMESTAMP(3), ?)`, id, input.TenantID, input.UserID, input.OrgUnitID, input.PositionID, input.MembershipType, isPrimary, nullableTime(input.EffectiveFrom), nullableTime(input.EffectiveTo), input.OperatorID, input.OperatorID)
	if err != nil {
		return domain.Membership{}, mapWriteError(err, "create membership")
	}
	if isPrimary {
		if _, err := tx.ExecContext(ctx, "UPDATE iam_user SET primary_org_id = ?, updated_at = UTC_TIMESTAMP(3), updated_by = ?, version = version + 1 WHERE id = ? AND tenant_id = ?", input.OrgUnitID, input.OperatorID, input.UserID, input.TenantID); err != nil {
			return domain.Membership{}, fmt.Errorf("set user primary organization: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Membership{}, fmt.Errorf("commit membership transaction: %w", err)
	}
	return repository.getMembership(ctx, input.TenantID, id)
}

func (repository *MySQLRepository) UpdateMembership(ctx context.Context, input application.MembershipUpdateInput) (domain.Membership, error) {
	tx, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return domain.Membership{}, fmt.Errorf("begin membership update transaction: %w", err)
	}
	defer rollback(tx)
	var existingUserID, existingOrgID string
	var existingPrimary bool
	err = tx.QueryRowContext(ctx, "SELECT user_id, org_unit_id, is_primary FROM iam_membership WHERE tenant_id = ? AND id = ?", input.TenantID, input.MembershipID).Scan(&existingUserID, &existingOrgID, &existingPrimary)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Membership{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Membership{}, fmt.Errorf("get membership for update: %w", err)
	}
	if err := ensureMembershipReferences(ctx, tx, input.MembershipCreateInput); err != nil {
		return domain.Membership{}, err
	}
	isPrimary := input.MembershipType == domain.MembershipPrimary && *input.Status == domain.StatusActive
	if isPrimary {
		if err := ensureNoOtherPrimary(ctx, tx, input.TenantID, input.UserID, input.MembershipID); err != nil {
			return domain.Membership{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE iam_membership SET user_id = ?, org_unit_id = ?, position_id = ?, membership_type = ?, is_primary = ?, valid_from = ?, valid_until = ?, status = ?, version = version + 1, updated_at = UTC_TIMESTAMP(3), updated_by = ?
WHERE tenant_id = ? AND id = ? AND version = ?`, input.UserID, input.OrgUnitID, input.PositionID, input.MembershipType, isPrimary, nullableTime(input.EffectiveFrom), nullableTime(input.EffectiveTo), *input.Status, input.OperatorID, input.TenantID, input.MembershipID, input.Version)
	if err != nil {
		return domain.Membership{}, mapWriteError(err, "update membership")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Membership{}, fmt.Errorf("read updated membership count: %w", err)
	}
	if changed == 0 {
		return domain.Membership{}, application.ErrVersionConflict
	}
	if existingPrimary {
		if _, err := tx.ExecContext(ctx, "UPDATE iam_user SET primary_org_id = NULL, updated_at = UTC_TIMESTAMP(3), updated_by = ?, version = version + 1 WHERE tenant_id = ? AND id = ? AND primary_org_id = ?", input.OperatorID, input.TenantID, existingUserID, existingOrgID); err != nil {
			return domain.Membership{}, fmt.Errorf("clear prior primary organization: %w", err)
		}
	}
	if isPrimary {
		if _, err := tx.ExecContext(ctx, "UPDATE iam_user SET primary_org_id = ?, updated_at = UTC_TIMESTAMP(3), updated_by = ?, version = version + 1 WHERE tenant_id = ? AND id = ?", input.OrgUnitID, input.OperatorID, input.TenantID, input.UserID); err != nil {
			return domain.Membership{}, fmt.Errorf("set updated primary organization: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Membership{}, fmt.Errorf("commit membership update transaction: %w", err)
	}
	return repository.getMembership(ctx, input.TenantID, input.MembershipID)
}

const membershipSelect = `SELECT m.id, m.tenant_id, u.id, u.display_name, o.id, o.name, p.id, p.name, m.membership_type, m.valid_from, m.valid_until, m.status, m.version, m.is_primary
FROM iam_membership m JOIN iam_user u ON u.id = m.user_id AND u.tenant_id = m.tenant_id JOIN iam_org_unit o ON o.id = m.org_unit_id AND o.tenant_id = m.tenant_id JOIN iam_position p ON p.id = m.position_id AND p.tenant_id = m.tenant_id `

func (repository *MySQLRepository) getMembership(ctx context.Context, tenantID, membershipID string) (domain.Membership, error) {
	membership, err := scanMembership(repository.database.QueryRowContext(ctx, membershipSelect+"WHERE m.tenant_id = ? AND m.id = ?", tenantID, membershipID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Membership{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Membership{}, fmt.Errorf("get membership: %w", err)
	}
	return membership, nil
}

func ensureMembershipReferences(ctx context.Context, tx *sql.Tx, input application.MembershipCreateInput) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM iam_user u JOIN iam_org_unit o ON o.id = ? AND o.tenant_id = u.tenant_id AND o.status = 'ACTIVE' JOIN iam_position p ON p.id = ? AND p.tenant_id = u.tenant_id AND p.org_unit_id = o.id AND p.status = 'ACTIVE'
WHERE u.id = ? AND u.tenant_id = ? AND u.status = 'ACTIVE'`, input.OrgUnitID, input.PositionID, input.UserID, input.TenantID).Scan(&count)
	if err != nil {
		return fmt.Errorf("validate membership references: %w", err)
	}
	if count != 1 {
		return application.ErrNotFound
	}
	return nil
}

func ensureNoOtherPrimary(ctx context.Context, tx *sql.Tx, tenantID, userID, excludingID string) error {
	query := "SELECT COUNT(*) FROM iam_membership WHERE tenant_id = ? AND user_id = ? AND is_primary = 1 AND status = 'ACTIVE'"
	arguments := []any{tenantID, userID}
	if excludingID != "" {
		query += " AND id <> ?"
		arguments = append(arguments, excludingID)
	}
	var count int
	if err := tx.QueryRowContext(ctx, query, arguments...).Scan(&count); err != nil {
		return fmt.Errorf("check primary membership: %w", err)
	}
	if count > 0 {
		return application.ErrConflict
	}
	return nil
}

func organizationFilter(tenantID, keyword, status string) (string, []any) {
	where := "WHERE tenant_id = ?"
	arguments := []any{tenantID}
	if keyword != "" {
		where += " AND (code LIKE ? OR name LIKE ?)"
		like := "%" + keyword + "%"
		arguments = append(arguments, like, like)
	}
	if status != "" {
		where += " AND status = ?"
		arguments = append(arguments, status)
	}
	return where, arguments
}
func positionFilter(tenantID, keyword, status string) (string, []any) {
	where := "WHERE tenant_id = ?"
	arguments := []any{tenantID}
	if keyword != "" {
		where += " AND (code LIKE ? OR name LIKE ?)"
		like := "%" + keyword + "%"
		arguments = append(arguments, like, like)
	}
	if status != "" {
		where += " AND status = ?"
		arguments = append(arguments, status)
	}
	return where, arguments
}

func userFilter(tenantID string, query application.PageRequest) (string, []any) {
	where := "WHERE tenant_id = ?"
	arguments := []any{tenantID}
	if query.Keyword != "" {
		where += " AND (display_name LIKE ? OR employee_no LIKE ? OR email LIKE ?)"
		like := "%" + query.Keyword + "%"
		arguments = append(arguments, like, like, like)
	}
	if query.Status != "" {
		where += " AND status = ?"
		arguments = append(arguments, query.Status)
	}
	return where, arguments
}
func accountFilter(tenantID string, query application.PageRequest) (string, []any) {
	where := "WHERE tenant_id = ?"
	arguments := []any{tenantID}
	if query.Keyword != "" {
		where += " AND username LIKE ?"
		arguments = append(arguments, "%"+query.Keyword+"%")
	}
	if query.Status != "" {
		where += " AND status = ?"
		arguments = append(arguments, query.Status)
	}
	return where, arguments
}
func membershipFilter(tenantID string, query application.PageRequest) (string, []any) {
	where := "WHERE m.tenant_id = ?"
	arguments := []any{tenantID}
	if query.Keyword != "" {
		where += " AND (u.display_name LIKE ? OR o.name LIKE ?)"
		like := "%" + query.Keyword + "%"
		arguments = append(arguments, like, like)
	}
	if query.Status != "" {
		where += " AND m.status = ?"
		arguments = append(arguments, query.Status)
	}
	return where, arguments
}

func scanUser(scanner interface{ Scan(...any) error }) (domain.User, error) {
	var user domain.User
	var employeeNo, email sql.NullString
	var ciphertext []byte
	err := scanner.Scan(&user.ID, &user.TenantID, &employeeNo, &user.DisplayName, &email, &ciphertext, &user.Status, &user.Version, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return domain.User{}, err
	}
	user.EmployeeNo = stringPointer(employeeNo)
	user.Email = stringPointer(email)
	user.MobileCiphertext = ciphertext
	return user, nil
}
func scanAccount(scanner interface{ Scan(...any) error }) (domain.Account, error) {
	var account domain.Account
	var userID sql.NullString
	var lastLogin sql.NullTime
	err := scanner.Scan(&account.ID, &account.TenantID, &userID, &account.AccountName, &account.Status, &lastLogin, &account.Version, &account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		return domain.Account{}, err
	}
	account.UserID = stringPointer(userID)
	if lastLogin.Valid {
		value := lastLogin.Time.UTC()
		account.LastLoginAt = &value
	}
	return account, nil
}
func scanOrgUnit(scanner interface{ Scan(...any) error }) (domain.OrgUnit, error) {
	var value domain.OrgUnit
	var parent sql.NullString
	err := scanner.Scan(&value.ID, &value.TenantID, &parent, &value.Code, &value.Name, &value.OrgType, &value.Path, &value.Depth, &value.SortOrder, &value.Status, &value.Version)
	if err != nil {
		return domain.OrgUnit{}, err
	}
	value.ParentID = stringPointer(parent)
	return value, nil
}
func scanPosition(scanner interface{ Scan(...any) error }) (domain.Position, error) {
	var value domain.Position
	err := scanner.Scan(&value.ID, &value.TenantID, &value.OrgUnitID, &value.Code, &value.Name, &value.Status, &value.Version)
	if err != nil {
		return domain.Position{}, err
	}
	return value, nil
}
func scanMembership(scanner interface{ Scan(...any) error }) (domain.Membership, error) {
	var value domain.Membership
	var from, to sql.NullTime
	err := scanner.Scan(&value.ID, &value.TenantID, &value.User.ID, &value.User.Name, &value.OrgUnit.ID, &value.OrgUnit.Name, &value.Position.ID, &value.Position.Name, &value.MembershipType, &from, &to, &value.Status, &value.Version, &value.IsPrimary)
	if err != nil {
		return domain.Membership{}, err
	}
	if from.Valid {
		date := from.Time.UTC()
		value.EffectiveFrom = &date
	}
	if to.Valid {
		date := to.Time.UTC()
		value.EffectiveTo = &date
	}
	return value, nil
}
func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copied := value.String
	return &copied
}
func nullableString(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}
func rollback(tx *sql.Tx) { _ = tx.Rollback() }
func mapWriteError(err error, operation string) error {
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
		return application.ErrConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}
