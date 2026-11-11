package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegratedReimbursementEnvironmentDoesNotOverrideOtherTenants 验证完整报销配置不进入全局插值。
// 参数：t 为使用临时文件和记录型执行器的单场景上下文，不调用真实 Docker。
// 返回值：无；未选择报销时覆盖平台租户、泄漏配置或丢失专属数据库插值时失败。
func TestIntegratedReimbursementEnvironmentDoesNotOverrideOtherTenants(t *testing.T) {
	_, root, _, gateway := createIntegratedProvisionerFixture(t, integratedCustomerApplicationCode)
	platform := filepath.Join(root, "docker", ".env.local")
	if err := os.WriteFile(platform, []byte("OIDC_TENANT_ID=platform-tenant\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reimbursement := filepath.Join(root, "docker", ".env.reimbursement.local")
	if err := os.WriteFile(reimbursement, []byte("OIDC_TENANT_ID=reimbursement-tenant\n"+
		"REIMBURSEMENT_MYSQL_PASSWORD=isolated-db\n"+
		"REIMBURSEMENT_MYSQL_ROOT_PASSWORD=isolated-root\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingSubsystemRunner{}
	p := &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{
		GatewayScriptPath: gateway, DockerBinary: "docker", PlatformComposeProject: "isolated"}, runner: runner}
	if err := p.runIntegratedPlatformCompose(context.Background(), "build", "customer-api"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatal("没有记录到唯一Compose命令")
	}
	call := runner.calls[0]
	values := map[string]string{}
	for index, arg := range call.arguments {
		if arg != "--env-file" {
			continue
		}
		read, err := readEnvironmentValues(call.arguments[index+1])
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range read {
			values[key] = value
		}
	}
	if values["OIDC_TENANT_ID"] != "platform-tenant" {
		t.Fatal("未选报销的Compose命令仍被报销配置覆盖全局租户")
	}
	assertReimbursementComposeEnvironment(t, call, reimbursement)
}

// assertReimbursementComposeEnvironment 检查报销仅以服务文件和两个专属数据库键参与执行。
// 参数：t 为测试上下文，call 为记录的Compose调用，path 为临时完整报销文件。
// 返回值：无；完整文件进入全局插值、数据库键缺失或服务级路径丢失时失败。
func assertReimbursementComposeEnvironment(t *testing.T, call recordingSubsystemRunnerCall, path string) {
	t.Helper()
	for index, arg := range call.arguments {
		if arg == "--env-file" && call.arguments[index+1] == path {
			t.Fatal("完整报销配置进入全局插值")
		}
	}
	wanted := map[string]bool{
		"REIMBURSEMENT_RUNTIME_ENV_FILE=" + path:          false,
		"REIMBURSEMENT_MYSQL_PASSWORD=isolated-db":        false,
		"REIMBURSEMENT_MYSQL_ROOT_PASSWORD=isolated-root": false,
	}
	for _, entry := range call.environment {
		if _, exists := wanted[entry]; exists {
			wanted[entry] = true
		}
	}
	for _, found := range wanted {
		if !found {
			t.Fatal("服务级配置或专属数据库插值缺失")
		}
	}
}

// TestIntegratedSelectedReimbursementUsesControlledInterpolation 验证所选报销只使用受控专属插值。
// 参数：t 为临时配置与记录型执行器，不运行真实 Docker。
// 返回值：无；继承环境覆盖受控值、出现重复键或秘密进入全局插值时失败。
func TestIntegratedSelectedReimbursementUsesControlledInterpolation(t *testing.T) {
	_, root, _, gateway := createIntegratedProvisionerFixture(t, integratedCustomerApplicationCode)
	path := filepath.Join(root, "docker", ".env.reimbursement.local")
	if err := os.WriteFile(path, []byte("OIDC_TENANT_ID=reimbursement-tenant\n"+
		"PROJECT_REFERENCE_CLIENT_SECRET=service-only-secret\n"+
		"REIMBURSEMENT_MYSQL_PASSWORD=isolated-db\n"+
		"REIMBURSEMENT_MYSQL_ROOT_PASSWORD=isolated-root\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"REIMBURSEMENT_MYSQL_PASSWORD", "REIMBURSEMENT_MYSQL_ROOT_PASSWORD",
		"REIMBURSEMENT_RUNTIME_ENV_FILE"} {
		t.Setenv(key, "stale-inherited-value")
	}
	runner := &recordingSubsystemRunner{}
	p := &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{
		GatewayScriptPath: gateway, DockerBinary: "docker", PlatformComposeProject: "isolated"}, runner: runner}
	if err := p.runIntegratedPlatformCompose(context.Background(), "build", "reimbursement-api"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatal("未捕获唯一编排命令")
	}
	call := runner.calls[0]
	assertReimbursementComposeEnvironment(t, call, path)
	counts := map[string]int{}
	for _, entry := range call.environment {
		key, _, _ := strings.Cut(entry, "=")
		counts[key]++
		if strings.Contains(entry, "stale-inherited-value") || strings.Contains(entry, "service-only-secret") {
			t.Fatal("继承环境或服务秘密污染全局插值")
		}
	}
	for _, key := range []string{"REIMBURSEMENT_MYSQL_PASSWORD", "REIMBURSEMENT_MYSQL_ROOT_PASSWORD",
		"REIMBURSEMENT_RUNTIME_ENV_FILE"} {
		if counts[key] != 1 {
			t.Fatal("受控键未收敛为唯一值", key, counts[key])
		}
	}
}

// TestReimbursementComposeEnvironmentRejectsUnsafeFiles 验证存在的配置必须是私密普通文件。
// 参数：t 为逐项执行的本地路径边界场景，不读工程运行配置。
// 返回值：无；链接、目录或宽权限配置被接受，或错误泄漏配置内容时失败。
func TestReimbursementComposeEnvironmentRejectsUnsafeFiles(t *testing.T) {
	for _, mode := range []string{"symlink", "directory", "public"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "controlled.env")
			switch mode {
			case "symlink":
				target := filepath.Join(root, "secret.env")
				if err := os.WriteFile(target, []byte("REIMBURSEMENT_MYSQL_PASSWORD=do-not-expose\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(path, []byte("REIMBURSEMENT_MYSQL_PASSWORD=do-not-expose\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := reimbursementComposeEnvironment(path, false)
			if err == nil || strings.Contains(err.Error(), "do-not-expose") {
				t.Fatal("未拒绝不安全配置或泄漏内容")
			}
		})
	}
}

// TestReimbursementComposeEnvironmentMissingSelectionBoundary 验证缺失配置只允许未选择报销时解析。
// 参数：t 为单个临时文件缺失边界场景。
// 返回值：无；未选择时误阻断其他模块或已选择时错误放行则失败。
func TestReimbursementComposeEnvironmentMissingSelectionBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.env")
	file, values, err := reimbursementComposeEnvironment(path, false)
	if err != nil || file != os.DevNull || len(values) != 0 {
		t.Fatal("未选择报销时缺失配置错误阻断", err)
	}
	if _, _, err = reimbursementComposeEnvironment(path, true); err == nil {
		t.Fatal("已选择报销却允许缺失配置")
	}
}
