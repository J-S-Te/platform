package infrastructure

import (
	"os"
	"strings"
	"testing"
)

// Keycloak Client 是环境级资源。投影解析有效授权时必须带上本次事件的 environment_id，
// 否则同应用其他环境的环境级绑定会被写进当前 Client（例如仅授予 dev 的角色出现在
// prod Client 里）。平台在线 authorization-context 的环境校验无法纠正已经投出的
// Keycloak Client Role，所以必须在投影源头隔离。
//
// 环境过滤本身由 applicationaccess 的 applicationAccessScopeFilter 覆盖；这里锁定
// 投影链路确实把事件环境透传下去，防止再次退化为"不传环境"。
func TestProjectionLoadsAuthorizationSnapshotForEventEnvironment(t *testing.T) {
	source, err := os.ReadFile("projection_source_gorm.go")
	if err != nil {
		t.Fatalf("read projection source: %v", err)
	}
	if !strings.Contains(string(source), "ResolveKeycloakAuthorizationSnapshot(ctx, event.TenantID, event.IdentityID, event.ApplicationID, event.EnvironmentID)") {
		t.Fatalf("投影必须按事件环境解析授权快照：\n%s", string(source))
	}
}
