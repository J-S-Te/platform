package infrastructure

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/gorm"
)

// supersededRecoveryMySQLAgent 模拟 Agent 对原代次的否定证明。
type supersededRecoveryMySQLAgent struct {
	deploymentProvisionerStub
	current    string
	success    bool
	terminal   bool
	fenceCalls int
}

// ReadAccessDeploymentSupersession 提供只读否定证明替身。
// 参数：request限定测试原代次；此替身不操作真实服务器。
// 返回值：当前清单摘要与预置的肯定事实标记。
func (a *supersededRecoveryMySQLAgent) ReadAccessDeploymentSupersession(_ context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error) {
	return application.SubsystemAccessRecoveryEvidence{Receipt: request.Receipt,
		CurrentProfileChecksum: a.current, SuccessReceipt: a.success, TerminalOutcome: a.terminal}, nil
}

// CheckAccessDeploymentRecovery 断言 superseded 模式不会触达 fenced 预检。
// 参数：其余参数沿用既有恢复端口契约。
// 返回值：固定错误，用于暴露错误的路径选择。
func (a *supersededRecoveryMySQLAgent) CheckAccessDeploymentRecovery(context.Context,
	application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error) {
	return application.SubsystemAccessRecoveryEvidence{},
		errors.New("superseded recovery must not run the fenced preview")
}

// AbandonAccessDeployment 断言 superseded 模式不会写入 Agent 栅栏。
// 参数：其余参数沿用既有终止端口契约。
// 返回值：固定错误；任何调用都说明平台错误地成了第二个终止写者。
func (a *supersededRecoveryMySQLAgent) AbandonAccessDeployment(context.Context,
	application.SubsystemAccessAbandonRequest) (application.SubsystemAccessDeploymentOutcome, error) {
	a.fenceCalls++
	return application.SubsystemAccessDeploymentOutcome{},
		errors.New("superseded recovery must only read the negative proof")
}

// applyAccessRecoveryAuditMigration 在隔离测试库中执行000141迁移的全部语句。
// 参数：t为需显式DSN的测试上下文，db为隔离库连接。
// 返回值：无；迁移不可执行时直接失败，不静默跳过审计表。
func applyAccessRecoveryAuditMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec("DROP TABLE IF EXISTS subsystem_access_recovery_audit").Error; err != nil {
		t.Fatal(err)
	}
	body, err := migrations.Files.ReadFile("000141_subsystem_access_recovery_audit.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := migration.SplitStatements(string(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("apply 000141 statement: %v\n%s", err, statement)
		}
	}
}

// supersededRecoveryDatabase 建立"结果未知且审核基准已变更"的隔离场景。
// 参数：t为需显式DSN的测试上下文。
// 返回值：仓储、代次读取器、数据库、已转 WAITING 的原任务与审计表。
func supersededRecoveryDatabase(t *testing.T) (*SubsystemAccessOperationGORMRepository,
	*SubsystemAccessDeploymentStateGORMReader, *gorm.DB, application.SubsystemAccessOperation) {
	t.Helper()
	repo, reader, db, op := waitingRecoveryDatabase(t)
	applyAccessRecoveryAuditMigration(t, db)
	return repo, reader, db, op
}

// supersededRecoveryInput 构造操作者携带的收口输入。
// 参数：op为原任务，收口目标为第五代派发。
// 返回值：完整的 superseded 模式输入。
func supersededRecoveryInput(op application.SubsystemAccessOperation) application.SubsystemAccessManualRecoveryInput {
	return application.SubsystemAccessManualRecoveryInput{TenantID: op.TenantID, OperationID: op.ID,
		OperatorID: "operator-1", ExpectedVersion: op.Version, Generation: 5,
		Mode:                    application.SubsystemAccessRecoveryModeSuperseded,
		ReceiptManifestChecksum: "manifest-1", CurrentProfileChecksum: "manifest-2",
		Reason: "已核对原代次无成功回执且无终止结果，确认由新发布取代"}
}

// TestAccessSupersededRecoveryClosesUnknownGenerationOnMySQL 验证取代收口真正解开死锁。
// 参数：t为本轮专用可销毁 MySQL 库的测试上下文。
// 返回值：无；代次必须保持不变，且收口后原目标可以重新领取新代次。
func TestAccessSupersededRecoveryClosesUnknownGenerationOnMySQL(t *testing.T) {
	repo, reader, db, op := supersededRecoveryDatabase(t)
	ctx := context.Background()
	agent := &supersededRecoveryMySQLAgent{current: "manifest-2"}
	adapter, err := NewSubsystemAccessManualRecoveryAdapter(db, agent)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := adapter.PreviewSubsystemAccessRecovery(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.CurrentProfileChecksum != "manifest-2" || evidence.SuccessReceipt || evidence.TerminalOutcome ||
		len(evidence.RuntimeFiles) != 0 || len(evidence.Containers) != 0 ||
		evidence.Receipt.ManifestChecksum != "manifest-1" || evidence.Receipt.Generation != 5 {
		t.Fatalf("superseded preview = %+v", evidence)
	}
	input := supersededRecoveryInput(op)
	if err = adapter.AbandonSubsystemAccess(ctx, op, input); err != nil {
		t.Fatal(err)
	}
	if agent.fenceCalls != 0 {
		t.Fatal("superseded recovery wrote the agent fence")
	}

	var row subsystemAccessOperationModel
	if err = db.Where("id = ?", op.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "FAILED" || accessDerefString(row.ErrorCode) != application.AccessDeploymentSupersededUnknown ||
		row.ActiveSlot != nil || row.LeaseOwner != nil || row.LeaseExpiresAt != nil {
		t.Fatalf("operation not closed: %+v", row)
	}
	if row.Version != op.Version+1 {
		t.Fatalf("operation version = %d, want %d", row.Version, op.Version+1)
	}
	var stage subsystemAccessOperationStageModel
	if err = db.Where("tenant_id = ? AND operation_id = ? AND stage = ?", op.TenantID, op.ID, "DEPLOY").Take(&stage).Error; err != nil {
		t.Fatal(err)
	}
	if stage.Status != "FAILED" || accessDerefString(stage.ErrorCode) != application.AccessDeploymentSupersededUnknown ||
		stage.FinishedAt == nil || !strings.HasPrefix(accessDerefString(stage.EvidenceRef), "superseded:") {
		t.Fatalf("deploy stage not closed: %+v", stage)
	}
	var attempt subsystemAccessDeploymentAttempt
	if err = db.Where("operation_id = ?", op.ID).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Phase != "FAILED" || attempt.Generation != 5 {
		t.Fatalf("attempt = %+v", attempt)
	}
	var state subsystemDeploymentStateModel
	if err = db.Where("tenant_id = ?", op.TenantID).Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Status != string(application.SubsystemDeploymentStatusFailed) || state.Generation != 5 ||
		accessDerefString(state.AppliedManifestChecksum) != "previous-applied" {
		t.Fatalf("lifecycle falsely completed or re-claimed: %+v", state)
	}
	var audit subsystemAccessRecoveryAuditModel
	if err = db.Where("operation_id = ?", op.ID).Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.TenantID != op.TenantID || audit.ApplicationCode != op.ApplicationCode ||
		audit.EnvironmentCode != op.EnvironmentCode || audit.Generation != 5 ||
		audit.ReceiptManifestChecksum != "manifest-1" || audit.CurrentProfileChecksum != "manifest-2" ||
		audit.StateStatus != string(application.SubsystemDeploymentStatusUpdating) || audit.StateGeneration != 5 ||
		audit.StateAppliedManifest != "previous-applied" || audit.OperatorID != "operator-1" ||
		audit.Reason == "" || audit.CreatedAt.IsZero() {
		t.Fatalf("audit row = %+v", audit)
	}

	// 死锁真正解开：原任务可以显式重排、重新领取，并从 PREPARED 派发新一代次。
	if ok, err := repo.RequeueOperation(ctx, op.TenantID, op.ID, time.Now()); err != nil || !ok {
		t.Fatalf("explicit retry blocked after superseded close: %v %v", ok, err)
	}
	retried, ok, err := repo.ClaimOperation(ctx, op.TenantID, op.ID, "retry-worker", time.Minute, time.Now())
	if err != nil || !ok {
		t.Fatalf("retry claim: %v %v", ok, err)
	}
	phase, err := reader.PrepareAccessDeployment(ctx, retried)
	if err != nil || phase != "PREPARED" {
		t.Fatalf("superseded close did not free a new controlled generation: %s %v", phase, err)
	}
	if err = db.Where("tenant_id = ?", op.TenantID).Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Generation != 6 || state.Status != string(application.SubsystemDeploymentStatusUpdating) {
		t.Fatalf("new controlled generation = %+v", state)
	}
}

// TestAccessSupersededRecoveryRejectsIncompleteProofsOnMySQL 验证四类否定/一致性失败。
// 参数：t为本轮专用可销毁 MySQL 库的测试上下文。
// 返回值：无；任何拒绝都不得改变任务、代次、状态或写入审计。
func TestAccessSupersededRecoveryRejectsIncompleteProofsOnMySQL(t *testing.T) {
	cases := map[string]func(*supersededRecoveryMySQLAgent, *application.SubsystemAccessManualRecoveryInput){
		"active_lease": func(agent *supersededRecoveryMySQLAgent, input *application.SubsystemAccessManualRecoveryInput) {
			_ = agent
			_ = input
		},
		"checksum_equal": func(agent *supersededRecoveryMySQLAgent, input *application.SubsystemAccessManualRecoveryInput) {
			agent.current, input.CurrentProfileChecksum = "manifest-1", "manifest-1"
		},
		"success_receipt": func(agent *supersededRecoveryMySQLAgent, _ *application.SubsystemAccessManualRecoveryInput) {
			agent.success = true
		},
		"terminal_outcome": func(agent *supersededRecoveryMySQLAgent, _ *application.SubsystemAccessManualRecoveryInput) {
			agent.terminal = true
		},
		"stale_proof": func(agent *supersededRecoveryMySQLAgent, input *application.SubsystemAccessManualRecoveryInput) {
			input.CurrentProfileChecksum = "manifest-3"
			_ = agent
		},
		"version_drift": func(_ *supersededRecoveryMySQLAgent, input *application.SubsystemAccessManualRecoveryInput) {
			input.ExpectedVersion++
		},
		"generation_drift": func(_ *supersededRecoveryMySQLAgent, input *application.SubsystemAccessManualRecoveryInput) {
			input.Generation++
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			repo, _, db, op := supersededRecoveryDatabase(t)
			ctx := context.Background()
			if name == "active_lease" {
				// 活租约只能从仓储侧出现：模拟 worker 仍持有第 N 轮领取。
				if err := db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).
					Update("lease_owner", "worker-1").Error; err != nil {
					t.Fatal(err)
				}
				if current, err := repo.GetOperation(ctx, op.TenantID, op.ID); err != nil {
					t.Fatal(err)
				} else {
					op = current
				}
			}
			agent := &supersededRecoveryMySQLAgent{current: "manifest-2"}
			adapter, err := NewSubsystemAccessManualRecoveryAdapter(db, agent)
			if err != nil {
				t.Fatal(err)
			}
			input := supersededRecoveryInput(op)
			mutate(agent, &input)
			if err = adapter.AbandonSubsystemAccess(ctx, op, input); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
				t.Fatalf("incomplete proof accepted: %v", err)
			}
			var row subsystemAccessOperationModel
			if err = db.Where("id = ?", op.ID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.Status != "WAITING" || row.Version != op.Version || row.ActiveSlot == nil ||
				accessDerefString(row.ErrorCode) != application.AccessDeploymentEffectUnknown {
				t.Fatalf("rejected recovery changed the operation: %+v", row)
			}
			var attempt subsystemAccessDeploymentAttempt
			if err = db.Where("operation_id = ?", op.ID).Take(&attempt).Error; err != nil {
				t.Fatal(err)
			}
			var state subsystemDeploymentStateModel
			if err = db.Where("tenant_id = ?", op.TenantID).Take(&state).Error; err != nil {
				t.Fatal(err)
			}
			if attempt.Phase != "DISPATCHED" || state.Status != string(application.SubsystemDeploymentStatusUpdating) ||
				state.Generation != 5 {
				t.Fatalf("rejected recovery changed the lifecycle: %+v %+v", attempt, state)
			}
			var audits int64
			if err = db.Model(&subsystemAccessRecoveryAuditModel{}).Count(&audits).Error; err != nil {
				t.Fatal(err)
			}
			if audits != 0 {
				t.Fatalf("rejected recovery wrote %d audit rows", audits)
			}
		})
	}
}

// TestAccessSupersededRecoveryAuditIsAppendOnlyOnMySQL 验证审计表只允许插入。
// 参数：t为本轮专用可销毁 MySQL 库的测试上下文。
// 返回值：无，直接 UPDATE/DELETE 必须被数据库拒绝且原行保留。
func TestAccessSupersededRecoveryAuditIsAppendOnlyOnMySQL(t *testing.T) {
	_, _, db, op := supersededRecoveryDatabase(t)
	ctx := context.Background()
	agent := &supersededRecoveryMySQLAgent{current: "manifest-2"}
	adapter, err := NewSubsystemAccessManualRecoveryAdapter(db, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.AbandonSubsystemAccess(ctx, op, supersededRecoveryInput(op)); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("UPDATE subsystem_access_recovery_audit SET reason = 'rewritten'").Error; err == nil {
		t.Fatal("audit row was mutable")
	}
	if err = db.Exec("DELETE FROM subsystem_access_recovery_audit").Error; err == nil {
		t.Fatal("audit row was deletable")
	}
	var count int64
	if err = db.Model(&subsystemAccessRecoveryAuditModel{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("append-only audit rows = %d, %v", count, err)
	}
	var audit subsystemAccessRecoveryAuditModel
	if err = db.Where("operation_id = ?", op.ID).Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Reason != supersededRecoveryInput(op).Reason {
		t.Fatalf("audit reason changed: %q", audit.Reason)
	}
}

// TestAccessManualRecoveryLegacyAgentPreviewFillsChecksumOnMySQL 验证旧 Agent 的预检仍可用。
// 参数：t为本轮专用可销毁 MySQL 库的测试上下文。
// 返回值：无，fenced 预检成功后必须补齐当前清单摘要，供公共响应使用。
func TestAccessManualRecoveryLegacyAgentPreviewFillsChecksumOnMySQL(t *testing.T) {
	_, _, db, op := supersededRecoveryDatabase(t)
	// 只实现 fenced 恢复端口的 Agent 代表现场旧版本二进制。
	adapter, err := NewSubsystemAccessManualRecoveryAdapter(db, &manualRecoveryMySQLAgent{})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := adapter.PreviewSubsystemAccessRecovery(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.CurrentProfileChecksum != evidence.Receipt.ManifestChecksum ||
		len(evidence.RuntimeFiles) == 0 || len(evidence.Containers) == 0 || evidence.EvidenceDigest == "" {
		t.Fatalf("legacy fenced preview = %+v", evidence)
	}
}
