package infrastructure

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// statementCaptureLogger 记录 GORM 构建出的每一条 SQL。配合 DryRun 使用时不连接数据库，
// 只验证查询口径。
type statementCaptureLogger struct {
	logger.Interface
	mutex      sync.Mutex
	statements []string
}

func (capture *statementCaptureLogger) Trace(_ context.Context, _ time.Time, render func() (string, int64), _ error) {
	sql, _ := render()
	capture.mutex.Lock()
	defer capture.mutex.Unlock()
	capture.statements = append(capture.statements, sql)
}

func (capture *statementCaptureLogger) all() []string {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()
	return append([]string(nil), capture.statements...)
}

// newStatementCapturingDryRunDatabase 复用既有 dry-run 数据库，但替换日志器以便断言实际
// 构建的 SQL；不建立真实连接，因此不会触及任何业务库。
func newStatementCapturingDryRunDatabase(t *testing.T) (*gorm.DB, *statementCaptureLogger) {
	t.Helper()
	database := newDryRunManagementDatabase(t)
	capture := &statementCaptureLogger{}
	replacement := database.Session(&gorm.Session{Logger: capture, DryRun: true, SkipDefaultTransaction: true})
	return replacement, capture
}

func statementsContaining(statements []string, fragment string) []string {
	matched := make([]string, 0, len(statements))
	for _, statement := range statements {
		if strings.Contains(statement, fragment) {
			matched = append(matched, statement)
		}
	}
	return matched
}

func assertStatementFragment(t *testing.T, statements []string, fragment string) {
	t.Helper()
	if len(statementsContaining(statements, fragment)) == 0 {
		t.Fatalf("no statement contains %q; statements = %v", fragment, statements)
	}
}

// IAM-01：受限范围必须下推为“有效任职组织”谓词，而不是先取全租户再内存过滤。
func TestApplyUserManagementScopePushesEffectiveMembershipPredicate(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)
	query := application.PageRequest{ScopeRestricted: true, AllowedOrgUnitIDs: []string{"org-1", "org-2"}, AllowedResourceIDs: []string{"membership-1"}}

	result := applyUserManagementScope(database.Table("iam_user"), query).Find(&[]userModel{})
	if result.Error != nil {
		t.Fatalf("build scoped user query: %v", result.Error)
	}
	statement := result.Statement.SQL.String()
	for _, fragment := range []string{"scope_membership", "scope_membership.user_id = iam_user.id", "scope_membership.status", "scope_membership.valid_from", "scope_membership.valid_until", "scope_membership.org_unit_id IN"} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("scoped user SQL is missing %q; SQL=%s", fragment, statement)
		}
	}
	// 资源级绑定不映射到人员归属，不能出现在用户范围谓词里。
	if strings.Contains(statement, "membership-1") {
		t.Fatalf("resource scope must not widen user management; SQL=%s", statement)
	}
}

// IAM-01：账号范围经属主用户的有效任职判定；没有属主用户的服务账号不属于任何组织范围。
func TestApplyAccountManagementScopeJoinsOwnerMemberships(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)
	result := applyAccountManagementScope(database.Table("iam_account"), application.PageRequest{ScopeRestricted: true, AllowedOrgUnitIDs: []string{"org-1"}}).Find(&[]accountModel{})
	if result.Error != nil {
		t.Fatalf("build scoped account query: %v", result.Error)
	}
	statement := result.Statement.SQL.String()
	for _, fragment := range []string{"scope_membership", "scope_membership.user_id = iam_account.user_id", "scope_membership.org_unit_id IN"} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("scoped account SQL is missing %q; SQL=%s", fragment, statement)
		}
	}
}

// 受限但没有任何授权组织（例如只有资源绑定）时必须显式失败关闭，不能退化为全租户。
func TestManagementScopePredicateFailsClosedWithoutOrganizations(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)

	userResult := applyUserManagementScope(database.Table("iam_user"), application.PageRequest{ScopeRestricted: true, AllowedResourceIDs: []string{"usr-1"}}).Find(&[]userModel{})
	if userResult.Error != nil {
		t.Fatalf("build empty scoped user query: %v", userResult.Error)
	}
	if statement := userResult.Statement.SQL.String(); !strings.Contains(statement, "1 = 0") {
		t.Fatalf("resource-only user scope must fail closed; SQL=%s", statement)
	}

	accountResult := applyAccountManagementScope(database.Table("iam_account"), application.PageRequest{ScopeRestricted: true, AllowedResourceIDs: []string{"acc-1"}}).Find(&[]accountModel{})
	if accountResult.Error != nil {
		t.Fatalf("build empty scoped account query: %v", accountResult.Error)
	}
	if statement := accountResult.Statement.SQL.String(); !strings.Contains(statement, "1 = 0") {
		t.Fatalf("resource-only account scope must fail closed; SQL=%s", statement)
	}
}

// 未受限查询必须与既有行为完全一致：不注入任何范围谓词。
func TestManagementScopePredicateIsSkippedWhenUnrestricted(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)

	result := applyUserManagementScope(database.Table("iam_user"), application.PageRequest{}).Find(&[]userModel{})
	if result.Error != nil {
		t.Fatalf("build unrestricted user query: %v", result.Error)
	}
	if statement := result.Statement.SQL.String(); strings.Contains(statement, "scope_membership") || strings.Contains(statement, "1 = 0") {
		t.Fatalf("unrestricted query must not be filtered; SQL=%s", statement)
	}
}

// IAM-01：列表的总数与条目必须同口径——仓储在 Count 与分页 Find 之前一次性应用范围谓词，
// 因此从同一基查询派生的两条语句都必须带上范围条件。
func TestListUsersCountAndItemsShareTheManagementScopePredicate(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)
	query := application.PageRequest{Page: 1, PageSize: 20, ScopeRestricted: true, AllowedOrgUnitIDs: []string{"org-1"}}

	scoped := applyUserManagementScope(applyUserFilter(database.Model(&userModel{}), "tenant-1", query), query)
	var total int64
	countResult := scoped.Session(&gorm.Session{}).Count(&total)
	if countResult.Error != nil {
		t.Fatalf("count scoped users: %v", countResult.Error)
	}
	var rows []userModel
	itemsResult := scoped.Session(&gorm.Session{}).Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows)
	if itemsResult.Error != nil {
		t.Fatalf("list scoped users: %v", itemsResult.Error)
	}

	for name, result := range map[string]*gorm.DB{"count": countResult, "items": itemsResult} {
		statement := result.Statement.SQL.String()
		assertStatementFragment(t, []string{statement}, "scope_membership.org_unit_id IN")
		if !strings.Contains(statement, "scope_membership.user_id = iam_user.id") {
			t.Fatalf("%s statement lost the owner predicate; SQL=%s", name, statement)
		}
	}
}

func TestListAccountsCountAndItemsShareTheManagementScopePredicate(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)
	query := application.PageRequest{Page: 1, PageSize: 20, ScopeRestricted: true, AllowedOrgUnitIDs: []string{"org-1"}}

	scoped := applyAccountManagementScope(applyAccountFilter(database.Model(&accountModel{}), "tenant-1", query), query)
	var total int64
	countResult := scoped.Session(&gorm.Session{}).Count(&total)
	if countResult.Error != nil {
		t.Fatalf("count scoped accounts: %v", countResult.Error)
	}
	var rows []accountModel
	itemsResult := scoped.Session(&gorm.Session{}).Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows)
	if itemsResult.Error != nil {
		t.Fatalf("list scoped accounts: %v", itemsResult.Error)
	}

	for name, result := range map[string]*gorm.DB{"count": countResult, "items": itemsResult} {
		statement := result.Statement.SQL.String()
		if !strings.Contains(statement, "scope_membership.user_id = iam_account.user_id") {
			t.Fatalf("%s statement lost the account owner scope predicate; SQL=%s", name, statement)
		}
	}
}

// 按 ID 范围判定与列表范围判定共用“有效任职”口径：只统计 ACTIVE 且落在有效窗口内的任职。
func TestEffectiveMembershipQueryDefinesTheSharedScopeWindow(t *testing.T) {
	database, _ := newStatementCapturingDryRunDatabase(t)
	now := time.Date(2026, time.October, 30, 8, 30, 0, 0, time.UTC)

	result := effectiveMembershipQuery(database.Table("iam_membership"), "tenant-1", "user-1", now).Find(&[]membershipModel{})
	if result.Error != nil {
		t.Fatalf("build effective membership query: %v", result.Error)
	}
	statement := result.Statement.SQL.String()
	for _, fragment := range []string{"status", "valid_from IS NULL OR valid_from <=", "valid_until IS NULL OR valid_until >", "tenant_id", "user_id"} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("effective membership SQL is missing %q; SQL=%s", fragment, statement)
		}
	}
}
