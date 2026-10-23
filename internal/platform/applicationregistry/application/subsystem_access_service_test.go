package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type accessFactsStub struct {
	facts   SubsystemAccessFacts
	version string
	err     error
}

func (stub *accessFactsStub) ReadSubsystemAccessFacts(context.Context, string, string, string) (SubsystemAccessFacts, SubsystemAccessPreviewVersion, error) {
	if stub.err != nil {
		return SubsystemAccessFacts{}, SubsystemAccessPreviewVersion{}, stub.err
	}
	return stub.facts, SubsystemAccessPreviewVersion{Version: stub.version}, nil
}

type accessRepositoryStub struct {
	operations map[string]SubsystemAccessOperation
	stages     map[string][]SubsystemAccessOperationStage
	requeued   int
	requeueOK  bool
}

func newAccessRepositoryStub() *accessRepositoryStub {
	return &accessRepositoryStub{operations: map[string]SubsystemAccessOperation{}, stages: map[string][]SubsystemAccessOperationStage{}, requeueOK: true}
}

func (stub *accessRepositoryStub) CreateOperation(_ context.Context, create SubsystemAccessOperationCreate) (SubsystemAccessOperation, bool, error) {
	for _, existing := range stub.operations {
		if existing.IdempotencyKey == create.IdempotencyKey {
			if existing.RequestDigest != create.RequestDigest {
				return SubsystemAccessOperation{}, false, ErrSubsystemAccessOperationConflict
			}
			return existing, false, nil
		}
		if existing.HoldsActiveSlotFor(create.TenantID, create.ApplicationCode, create.EnvironmentCode) {
			return SubsystemAccessOperation{}, false, ErrSubsystemAccessOperationConflict
		}
	}
	operation := SubsystemAccessOperation{ID: create.ID, TenantID: create.TenantID, ApplicationID: create.ApplicationID,
		EnvironmentID: create.EnvironmentID, ApplicationCode: create.ApplicationCode, EnvironmentCode: create.EnvironmentCode,
		Action: create.Action, Stage: create.Stage, Status: SubsystemAccessStatusPending, PreviewVersion: create.PreviewVersion,
		RequestDigest: create.RequestDigest, IdempotencyKey: create.IdempotencyKey, OperatorID: create.OperatorID,
		Version: 1, CreatedAt: create.Now, UpdatedAt: create.Now}
	stub.operations[operation.ID] = operation
	return operation, true, nil
}

func (stub *accessRepositoryStub) GetOperation(_ context.Context, tenantID, id string) (SubsystemAccessOperation, error) {
	operation, ok := stub.operations[id]
	if !ok || operation.TenantID != tenantID {
		return SubsystemAccessOperation{}, ErrSubsystemAccessOperationNotFound
	}
	return operation, nil
}

func (stub *accessRepositoryStub) FindOperationByTarget(_ context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessOperation, bool, error) {
	for _, operation := range stub.operations {
		if operation.TenantID == tenantID && operation.ApplicationCode == applicationCode && operation.EnvironmentCode == environmentCode {
			return operation, true, nil
		}
	}
	return SubsystemAccessOperation{}, false, nil
}

func (stub *accessRepositoryStub) ListOperationsByTarget(context.Context, string, string, string, int) ([]SubsystemAccessOperation, error) {
	return nil, nil
}

func (stub *accessRepositoryStub) ClaimOperation(context.Context, string, string, string, time.Duration, time.Time) (SubsystemAccessOperation, bool, error) {
	return SubsystemAccessOperation{}, false, nil
}

func (stub *accessRepositoryStub) RenewOperationLease(context.Context, string, string, string, time.Duration, time.Time) (bool, error) {
	return false, nil
}

func (stub *accessRepositoryStub) CompleteOperation(context.Context, SubsystemAccessOperationCompletion) (bool, error) {
	return false, nil
}

func (stub *accessRepositoryStub) AdvanceOperationStage(context.Context, string, string, string, SubsystemAccessStage, time.Time) (bool, error) {
	return true, nil
}

func (stub *accessRepositoryStub) ListClaimableOperations(context.Context, int, time.Time) ([]SubsystemAccessOperation, error) {
	return nil, nil
}

func (stub *accessRepositoryStub) RequeueOperation(_ context.Context, tenantID, id string, _ time.Time) (bool, error) {
	operation, ok := stub.operations[id]
	if !ok || operation.TenantID != tenantID || !stub.requeueOK {
		return false, nil
	}
	stub.requeued++
	operation.Status = SubsystemAccessStatusPending
	stub.operations[id] = operation
	return true, nil
}

func (stub *accessRepositoryStub) UpsertOperationStage(context.Context, string, string, SubsystemAccessStage, SubsystemAccessStatus, string, string, string, time.Time) error {
	return nil
}

func (stub *accessRepositoryStub) ListOperationStages(_ context.Context, tenantID, operationID string) ([]SubsystemAccessOperationStage, error) {
	return stub.stages[tenantID+":"+operationID], nil
}

// HoldsActiveSlotFor 供桩件判断槽位冲突（真实实现由唯一索引保证）。
func (operation SubsystemAccessOperation) HoldsActiveSlotFor(tenantID, applicationCode, environmentCode string) bool {
	return operation.TenantID == tenantID && operation.ApplicationCode == applicationCode &&
		operation.EnvironmentCode == environmentCode && operation.Status.HoldsActiveSlot()
}

// accessIDsStub / accessClockStub 是最小依赖实现（接口而非函数类型）。
type accessIDsStub struct{}

func (accessIDsStub) New(time.Time) (string, error) { return "01SAOP000000000000000000001", nil }

type accessClockStub struct{}

func (accessClockStub) Now() time.Time { return time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC) }

func accessServiceFor(t *testing.T, facts *accessFactsStub, repository *accessRepositoryStub) *SubsystemAccessService {
	t.Helper()
	service, err := NewSubsystemAccessService(repository, facts, accessIDsStub{}, accessClockStub{})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// 预览：动作来自服务端判定；缺项与所需权限逐项返回，权限不足时明确列出。
func TestSubsystemAccessPreviewReportsActionMissingAndPermissions(t *testing.T) {
	facts := &accessFactsStub{facts: SubsystemAccessFacts{InScope: true, Registered: true, ApprovalReady: false, LicenseReady: true, ImagesReady: true,
		ResumeStage: SubsystemAccessStageAuth, ChangeSummary: []string{"公开地址变更"}}, version: "pv-7"}
	service := accessServiceFor(t, facts, newAccessRepositoryStub())

	preview, err := service.Preview(context.Background(), "tenant-1", "settlement", "dev", []string{PermissionApplicationUpdate})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Decision.Action != SubsystemAccessActionAdopt {
		t.Fatalf("action = %s, want ADOPT", preview.Decision.Action)
	}
	if preview.NextStage != SubsystemAccessStageAuth || preview.PreviewVersion != "pv-7" {
		t.Fatalf("preview = %+v", preview)
	}
	codes := make([]string, 0, len(preview.Missing))
	for _, item := range preview.Missing {
		codes = append(codes, item.Code)
	}
	if strings.Join(codes, ",") != "RUNTIME_APPROVAL_MISSING" {
		t.Fatalf("missing = %v", codes)
	}
	if len(preview.RequiredPerms) != 4 || len(preview.MissingPerms) != 3 {
		t.Fatalf("perms required=%v missing=%v", preview.RequiredPerms, preview.MissingPerms)
	}
	if len(preview.ChangeSummary) != 1 || preview.ChangeSummary[0] != "公开地址变更" {
		t.Fatalf("change summary = %v", preview.ChangeSummary)
	}
	// 未提供主体权限时不做"已具备"判断。
	withoutPermissions, err := service.Preview(context.Background(), "tenant-1", "settlement", "dev", nil)
	if err != nil || len(withoutPermissions.MissingPerms) != 0 {
		t.Fatalf("missing perms without operator context = %v (%v)", withoutPermissions.MissingPerms, err)
	}
}

// 提交：预览版本变化必须冲突；同键同内容返回原操作；已一致时不创建。
func TestSubsystemAccessSubmitGuardsPreviewAndIdempotency(t *testing.T) {
	facts := &accessFactsStub{facts: SubsystemAccessFacts{InScope: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true}, version: "pv-1"}
	repository := newAccessRepositoryStub()
	service := accessServiceFor(t, facts, repository)
	request := SubsystemAccessSubmitRequest{TenantID: "tenant-1", ApplicationCode: "contract", EnvironmentCode: "Dev",
		PreviewVersion: "pv-1", IdempotencyKey: "idem-1", OperatorID: "operator-1"}

	result, err := service.Submit(context.Background(), request)
	if err != nil || !result.Created || result.Operation.Action != SubsystemAccessActionOnboard {
		t.Fatalf("submit = %+v err = %v", result, err)
	}
	replay, err := service.Submit(context.Background(), request)
	if err != nil || replay.Created || replay.Operation.ID != result.Operation.ID {
		t.Fatalf("idempotent replay = %+v err = %v", replay, err)
	}
	// 同键不同内容（公开地址变化）→ 冲突。
	drifted := request
	drifted.PublicOrigin = "https://changed.example.com"
	if _, err := service.Submit(context.Background(), drifted); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
		t.Fatalf("same key with different content must conflict, got %v", err)
	}
	// 预览版本变化 → 冲突（要求重新预览）。
	facts.version = "pv-2"
	request.IdempotencyKey = "idem-2"
	if _, err := service.Submit(context.Background(), request); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
		t.Fatalf("stale preview version must conflict, got %v", err)
	}
	// 已接入且一致 → 只展示，不创建操作。
	facts.version = "pv-3"
	facts.facts = SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true}
	request.PreviewVersion = "pv-3"
	result, err = service.Submit(context.Background(), request)
	if err != nil || !result.DisplayOnly || result.Operation.ID != "" {
		t.Fatalf("consistent target must be display-only: %+v err = %v", result, err)
	}
}

// 恢复：条件未清仍等待（不重新排队）；条件已清则放回可领取状态。
func TestSubsystemAccessResumeRechecksFacts(t *testing.T) {
	facts := &accessFactsStub{facts: SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, ApprovalReady: false, LicenseReady: true, ImagesReady: true}, version: "pv-1"}
	repository := newAccessRepositoryStub()
	service := accessServiceFor(t, facts, repository)
	repository.operations["01SAOP000000000000000000001"] = SubsystemAccessOperation{
		ID: "01SAOP000000000000000000001", TenantID: "tenant-1", ApplicationCode: "contract", EnvironmentCode: "dev",
		Action: SubsystemAccessActionUpdate, Stage: SubsystemAccessStageAuth, Status: SubsystemAccessStatusWaiting, Version: 2}

	view, err := service.Resume(context.Background(), "tenant-1", "01SAOP000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if repository.requeued != 0 || view.Operation.Status != SubsystemAccessStatusWaiting || len(view.Missing) == 0 {
		t.Fatalf("blocked resume must stay waiting: requeued=%d view=%+v", repository.requeued, view)
	}

	facts.facts.ApprovalReady = true
	view, err = service.Resume(context.Background(), "tenant-1", "01SAOP000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if repository.requeued != 1 || view.Operation.Status != SubsystemAccessStatusPending {
		t.Fatalf("cleared resume must requeue: requeued=%d status=%s", repository.requeued, view.Operation.Status)
	}

	// 运行中的操作不能"恢复"。
	repository.operations["01SAOP000000000000000000001"] = SubsystemAccessOperation{
		ID: "01SAOP000000000000000000001", TenantID: "tenant-1", Status: SubsystemAccessStatusRunning}
	if _, err := service.Resume(context.Background(), "tenant-1", "01SAOP000000000000000000001"); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
		t.Fatalf("running operation must not resume, got %v", err)
	}
}

// 权限映射与既有接入端点保持一致，统一入口不降低要求。
func TestSubsystemAccessRequiredPermissionsMatchLegacyRoutes(t *testing.T) {
	onboard := SubsystemAccessRequiredPermissions(SubsystemAccessActionOnboard)
	if len(onboard) != 5 {
		t.Fatalf("onboard permissions = %v", onboard)
	}
	for _, permission := range []string{PermissionApplicationCreate, PermissionApplicationEnvironmentCreate, PermissionLoginTargetCreate, PermissionOAuthClientCreate, PermissionRoleBindingUpdate} {
		if !containsStringValue(onboard, permission) {
			t.Fatalf("onboard missing %s", permission)
		}
	}
	for _, action := range []SubsystemAccessAction{SubsystemAccessActionAdopt, SubsystemAccessActionUpdate, SubsystemAccessActionRetry} {
		required := SubsystemAccessRequiredPermissions(action)
		if len(required) != 4 || !containsStringValue(required, PermissionOAuthClientDisable) {
			t.Fatalf("%s permissions = %v", action, required)
		}
	}
	if readOnly := SubsystemAccessRequiredPermissions(SubsystemAccessActionNone); len(readOnly) != 1 || readOnly[0] != PermissionApplicationRead {
		t.Fatalf("display-only permissions = %v", readOnly)
	}
}

func containsStringValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// 执行路径：首次接入走统一入口；接管/更新/恢复由既有受控入口执行并给出可执行提示；已一致为无需操作。
func TestSubsystemAccessPreviewReportsExecutionPath(t *testing.T) {
	cases := []struct {
		name     string
		facts    SubsystemAccessFacts
		wantPath string
		wantHint bool
	}{
		{"首次接入 → 统一入口", SubsystemAccessFacts{InScope: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true},
			SubsystemAccessExecutionUnified, false},
		{"已登记未受控 → 统一入口（接管）", SubsystemAccessFacts{InScope: true, Registered: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true},
			SubsystemAccessExecutionUnified, false},
		{"已接入且漂移 → 统一入口（更新）", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, ConfigurationDrift: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true},
			SubsystemAccessExecutionUnified, false},
		{"上次失败 → 统一入口（重试）", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, LastOperationFailed: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true},
			SubsystemAccessExecutionUnified, false},
		{"已接入且一致 → 无需操作", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true},
			SubsystemAccessExecutionNone, false},
	}
	for _, item := range cases {
		service := accessServiceFor(t, &accessFactsStub{facts: item.facts, version: "pv-1"}, newAccessRepositoryStub())
		preview, err := service.Preview(context.Background(), "tenant-1", "settlement", "dev", nil)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		if preview.ExecutionPath != item.wantPath {
			t.Fatalf("%s: execution path = %s, want %s", item.name, preview.ExecutionPath, item.wantPath)
		}
		if item.wantHint && !strings.Contains(preview.LegacyHint, "subsystem.sh update") {
			t.Fatalf("%s: legacy hint = %q", item.name, preview.LegacyHint)
		}
		if !item.wantHint && preview.LegacyHint != "" {
			t.Fatalf("%s: unexpected hint %q", item.name, preview.LegacyHint)
		}
	}
}
