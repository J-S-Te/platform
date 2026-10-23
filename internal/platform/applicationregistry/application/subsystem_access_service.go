package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Permission* 常量是统一入口复用的权限码，与既有接入端点保持同一组取值，
// 统一入口不降低权限要求；页面只展示，授权以服务端判定为准。
const (
	PermissionApplicationCreate            = "platform:application:create"
	PermissionApplicationUpdate            = "platform:application:update"
	PermissionApplicationRead              = "platform:application:read"
	PermissionApplicationEnvironmentCreate = "platform:application-environment:create"
	PermissionApplicationEnvironmentUpdate = "platform:application-environment:update"
	PermissionLoginTargetCreate            = "platform:application-login-target:create"
	PermissionLoginTargetUpdate            = "platform:application-login-target:update"
	PermissionOAuthClientCreate            = "platform:oauth-client:create"
	PermissionOAuthClientDisable           = "platform:oauth-client:disable"
	PermissionRoleBindingUpdate            = "platform:role-binding:update"
)

// SubsystemAccessRequiredPermissions 返回执行该动作所需的权限（服务端决定，页面只展示）。
func SubsystemAccessRequiredPermissions(action SubsystemAccessAction) []string {
	switch action {
	case SubsystemAccessActionOnboard:
		return []string{PermissionApplicationCreate, PermissionApplicationEnvironmentCreate,
			PermissionLoginTargetCreate, PermissionOAuthClientCreate, PermissionRoleBindingUpdate}
	case SubsystemAccessActionAdopt, SubsystemAccessActionUpdate, SubsystemAccessActionRetry:
		return []string{PermissionApplicationUpdate, PermissionApplicationEnvironmentUpdate,
			PermissionLoginTargetUpdate, PermissionOAuthClientDisable}
	default:
		return []string{PermissionApplicationRead}
	}
}

// SubsystemAccessMissing 是一项未满足条件（含下一步提示）。
type SubsystemAccessMissing struct {
	Code       string
	Message    string
	NextAction string
}

// SubsystemAccessFactsSource 读取判定所需事实与预览版本（清单摘要/批准事实/环境版本的变化会改变它）。
type SubsystemAccessFactsSource interface {
	ReadSubsystemAccessFacts(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessFacts, SubsystemAccessPreviewVersion, error)
}

// SubsystemAccessPreviewVersion 是预览依赖事实的快照标识；事实变化必须要求重新预览。
type SubsystemAccessPreviewVersion struct {
	Version string
}

// 执行路径常量：统一入口可执行，或需改用既有受控入口。
const (
	// SubsystemAccessExecutionUnified 表示本次动作由统一入口的持久执行器完成。
	SubsystemAccessExecutionUnified = "unified"
	// SubsystemAccessExecutionLegacy 表示本次动作暂由既有受控入口完成（更新路径尚未接线）。
	SubsystemAccessExecutionLegacy = "legacy"
	// SubsystemAccessExecutionNone 表示无需操作（已一致）。
	SubsystemAccessExecutionNone = "none"
)

// SubsystemAccessPreview 是只读检查结果。
type SubsystemAccessPreview struct {
	Decision       SubsystemAccessDecision
	NextStage      SubsystemAccessStage
	Missing        []SubsystemAccessMissing
	RequiredPerms  []string
	MissingPerms   []string
	ChangeSummary  []string
	PreviewVersion string
	// ExecutionPath 说明本次动作由哪个入口执行（统一入口 / 既有受控入口 / 无需操作）。
	ExecutionPath string
	// LegacyHint 在执行路径为 legacy 时给出可直接使用的既有入口命令提示。
	LegacyHint string
}

// SubsystemAccessSubmitRequest 是确认执行请求（只携带目标、公开地址、认证选择与预览版本）。
type SubsystemAccessSubmitRequest struct {
	TenantID        string
	ApplicationCode string
	EnvironmentCode string
	PublicOrigin    string
	AuthProvider    string
	InitialAdminID  string
	PreviewVersion  string
	IdempotencyKey  string
	OperatorID      string
}

// SubsystemAccessSubmitResult 是提交结果；DisplayOnly 为真表示无需创建操作。
type SubsystemAccessSubmitResult struct {
	Operation   SubsystemAccessOperation
	Created     bool
	DisplayOnly bool
	Decision    SubsystemAccessDecision
}

// SubsystemAccessOperationView 是查询结果：操作 + 阶段 + 分项结果（不合并成单一绿色状态）。
type SubsystemAccessOperationView struct {
	Operation SubsystemAccessOperation
	Stages    []SubsystemAccessOperationStage
	Decision  SubsystemAccessDecision
	Missing   []SubsystemAccessMissing
}

// SubsystemAccessService 统一接入向导的服务端流程编排。
type SubsystemAccessService struct {
	repository SubsystemAccessOperationRepository
	facts      SubsystemAccessFactsSource
	ids        IdentifierGenerator
	clock      Clock
}

// NewSubsystemAccessService 构造服务；依赖缺失即失败（不使用可选依赖静默降级）。
func NewSubsystemAccessService(repository SubsystemAccessOperationRepository, facts SubsystemAccessFactsSource, ids IdentifierGenerator, clock Clock) (*SubsystemAccessService, error) {
	if repository == nil || facts == nil || ids == nil || clock == nil {
		return nil, errors.New("subsystem access service dependencies must not be nil")
	}
	return &SubsystemAccessService{repository: repository, facts: facts, ids: ids, clock: clock}, nil
}

// Preview 只读检查：返回服务端判定的动作、起始阶段、缺项、所需权限与变更摘要。
// operatorPermissions 为空表示只返回所需权限（调用方未提供主体权限时不做"已具备"判断）。
func (service *SubsystemAccessService) Preview(ctx context.Context, tenantID, applicationCode, environmentCode string, operatorPermissions []string) (SubsystemAccessPreview, error) {
	facts, version, err := service.readFacts(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return SubsystemAccessPreview{}, err
	}
	decision := DecideSubsystemAccessAction(facts)
	preview := SubsystemAccessPreview{
		Decision:       decision,
		NextStage:      facts.ResumeStage,
		Missing:        describeSubsystemAccessMissing(facts),
		RequiredPerms:  SubsystemAccessRequiredPermissions(decision.Action),
		MissingPerms:   missingPermissions(SubsystemAccessRequiredPermissions(decision.Action), operatorPermissions),
		ChangeSummary:  append([]string(nil), facts.ChangeSummary...),
		PreviewVersion: version.Version,
	}
	preview.ExecutionPath, preview.LegacyHint = subsystemAccessExecutionPath(decision, applicationCode, environmentCode)
	if preview.NextStage == "" {
		preview.NextStage = SubsystemAccessStageCheck
	}
	if !ValidSubsystemAccessStage(string(preview.NextStage)) {
		return SubsystemAccessPreview{}, fmt.Errorf("facts returned an unknown stage %q: %w", preview.NextStage, ErrSubsystemAccessInvalidRequest)
	}
	return preview, nil
}

// Submit 重新检查事实后创建操作：预览版本变化即冲突；同键同内容返回原操作；已一致时不创建。
func (service *SubsystemAccessService) Submit(ctx context.Context, request SubsystemAccessSubmitRequest) (SubsystemAccessSubmitResult, error) {
	tenantID := strings.TrimSpace(request.TenantID)
	applicationCode := strings.TrimSpace(request.ApplicationCode)
	environmentCode := strings.ToLower(strings.TrimSpace(request.EnvironmentCode))
	operatorID := strings.TrimSpace(request.OperatorID)
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if tenantID == "" || applicationCode == "" || environmentCode == "" || operatorID == "" || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return SubsystemAccessSubmitResult{}, fmt.Errorf("submit requires tenant, target, operator and idempotency key: %w", ErrSubsystemAccessInvalidRequest)
	}
	publicOrigin := strings.TrimSpace(request.PublicOrigin)
	if len(publicOrigin) > 512 {
		return SubsystemAccessSubmitResult{}, fmt.Errorf("public origin is too long: %w", ErrSubsystemAccessInvalidRequest)
	}
	if publicOrigin != "" {
		parsed, err := url.ParseRequestURI(publicOrigin)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return SubsystemAccessSubmitResult{}, fmt.Errorf("public origin is invalid: %w", ErrSubsystemAccessInvalidRequest)
		}
	}
	authProvider := strings.TrimSpace(request.AuthProvider)
	initialAdminID := strings.TrimSpace(request.InitialAdminID)
	if len(authProvider) > 64 || (initialAdminID != "" && (len(initialAdminID) != 26 || !validIdentifier(initialAdminID))) {
		return SubsystemAccessSubmitResult{}, fmt.Errorf("authentication selection is invalid: %w", ErrSubsystemAccessInvalidRequest)
	}
	facts, version, err := service.readFacts(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return SubsystemAccessSubmitResult{}, err
	}
	// 提交必须携带预览版本，且与当前事实一致；否则要求重新预览。
	if strings.TrimSpace(request.PreviewVersion) == "" || strings.TrimSpace(request.PreviewVersion) != version.Version {
		return SubsystemAccessSubmitResult{}, ErrSubsystemAccessOperationConflict
	}
	decision := DecideSubsystemAccessAction(facts)
	if decision.DisplayOnly {
		return SubsystemAccessSubmitResult{DisplayOnly: true, Decision: decision}, nil
	}
	digest, err := subsystemAccessRequestDigest(request, decision.Action)
	if err != nil {
		return SubsystemAccessSubmitResult{}, err
	}
	now := service.clock.Now().UTC()
	operationID, err := service.ids.New(now)
	if err != nil {
		return SubsystemAccessSubmitResult{}, err
	}
	stage := facts.ResumeStage
	if !ValidSubsystemAccessStage(string(stage)) {
		stage = SubsystemAccessStageCheck
	}
	operation, created, err := service.repository.CreateOperation(ctx, SubsystemAccessOperationCreate{
		ID: operationID, TenantID: tenantID, ApplicationID: facts.ApplicationID, EnvironmentID: facts.EnvironmentID,
		ApplicationCode: applicationCode, EnvironmentCode: environmentCode,
		Action: decision.Action, Stage: stage, PreviewVersion: version.Version, RequestDigest: digest,
		PublicOrigin: strings.TrimSpace(request.PublicOrigin), AuthProvider: strings.TrimSpace(request.AuthProvider),
		InitialAdminID: strings.TrimSpace(request.InitialAdminID),
		IdempotencyKey: idempotencyKey, OperatorID: operatorID, Now: now,
	})
	if err != nil {
		return SubsystemAccessSubmitResult{}, err
	}
	return SubsystemAccessSubmitResult{Operation: operation, Created: created, Decision: decision}, nil
}

// Resume 在条件补齐后继续：重新检查事实，满足则回到可领取状态，否则维持等待并返回新的缺项。
func (service *SubsystemAccessService) Resume(ctx context.Context, tenantID, operationID string) (SubsystemAccessOperationView, error) {
	operation, err := service.repository.GetOperation(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(operationID))
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	if operation.Status != SubsystemAccessStatusWaiting && operation.Status != SubsystemAccessStatusFailed {
		return SubsystemAccessOperationView{}, fmt.Errorf("operation %s is %s and cannot resume: %w", operation.ID, operation.Status, ErrSubsystemAccessOperationConflict)
	}
	facts, _, err := service.readFacts(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	missing := describeSubsystemAccessMissing(facts)
	blocked := len(missing) > 0
	if !blocked {
		if _, err := service.repository.RequeueOperation(ctx, operation.TenantID, operation.ID, service.clock.Now().UTC()); err != nil {
			return SubsystemAccessOperationView{}, err
		}
	}
	return service.Describe(ctx, operation.TenantID, operation.ID)
}

// Describe 返回操作、阶段与分项结果。
func (service *SubsystemAccessService) Describe(ctx context.Context, tenantID, operationID string) (SubsystemAccessOperationView, error) {
	operation, err := service.repository.GetOperation(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(operationID))
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	stages, err := service.repository.ListOperationStages(ctx, operation.TenantID, operation.ID)
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	facts, _, err := service.readFacts(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	return SubsystemAccessOperationView{Operation: operation, Stages: stages,
		Decision: DecideSubsystemAccessAction(facts), Missing: describeSubsystemAccessMissing(facts)}, nil
}

// FindByTarget 供页面刷新或 CLI 重连定位当前操作。
func (service *SubsystemAccessService) FindByTarget(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessOperationView, bool, error) {
	operation, found, err := service.repository.FindOperationByTarget(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environmentCode)))
	if err != nil || !found {
		return SubsystemAccessOperationView{}, false, err
	}
	view, err := service.Describe(ctx, operation.TenantID, operation.ID)
	if err != nil {
		return SubsystemAccessOperationView{}, false, err
	}
	return view, true, nil
}

func (service *SubsystemAccessService) readFacts(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessFacts, SubsystemAccessPreviewVersion, error) {
	tenantID, applicationCode = strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode)
	environmentCode = strings.ToLower(strings.TrimSpace(environmentCode))
	if tenantID == "" || applicationCode == "" || environmentCode == "" {
		return SubsystemAccessFacts{}, SubsystemAccessPreviewVersion{}, fmt.Errorf("facts lookup requires tenant, application and environment: %w", ErrSubsystemAccessInvalidRequest)
	}
	return service.facts.ReadSubsystemAccessFacts(ctx, tenantID, applicationCode, environmentCode)
}

// subsystemAccessExecutionPath 返回动作的执行入口。
//
// 四类动作现在都走统一入口：CHECK 按动作选预检、AUTH 按动作准备凭据（首次接入走原子
// 流程，接管/更新/重试委托受控更新）、DEPLOY 按动作分派、READINESS 核对代次与清单、
// RESULT 完成初始管理员与认证投影。因此不再给出 CLI 提示把用户引回旧入口。
func subsystemAccessExecutionPath(decision SubsystemAccessDecision, _, _ string) (string, string) {
	switch decision.Action {
	case SubsystemAccessActionOnboard, SubsystemAccessActionAdopt, SubsystemAccessActionUpdate, SubsystemAccessActionRetry:
		return SubsystemAccessExecutionUnified, ""
	default:
		return SubsystemAccessExecutionNone, ""
	}
}

// describeSubsystemAccessMissing 把事实折算成用户可执行的缺项清单；顺序稳定便于测试与界面展示。
func describeSubsystemAccessMissing(facts SubsystemAccessFacts) []SubsystemAccessMissing {
	missing := []SubsystemAccessMissing{}
	if !facts.InScope {
		missing = append(missing, SubsystemAccessMissing{Code: "OUT_OF_SCOPE", Message: "目标不在审核清单或交付范围内",
			NextAction: "确认应用编码与环境；如确需接入，先在审核清单中登记该目标"})
	}
	if !facts.ApprovalReady {
		missing = append(missing, SubsystemAccessMissing{Code: "RUNTIME_APPROVAL_MISSING", Message: "缺少运行批准",
			NextAction: "生成批准候选并完成审核发布（refresh-approvals --approve）"})
	}
	if !facts.LicenseReady {
		missing = append(missing, SubsystemAccessMissing{Code: "LICENSE_MISSING", Message: "缺少商业授权",
			NextAction: "在「商业授权」完成初始化并导入签发产物"})
	}
	if !facts.ImagesReady {
		if subsystemAccessFactUnverified(facts, "images") {
			// 未上报 ≠ 缺失：不能因为读不到状态就让用户去"准备镜像"。
			missing = append(missing, SubsystemAccessMissing{Code: "IMAGES_UNVERIFIED", Message: "镜像就绪状态未上报（无法确认）",
				NextAction: "确认部署 Agent 版本并重试条件检查；未确认前不要按缺少镜像处理"})
		} else {
			missing = append(missing, SubsystemAccessMissing{Code: "IMAGES_MISSING", Message: "缺少所需镜像",
				NextAction: "准备候选镜像，或先对固定记录执行受控更新"})
		}
	}
	if facts.AuthObservationPending {
		missing = append(missing, SubsystemAccessMissing{Code: "AUTH_OBSERVATION_PENDING", Message: "认证观察期未结束",
			NextAction: "等待观察期结束；不得通过改写 issuer 别名绕过"})
	}
	return missing
}

func missingPermissions(required, held []string) []string {
	if len(required) == 0 {
		return nil
	}
	heldSet := map[string]struct{}{}
	for _, permission := range held {
		heldSet[strings.TrimSpace(permission)] = struct{}{}
	}
	if len(heldSet) == 0 {
		return nil
	}
	missing := []string{}
	for _, permission := range required {
		if _, ok := heldSet[permission]; !ok {
			missing = append(missing, permission)
		}
	}
	sort.Strings(missing)
	return missing
}

// subsystemAccessRequestDigest 计算请求摘要：同幂等键下内容不同必须被拒。
func subsystemAccessRequestDigest(request SubsystemAccessSubmitRequest, action SubsystemAccessAction) (string, error) {
	canonical := map[string]string{
		"tenant_id":        strings.TrimSpace(request.TenantID),
		"application_code": strings.TrimSpace(request.ApplicationCode),
		"environment_code": strings.ToLower(strings.TrimSpace(request.EnvironmentCode)),
		"action":           string(action),
		"preview_version":  strings.TrimSpace(request.PreviewVersion),
		"public_origin":    strings.TrimSpace(request.PublicOrigin),
		"auth_provider":    strings.TrimSpace(request.AuthProvider),
		"initial_admin_id": strings.TrimSpace(request.InitialAdminID),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode subsystem access request digest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// SubsystemAccessClock 适配器：把 time.Now 作为 Clock 使用（组合根复用）。
type SubsystemAccessClock struct{}

// Now 返回本地当前时间；仅组合根装配使用，业务路径一律走注入的 Clock。
func (SubsystemAccessClock) Now() time.Time { return time.Now() }

// subsystemAccessFactUnverified 判断某项事实是否处于"未验证"状态。
func subsystemAccessFactUnverified(facts SubsystemAccessFacts, name string) bool {
	for _, item := range facts.Unverified {
		if strings.EqualFold(strings.TrimSpace(item), name) {
			return true
		}
	}
	return false
}
