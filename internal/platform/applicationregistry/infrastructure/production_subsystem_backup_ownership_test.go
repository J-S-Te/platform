package infrastructure

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHandoffBackupOwnershipNonRootIsNoop 锁定非 root 环境的行为：
// 没有 CHOWN 能力时必须静默跳过，不能把无法改属主当成失败或伪成功。
func TestHandoffBackupOwnershipNonRootIsNoop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root test user")
	}
	root := t.TempDir()
	backupRoot := filepath.Join(root, "backups")
	batch := filepath.Join(backupRoot, "demo-onboarding-20261107T000000.000000000Z.sql.backup")
	if err := os.MkdirAll(batch, 0o750); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(batch, "demo.sql")
	if err := os.WriteFile(payload, []byte("dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root}}
	if err := target.handoffBackupOwnership(backupRoot, backupRoot, batch); err != nil {
		t.Fatalf("non-root handoff must be a no-op, got %v", err)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatalf("payload must remain readable: %v", err)
	}
}

// TestHandoffBackupOwnershipIgnoresMissingPath 保证幂等：调用方可能在批次尚未生成时
// 先交还根目录，缺失路径不能报错。
func TestHandoffBackupOwnershipIgnoresMissingPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root test user")
	}
	root := t.TempDir()
	target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root}}
	if err := target.handoffBackupOwnership(root, filepath.Join(root, "backups", "missing.backup")); err != nil {
		t.Fatalf("missing path must be ignored, got %v", err)
	}
}
