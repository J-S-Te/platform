package application

import (
	"context"
	"errors"
	"testing"
)

type projectionStub struct {
	roleCodes     []string
	listErr       error
	ensureErr     error
	saveErr       error
	backfillErr   error
	readinessErr  error
	ensuredClient string
	ensuredRoles  []string
	steps         []string
}

func (stub *projectionStub) ListKeycloakRoleCodes(context.Context, string, string) ([]string, error) {
	stub.steps = append(stub.steps, "list")
	return stub.roleCodes, stub.listErr
}
func (stub *projectionStub) EnsureClientRoles(_ context.Context, clientID string, roles []string) error {
	stub.steps = append(stub.steps, "ensure")
	stub.ensuredClient, stub.ensuredRoles = clientID, roles
	return stub.ensureErr
}
func (stub *projectionStub) SaveKeycloakClientMapping(context.Context, string, string, string, string, string) error {
	stub.steps = append(stub.steps, "save")
	return stub.saveErr
}
func (stub *projectionStub) BackfillKeycloakAuthorization(context.Context, string, string, string) error {
	stub.steps = append(stub.steps, "backfill")
	return stub.backfillErr
}
func (stub *projectionStub) MarkKeycloakClientAndRoleCatalogSynced(context.Context, string, string, string) error {
	stub.steps = append(stub.steps, "readiness")
	return stub.readinessErr
}

func projectionInput() KeycloakProjectionInput {
	return KeycloakProjectionInput{TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1", ClientID: "settlement-dev-web", Realm: "basic"}
}

// 非 Keycloak 接入（无 ClientID）必须跳过投影，不能误创建角色或映射。
func TestKeycloakProjectionSkipsWithoutClient(t *testing.T) {
	stub := &projectionStub{}
	service, err := NewKeycloakAuthorizationProjectionService(stub, stub, stub, stub)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Project(context.Background(), KeycloakProjectionInput{TenantID: "t", ApplicationID: "a", EnvironmentID: "e"})
	if err != nil || result.Projected || len(stub.steps) != 0 {
		t.Fatalf("result=%+v err=%v steps=%v，无 ClientID 时必须跳过", result, err, stub.steps)
	}
}

// 正常顺序：角色目录 → 确保角色 → 保存映射 → 回填投影 → 就绪标记。
func TestKeycloakProjectionRunsFullChain(t *testing.T) {
	stub := &projectionStub{roleCodes: []string{"crm.admin", "crm.viewer"}}
	service, _ := NewKeycloakAuthorizationProjectionService(stub, stub, stub, stub)
	result, err := service.Project(context.Background(), projectionInput())
	if err != nil || !result.Projected || result.ErrorCode != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"list", "ensure", "save", "backfill", "readiness"}
	if len(stub.steps) != len(want) {
		t.Fatalf("steps=%v", stub.steps)
	}
	for i := range want {
		if stub.steps[i] != want[i] {
			t.Fatalf("steps=%v，want %v", stub.steps, want)
		}
	}
	if stub.ensuredClient != "settlement-dev-web" || len(stub.ensuredRoles) != 2 {
		t.Fatalf("ensured=%s roles=%v", stub.ensuredClient, stub.ensuredRoles)
	}
}

// 每步失败都要给稳定错误码（与旧流程一一对应），不得吞错。
func TestKeycloakProjectionReportsStableCodes(t *testing.T) {
	cases := []struct {
		name  string
		stub  *projectionStub
		code  string
		steps int
	}{
		{"角色目录读取", &projectionStub{listErr: errors.New("db down")}, KeycloakProjectionCodeRoleCatalogSyncFail, 1},
		{"确保角色", &projectionStub{ensureErr: errors.New("keycloak 500")}, KeycloakProjectionCodeRoleCatalogSyncFail, 2},
		{"保存映射", &projectionStub{saveErr: errors.New("conflict")}, KeycloakProjectionCodeClientMappingFail, 3},
		{"回填投影", &projectionStub{backfillErr: errors.New("queue full")}, KeycloakProjectionCodeBackfillFail, 4},
		{"就绪写入", &projectionStub{readinessErr: errors.New("write failed")}, KeycloakProjectionCodeReadinessFail, 5},
	}
	for _, item := range cases {
		service, _ := NewKeycloakAuthorizationProjectionService(item.stub, item.stub, item.stub, item.stub)
		result, err := service.Project(context.Background(), projectionInput())
		if err != nil {
			t.Fatalf("%s: err=%v", item.name, err)
		}
		if result.ErrorCode != item.code {
			t.Fatalf("%s: code=%q，want %q", item.name, result.ErrorCode, item.code)
		}
		if len(item.stub.steps) != item.steps {
			t.Fatalf("%s: steps=%v", item.name, item.stub.steps)
		}
	}
}
