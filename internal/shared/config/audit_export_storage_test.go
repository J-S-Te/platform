package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// D6 独立导出目录：导出产物必须有自己的根目录，默认落在 FILE_STORAGE_ROOT 下的
// audit-exports 子目录，因此默认部署无需新增 compose 卷，且与平台上传文件逻辑隔离。
func TestLoadDefaultsAuditExportStorageRootUnderFileStorageRoot(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("FILE_STORAGE_ROOT", "/var/lib/basic-platform/uploads")
	t.Setenv("AUDIT_EXPORT_STORAGE_ROOT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := filepath.Join("/var/lib/basic-platform/uploads", "audit-exports")
	if cfg.AuditExportStorageRoot != want {
		t.Fatalf("AuditExportStorageRoot = %q, want %q", cfg.AuditExportStorageRoot, want)
	}
	if cfg.AuditExportStorageRoot == cfg.FileStorageRoot {
		t.Fatal("audit export root must be a dedicated directory, not FILE_STORAGE_ROOT itself")
	}
}

// AUDIT_EXPORT_STORAGE_ROOT 显式覆盖默认子目录；覆盖值就是装配与下载使用的唯一根。
func TestLoadOverridesAuditExportStorageRootFromEnv(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("FILE_STORAGE_ROOT", "/var/lib/basic-platform/uploads")
	t.Setenv("AUDIT_EXPORT_STORAGE_ROOT", "/var/lib/basic-platform/audit-exports")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.AuditExportStorageRoot, "/var/lib/basic-platform/audit-exports"; got != want {
		t.Fatalf("AuditExportStorageRoot = %q, want %q", got, want)
	}
}

// 导出根等于上传根会让保留期清理与导出配额直接波及业务上传文件，必须在启动前拒绝。
// 路径比较按清理后的形式进行：尾随分隔符不能绕过校验。
func TestLoadRejectsAuditExportStorageRootEqualToFileStorageRoot(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("FILE_STORAGE_ROOT", "/var/lib/basic-platform/uploads")
	t.Setenv("AUDIT_EXPORT_STORAGE_ROOT", "/var/lib/basic-platform/uploads/")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want AUDIT_EXPORT_STORAGE_ROOT == FILE_STORAGE_ROOT rejected")
	} else if !strings.Contains(err.Error(), "AUDIT_EXPORT_STORAGE_ROOT") {
		t.Fatalf("Load() error = %v, want mention AUDIT_EXPORT_STORAGE_ROOT", err)
	}
}

// 配置值里的 ".." 必须被清理成规范路径，不能让导出目录漂移到不可预期的位置。
func TestLoadCleansAuditExportStorageRootRelativeSegments(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("FILE_STORAGE_ROOT", "/var/lib/basic-platform/uploads")
	t.Setenv("AUDIT_EXPORT_STORAGE_ROOT", "/var/lib/basic-platform/exports/../audit-exports")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.AuditExportStorageRoot, "/var/lib/basic-platform/audit-exports"; got != want {
		t.Fatalf("AuditExportStorageRoot = %q, want cleaned %q", got, want)
	}
}

// 清理之后仍残留 ".." 说明配置试图逃出环境文件所在目录：拒绝启动而不是猜测运维意图。
func TestLoadRejectsAuditExportStorageRootEscapingParent(t *testing.T) {
	t.Setenv("ENV_FILE", "missing.env")
	t.Setenv("FILE_STORAGE_ROOT", "data/uploads")
	t.Setenv("AUDIT_EXPORT_STORAGE_ROOT", "../../etc/audit-exports")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want escaping AUDIT_EXPORT_STORAGE_ROOT rejected")
	} else if !strings.Contains(err.Error(), "AUDIT_EXPORT_STORAGE_ROOT") {
		t.Fatalf("Load() error = %v, want mention AUDIT_EXPORT_STORAGE_ROOT", err)
	}
}

// 直接构造 Config（不经 Load）也必须被 Validate 拦住共享根配置。
func TestValidateRejectsAuditExportStorageRootEqualToFileStorageRoot(t *testing.T) {
	cfg := productionInsecureCookieConfig("development")
	cfg.AuditExportStorageRoot = cfg.FileStorageRoot

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want shared audit export root rejected")
	}
	if !strings.Contains(err.Error(), "AUDIT_EXPORT_STORAGE_ROOT") {
		t.Fatalf("Validate() error = %v, want mention AUDIT_EXPORT_STORAGE_ROOT", err)
	}
}
