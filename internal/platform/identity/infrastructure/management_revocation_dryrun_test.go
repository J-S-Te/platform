package infrastructure

import (
	"strings"
	"testing"
	"time"
)

// IAM-02：撤销必须在没有任何活跃会话时继续执行。已停用/已删除用户、会话已过期的账号仍可能
// 持有可刷新的 OAuth 令牌族与未消费授权码，“没有会话”不能成为跳过撤销的理由。
func TestRevokeAccountSessionsTxRevokesTokensWithoutActiveSessions(t *testing.T) {
	database, capture := newStatementCapturingDryRunDatabase(t)
	repository := &GORMRepository{database: database}
	revokedAt := time.Date(2026, time.October, 30, 8, 30, 0, 0, time.UTC)

	hadActiveSessions, err := repository.revokeAccountSessionsTx(database, "tenant-1", "account-1", revokedAt, "USER_DISABLED")
	if err != nil {
		t.Fatalf("revokeAccountSessionsTx() error = %v", err)
	}
	if hadActiveSessions {
		t.Fatal("dry-run without session rows must report no active sessions")
	}

	statements := capture.all()
	// 会话锁定、令牌族查询、令牌族撤销、授权码撤销与本地会话撤销都必须发生，即使没有会话行。
	for _, fragment := range []string{
		"FROM `iam_session`",
		"FROM `oauth_token_family`",
		"oauth_authorization_code",
		"tenant_id = 'tenant-1'",
		"account_id = 'account-1'",
	} {
		assertStatementFragment(t, statements, fragment)
	}

	familyRevocations := statementsContaining(statements, "UPDATE `oauth_token_family`")
	if len(familyRevocations) == 0 {
		t.Fatalf("OAuth token families must be revoked even without sessions; statements = %v", statements)
	}
	for _, statement := range familyRevocations {
		if !strings.Contains(statement, "REVOKED") {
			t.Fatalf("token family revocation must set REVOKED; SQL=%s", statement)
		}
	}

	codeRevocations := statementsContaining(statements, "oauth_authorization_code")
	if len(codeRevocations) == 0 {
		t.Fatalf("unused authorization codes must be revoked even without sessions; statements = %v", statements)
	}
	if len(statementsContaining(codeRevocations, "REVOKED")) == 0 {
		t.Fatalf("authorization code revocation must set REVOKED; statements = %v", codeRevocations)
	}

	sessionRevocations := statementsContaining(statements, "UPDATE `iam_session`")
	if len(sessionRevocations) == 0 {
		t.Fatalf("account sessions must be revoked; statements = %v", statements)
	}
	if len(statementsContaining(sessionRevocations, "REVOKED")) == 0 {
		t.Fatalf("session revocation must set REVOKED; statements = %v", sessionRevocations)
	}
}

// IAM-02：撤销必须由调用方的业务事务句柄执行，缺失事务时失败关闭而不是另开隐式事务。
func TestRevokeAccountSessionsTxRequiresCallerTransaction(t *testing.T) {
	repository := &GORMRepository{}
	if _, err := repository.revokeAccountSessionsTx(nil, "tenant-1", "account-1", time.Now().UTC(), "USER_DISABLED"); err == nil {
		t.Fatal("revokeAccountSessionsTx must reject a nil transaction")
	}
}
