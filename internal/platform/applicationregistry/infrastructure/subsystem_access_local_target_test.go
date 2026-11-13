package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestLocalAccessConfigurationSnapshot 验证真实本机能力可生成受控配置快照。
// 参数：t 为测试上下文，文件和镜像事实均在隔离夹具内。
// 返回值：无；合法本机目标被误拒或镜像变更未失效时失败。
func TestLocalAccessConfigurationSnapshot(t *testing.T) {
	catalog := localAccessCatalogFixture(t)
	images := imageReaderStub{reported: true, images: []application.SubsystemImageReadiness{
		{ServiceID: "project-api", ImageDigest: "sha256:" + strings.Repeat("a", 64), Present: true},
		{ServiceID: "sla-notifier", ImageDigest: "sha256:" + strings.Repeat("a", 64), Present: true},
	}}
	tracker := &SubsystemAccessConfigurationTracker{catalog: catalog}
	tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, images)
	op := application.SubsystemAccessOperation{
		ApplicationCode: "project_management", EnvironmentCode: "dev", TenantID: "tenant-1",
	}
	first, err := tracker.Snapshot(context.Background(), op)
	if err != nil || len(first) != 64 {
		t.Fatalf("合法本机目标无法检查配置：digest=%q err=%v", first, err)
	}
	images.images[0].ImageDigest = "sha256:" + strings.Repeat("b", 64)
	tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, images)
	next, err := tracker.Snapshot(context.Background(), op)
	if err != nil || next == first {
		t.Fatalf("候选镜像变化必须使预览失效：digest=%q err=%v", next, err)
	}
	images.images[0].ImageDigest = "project-management/backend:local"
	tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, images)
	if _, err := tracker.Snapshot(context.Background(), op); err == nil {
		t.Fatal("本机标签不能冒充已核验的不可变镜像")
	}
	images.images[0].ImageDigest = "sha256:" + strings.Repeat("a", 64)
	tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, images)
	if err := os.Remove(filepath.Join(catalog.config.ProjectsRoot,
		"project_management/.env.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.Snapshot(context.Background(), op); err == nil {
		t.Fatal("已移除的本机安装不能继续通过条件检查")
	}
}

// localAccessCatalogFixture 创建真实本机执行器所需的最小工作区。
// 参数：t 为测试上下文，临时目录由测试框架清理。
// 返回值：返回只读本机能力来源，不连接 Docker 或业务数据库。
func localAccessCatalogFixture(t *testing.T) *LocalDockerSubsystemProvisioner {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"platform/scripts/gateway.sh", "platform/compose.local.yaml",
		"platform/docker/.env.local", "platform/docker/.env.customer.local",
		"contract_management/.env.local", "project_management/.env.example",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture-private-value"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root,
		GatewayScriptPath:  filepath.Join(root, "platform/scripts/gateway.sh"),
		GatewayIncludePath: filepath.Join(root, "platform/docker/gateway.conf"),
	}}
}

// TestLocalAccessTargetBoundaries 验证本机兼容不扩大生产清单或未知模式范围。
// 参数：t 为测试上下文。
// 返回值：无；模式、禁用、应用或环境边界被放宽时失败。
func TestLocalAccessTargetBoundaries(t *testing.T) {
	op := application.SubsystemAccessOperation{ApplicationCode: "project_management", EnvironmentCode: "dev"}
	for _, tt := range []struct {
		name, mode, app, env string
		enabled, want        bool
	}{
		{"local", "local", "project_management", "dev", true, true},
		{"disabled", "local", "project_management", "dev", false, false},
		{"production", "production", "project_management", "dev", true, false},
		{"unknown mode", "other", "project_management", "dev", true, false},
		{"unsupported app", "local", "contract_management", "dev", true, false},
		{"unsupported environment", "local", "project_management", "test", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			catalog := accessCatalogStub{capabilities: application.SubsystemProvisioningCapabilities{
				Mode: tt.mode, Enabled: tt.enabled,
				SupportedApplicationCodes: []string{tt.app}, SupportedEnvironments: []string{tt.env},
			}}
			target, found, err := resolveSubsystemAccessTarget(context.Background(), catalog, op)
			if err != nil || found != tt.want {
				t.Fatalf("found=%v want=%v err=%v", found, tt.want, err)
			}
			if found && (target.UpstreamURL != "http://project-api:8082" ||
				target.PathPrefix != "/project_management" ||
				!application.ValidSubsystemAccessChecksum(target.ManifestChecksum)) {
				t.Fatalf("本机项目目标未绑定正确拓扑和配置摘要：%+v", target)
			}
		})
	}
}

// TestProductionAccessSnapshotRejectsLocalDigest 验证服务器不能接受本机镜像标识。
// 参数：t 为测试上下文。
// 返回值：无；生产接受裸镜像ID或忽略精确清单目标时失败。
func TestProductionAccessSnapshotRejectsLocalDigest(t *testing.T) {
	catalog := accessCatalogWithTarget()
	catalog.capabilities.Mode = "production"
	tracker := &SubsystemAccessConfigurationTracker{catalog: catalog}
	tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, imageReaderStub{
		reported: true, images: []application.SubsystemImageReadiness{{
			ServiceID: "settlement-api", ImageDigest: "sha256:" + strings.Repeat("a", 64), Present: true,
		}},
	})
	if _, err := tracker.Snapshot(context.Background(), onboardingOperation()); err == nil {
		t.Fatal("生产只允许带仓库的不可变摘要")
	}
}

// TestLocalAccessPreflightUsesControlledTopology 验证检查阶段和部署阶段解析同一本机目标。
// 参数：t 为测试上下文。
// 返回值：无；合法本机目标被检查阶段误拒或拓扑不符时失败。
func TestLocalAccessPreflightUsesControlledTopology(t *testing.T) {
	service := &onboardingServiceStub{}
	adapter, err := NewSubsystemAccessOnboardingAdapter(service,
		localAccessCatalogFixture(t), registryLookupStub{})
	if err != nil {
		t.Fatal(err)
	}
	op := application.SubsystemAccessOperation{
		TenantID: "tenant-1", OperatorID: "operator-1", PublicOrigin: "http://localhost:8081",
		ApplicationCode: "project_management", EnvironmentCode: "dev",
		Action: application.SubsystemAccessActionOnboard,
	}
	outcome, err := adapter.PreflightSubsystemAccess(context.Background(), op)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("合法本机检查被拒：%+v err=%v", outcome, err)
	}
	input := service.seenOnboard
	if input.UpstreamURL != "http://project-api:8082" || input.PathPrefix != "/project_management" ||
		input.TenantID != "tenant-1" || input.AllowedServiceBindings != nil {
		t.Fatalf("拓扑、租户或默认用途被改写：%+v", input)
	}
	op.Action = application.SubsystemAccessActionUpdate
	if result, err := adapter.PreflightSubsystemAccess(context.Background(), op); err != nil ||
		result.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("已登记应用更新不应被误判范围外：%+v err=%v", result, err)
	}
}

// TestLocalAccessReceiptUsesSupportedEnvironments 验证所有本机受支持环境均可恢复真实回执。
// 参数：t 为临时文件测试上下文，不运行容器或连接业务库。
// 返回值：无；合法旧代次回执被拒、越界环境或不同租户命中时失败。
func TestLocalAccessReceiptUsesSupportedEnvironments(t *testing.T) {
	p := localAccessCatalogFixture(t)
	for _, environment := range []string{"dev", "test", "staging", "prod"} {
		t.Run(environment, func(t *testing.T) {
			receipt := application.SubsystemAccessDeploymentReceipt{
				TenantID: "tenant-1", OperationID: "operation-1", Generation: 2,
				ApplicationCode: "project_management", Environment: environment,
				ManifestChecksum: strings.Repeat("a", 64),
			}
			input := application.SubsystemProvisioningInput{
				TenantID: receipt.TenantID, ApplicationCode: receipt.ApplicationCode,
				Environment: environment, ManifestChecksum: receipt.ManifestChecksum,
				AccessReceipt: &receipt,
			}
			if err := writeAccessDeploymentReceipt(p.localPlatformRoot(), input); err != nil {
				t.Fatal(err)
			}
			if found, err := p.ReadAccessDeploymentCompletion(context.Background(), receipt); err != nil || !found {
				t.Fatalf("真实本机回执不可恢复：found=%v err=%v", found, err)
			}
			receipt.TenantID = "tenant-2"
			if found, err := p.ReadAccessDeploymentCompletion(context.Background(), receipt); err != nil || found {
				t.Fatalf("不同租户不应命中既有回执：found=%v err=%v", found, err)
			}
		})
	}
	if _, err := p.ReadAccessDeploymentCompletion(context.Background(),
		application.SubsystemAccessDeploymentReceipt{
			ApplicationCode: "project_management", Environment: "unknown",
		}); err == nil {
		t.Fatal("未支持的本机环境不能读取回执")
	}
}
