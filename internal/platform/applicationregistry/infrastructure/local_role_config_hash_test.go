package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roleHashRunner 覆盖两种只读投影：compose config 与一次性容器输出。
type roleHashRunner struct {
	compose string
	stdout  string
	runErr  error
	calls   [][]string
}

func (r *roleHashRunner) Run(context.Context, string, []string, string, ...string) error { return nil }

func (r *roleHashRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "compose" {
		return []byte(r.compose), nil
	}
	if r.runErr != nil {
		return nil, r.runErr
	}
	return []byte(r.stdout), nil
}

func roleHashFixture(t *testing.T, runner *roleHashRunner) *LocalDockerSubsystemProvisioner {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"platform/scripts", "platform/docker", "contract_management", "project_management", "data_analysis", "Settlement"} {
		if err = os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"platform/docker/.env.local", "platform/docker/.env.customer.local", "platform/docker/.env.portal.local", "contract_management/.env.local", "project_management/.env.local", "data_analysis/.env.local", "Settlement/.env.local", "platform/compose.local.yaml"} {
		if err = os.WriteFile(filepath.Join(root, path), []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root,
		GatewayScriptPath: filepath.Join(root, "platform", "scripts", "portal-gateway.sh"),
		PlatformComposeProject: "basic-platform-local",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocalRoleConfigHashUsesImageValueAndFallsBackToConstant(t *testing.T) {
	const imageHash = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	runner := &roleHashRunner{
		compose: `{"services":{"portal-api":{"image":"portal:local"},"customer-api":{"image":"crm:local"}}}`,
		stdout:  "application=portal\ncatalog_version=1\nclaims_role_config_hash=" + imageHash + "\n",
	}
	p := roleHashFixture(t, runner)
	if got := p.localRoleConfigHash("portal"); got != integratedPortalRoleConfigHash {
		t.Fatalf("未读取镜像前必须回退常量，got %q", got)
	}
	p.refreshRoleConfigHashes(context.Background())
	if got := p.localRoleConfigHash("portal"); got != imageHash {
		t.Fatalf("应优先使用镜像内嵌哈希，got %q", got)
	}
	// CRM 镜像同样解析；这里 run 输出被复用，验证键位映射与解析器一致。
	if got := p.localRoleConfigHash("crm"); got != imageHash {
		t.Fatalf("crm 哈希解析失败，got %q", got)
	}
}

func TestLocalRoleConfigHashKeepsConstantWhenImageReadFails(t *testing.T) {
	runner := &roleHashRunner{
		compose: `{"services":{"portal-api":{"image":"portal:local"},"customer-api":{"image":"crm:local"}}}`,
		runErr:  errors.New("docker run unavailable"),
	}
	p := roleHashFixture(t, runner)
	p.refreshRoleConfigHashes(context.Background())
	if got := p.localRoleConfigHash("portal"); got != integratedPortalRoleConfigHash {
		t.Fatalf("读取失败必须保留审核常量，got %q", got)
	}
	if got := p.localRoleConfigHash("crm"); got != integratedCustomerRoleConfigHash {
		t.Fatalf("crm 读取失败必须保留审核常量，got %q", got)
	}
}

func TestLocalRoleConfigHashIgnoresMalformedOutput(t *testing.T) {
	runner := &roleHashRunner{
		compose: `{"services":{"portal-api":{"image":"portal:local"}}}`,
		stdout:  "application=portal\ncatalog_version=1\n",
	}
	p := roleHashFixture(t, runner)
	p.refreshRoleConfigHashes(context.Background())
	if got := p.localRoleConfigHash("portal"); got != integratedPortalRoleConfigHash {
		t.Fatalf("缺少哈希行时必须回退常量，got %q", got)
	}
	if len(runner.calls) == 0 || !strings.Contains(strings.Join(runner.calls[0], " "), "config") {
		t.Fatalf("compose config 未被调用: %v", runner.calls)
	}
}
