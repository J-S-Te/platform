package infrastructure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 失败部署丢弃必须"先占用再销毁"：占用是与 status+generation 绑定的 CAS，只有抢占成功的
// 调用方才允许执行 Agent Teardown；删除必须匹配占用后的代号。否则并发的「重试」会把记录
// 推进为 UPDATING，删除虽被拒绝，但 Teardown 已经拆掉了重试刚部署的环境。

func newDiscardDryRunRepository(t *testing.T, captured *[]string) *SubsystemOnboardingGORMRepository {
	t.Helper()
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	record := func(tx *gorm.DB) {
		if sql := tx.Statement.SQL.String(); sql != "" {
			*captured = append(*captured, sql)
		}
	}
	if err := database.Callback().Update().After("gorm:update").Register("capture_discard_update", record); err != nil {
		t.Fatalf("register update callback: %v", err)
	}
	if err := database.Callback().Delete().After("gorm:delete").Register("capture_discard_delete", record); err != nil {
		t.Fatalf("register delete callback: %v", err)
	}
	return &SubsystemOnboardingGORMRepository{database: database}
}

func TestClaimFailedSubsystemDeploymentForDiscardUsesStatusAndGenerationCAS(t *testing.T) {
	var captured []string
	repository := newDiscardDryRunRepository(t, &captured)
	now := time.Date(2026, time.October, 17, 4, 0, 0, 0, time.UTC)

	claimed, err := repository.ClaimFailedSubsystemDeploymentForDiscard(context.Background(), "tenant-1", "contract", "prod", 7, now)
	if err != nil {
		t.Fatalf("claim failed deployment: %v", err)
	}
	// DryRun 不真正写入，RowsAffected 为 0：这里断言的是语句形状而不是抢占结果。
	if claimed {
		t.Fatal("DryRun 下不应报告抢占成功")
	}
	if len(captured) != 1 {
		t.Fatalf("captured statements = %d, want 1: %v", len(captured), captured)
	}
	sql := captured[0]
	for _, fragment := range []string{"UPDATE `subsystem_deployment_state`", "status = ?", "generation = ?"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("占用语句缺少 %q：%s", fragment, sql)
		}
	}
	for _, fragment := range []string{"generation + 1", "started_at", "completed_at"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("占用语句缺少 %q（占用需刷新看护判定依据）：%s", fragment, sql)
		}
	}
}

func TestDiscardClaimedSubsystemDeploymentDeletesOnlyClaimedGeneration(t *testing.T) {
	var captured []string
	repository := newDiscardDryRunRepository(t, &captured)

	if err := repository.DiscardClaimedSubsystemDeployment(context.Background(), "tenant-1", "contract", "prod", 8); err == nil {
		t.Fatal("DryRun 未删除任何行，应返回 ErrValidation 让调用方感知冲突")
	} else if err != application.ErrValidation {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured statements = %d, want 1: %v", len(captured), captured)
	}
	sql := captured[0]
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "DELETE") {
		t.Fatalf("丢弃记录必须是 DELETE：%s", sql)
	}
	for _, fragment := range []string{"status = ?", "generation = ?"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("删除语句缺少 %q：%s", fragment, sql)
		}
	}
}

// 重试领取与看护恢复都必须认识新的占用态，否则会出现"丢弃中仍可重试"或"崩溃的丢弃无人收口"。
func TestDiscardingStatusParticipatesInRetryGuardAndSweeper(t *testing.T) {
	source := readDiscardRepositorySource(t, "subsystem_onboarding_gorm_repository.go")
	if !strings.Contains(source, "row.Status == application.SubsystemDeploymentStatusDiscarding") {
		t.Error("ClaimSubsystemDeployment 必须在丢弃占用期间拒绝重试")
	}
	if !strings.Contains(source, "application.SubsystemDeploymentStatusDiscarding,\n\t\t}).") {
		t.Error("ListInFlightSubsystemDeployments 必须包含丢弃占用态，崩溃后才能被看护收口")
	}
	if !strings.Contains(source, `errorCode, errorMessage = "DISCARD_INTERRUPTED"`) {
		t.Error("看护收口中断的丢弃时必须写入可区分的错误码")
	}
	if !application.SubsystemDeploymentTransitionAllowed(application.SubsystemDeploymentStatusFailed, application.SubsystemDeploymentStatusDiscarding) {
		t.Error("PROVISION_FAILED → DISCARDING 必须是合法转移")
	}
	if !application.SubsystemDeploymentTransitionAllowed(application.SubsystemDeploymentStatusDiscarding, application.SubsystemDeploymentStatusFailed) {
		t.Error("DISCARDING → PROVISION_FAILED 必须合法，供看护收口")
	}
	if application.SubsystemDeploymentTransitionAllowed(application.SubsystemDeploymentStatusReady, application.SubsystemDeploymentStatusDiscarding) {
		t.Error("READY 不能被直接丢弃")
	}
	if application.SubsystemDeploymentTransitionAllowed(application.SubsystemDeploymentStatusDiscarding, application.SubsystemDeploymentStatusUpdating) {
		t.Error("丢弃占用期间不能直接被重试推进")
	}
}

func readDiscardRepositorySource(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}
