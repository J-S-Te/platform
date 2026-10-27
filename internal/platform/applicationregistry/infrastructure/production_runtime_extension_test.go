package infrastructure

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestProductionUpdateInitializesInheritedExtension 用途：覆盖旧系统增加继承式 Worker 配置的受控升级。
// 参数：t 为无业务依赖的部署测试上下文。
// 返回值：无。
func TestProductionUpdateInitializesInheritedExtension(t *testing.T) {
	p, _, _ := productionProvisionerFixture(t)
	input := productionContractInput("https://platform.example.com")
	if err := p.Provision(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
	mainPath := filepath.Join(target.config.DeployRoot, "runtime/contract.env")
	file, err := os.OpenFile(mainPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	contentKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	descriptorKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	_, writeErr := file.WriteString("\nPORTAL_REPORT_CONTENT_KEY_BASE64=" + contentKey +
		"\nPORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64=" + descriptorKey +
		"\nPORTAL_OIDC_TENANT_ID=tenant-1\nPORTAL_REPORT_STORE_ROOT=/report-vault\n")
	if closeErr := file.Close(); writeErr != nil || closeErr != nil {
		t.Fatal("fixture runtime write failed")
	}
	template := filepath.Join(target.config.DeployRoot, "extension.env.example")
	if err := os.WriteFile(template, []byte("EXTENSION_MODE=worker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target.config.Profile.Manifest.Runtime.Files = append(target.config.Profile.Manifest.Runtime.Files,
		productionSubsystemRuntimeFileManifest{Path: "runtime/extension.env", TemplatePath: "extension.env.example",
			InheritFrom: "runtime/contract.env", InheritKeys: []string{"PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_OIDC_TENANT_ID", "PORTAL_REPORT_STORE_ROOT"},
			Values: map[string]string{"EXTENSION_MODE": "worker"}})
	if err := p.Update(context.Background(), input); err != nil {
		t.Fatalf("update new approved extension: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(target.config.DeployRoot, "runtime/extension.env"))
	if err != nil {
		t.Fatal(err)
	}
	values := parseEnvironmentValues(string(raw))
	if values["PORTAL_REPORT_CONTENT_KEY_BASE64"] != contentKey || values["EXTENSION_MODE"] != "worker" {
		t.Fatal("extension inheritance missing")
	}
	// Existing application runtime must never be reconstructed when lost.
	if err := os.Remove(filepath.Join(target.config.DeployRoot, "runtime/contract.env")); err != nil {
		t.Fatal(err)
	}
	if err := p.Update(context.Background(), application.SubsystemProvisioningInput{
		TenantID: input.TenantID, ApplicationCode: input.ApplicationCode, Environment: input.Environment,
	}); err == nil {
		t.Fatal("missing application secrets were silently recreated")
	}
}

// TestReviewedPortalExtensionKeepsVaultKeys 用途：用真实门户审核清单验证补建报告 Worker 配置不轮换内容密钥。
// 参数：t 为隔离文件测试上下文。
// 返回值：无。
func TestReviewedPortalExtensionKeepsVaultKeys(t *testing.T) {
	target, input := reviewedP1PortalFixture(t)
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	apiPath := filepath.Join(target.config.DeployRoot, "runtime/portal.env")
	before, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(target.config.DeployRoot, "runtime/portal-report-worker.env")
	if err := os.Remove(workerPath); err != nil {
		t.Fatal(err)
	}
	file := target.config.Profile.Manifest.Runtime.Files[1]
	source, err := os.ReadFile(filepath.Join(platformModuleRoot(t), "deploy/production", file.TemplatePath))
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(target.config.DeployRoot, file.TemplatePath)
	if err := os.MkdirAll(filepath.Dir(template), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(template, source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.initializeInheritedRuntimeExtensions(input); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	after, _ := readEnvironmentValues(apiPath)
	worker, _ := readEnvironmentValues(workerPath)
	previous := parseEnvironmentValues(string(before))
	for _, key := range file.InheritKeys {
		if worker[key] != previous[key] || after[key] != previous[key] {
			t.Fatalf("vault binding changed: %s", key)
		}
	}
	info, err := os.Stat(workerPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private worker file permissions invalid")
	}
}
