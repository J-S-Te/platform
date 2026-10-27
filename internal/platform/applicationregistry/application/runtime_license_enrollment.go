package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
)

// RuntimeLicenseApproval 由隔离部署 Agent 的已评审发行提供，
// 绝不接受来自 HTTP 接入请求或可变镜像标签的同名数据，防止审批被伪造。
type RuntimeLicenseApproval struct {
	ServiceID      string `json:"service_id"`
	Protocol       int    `json:"protocol"`
	CoverageDigest string `json:"coverage_digest"`
	ImageDigest    string `json:"image_digest"`
}

// RuntimeLicenseCredential 是交付给部署 Agent 的单组件凭据包：
// 一次仅经审批通道下发，明文 Secret 不落库、不进日志。
type RuntimeLicenseCredential struct {
	// RetirementNotRequired 仅由受控登记的持久成员核对生成；默认 false 保留退役门禁。
	RetirementNotRequired bool                   `json:"retirement_not_required,omitempty"`
	Approval              RuntimeLicenseApproval `json:"approval"`
	RegistrationProtocol  int                    `json:"registration_protocol,omitempty"`
	CatalogVersion        int                    `json:"catalog_version,omitempty"`
	CredentialGeneration  string                 `json:"credential_generation,omitempty"`
	ClientID              string                 `json:"client_id"`
	ClientSecret          string                 `json:"client_secret"`
	ReleaseDigest         string                 `json:"release_digest,omitempty"`
}

// RuntimeLicenseSettings 携带内置目录路径所需的实例级设置；
// InstanceID 参与客户端 ID 推导，为空则无法走 V2 目录路径。
type RuntimeLicenseSettings struct {
	InstanceID            string
	Environment           string
	PlatformBaseURL       string
	PlatformPublicKeyPath string
	StateDirectory        string
	AllowHTTP             bool
	// ServiceIDs 把登记范围限定到所选目录成员，用于受控的部分桥接；为空表示全量目录。
	ServiceIDs []string
}

// RuntimeLicenseApprovalSource 提供 V1 遗留路径的已审批组件清单；
// 实现只读审批产物，不得在读取时生成或修补审批内容。
type RuntimeLicenseApprovalSource interface {
	ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]RuntimeLicenseApproval, error)
}

// RuntimeLicenseReleaseApproval 是一次完整的发行审批：组件清单、必选/退役
// 服务集合与发行代次。摘要绑定全部字段，任一改动都会改变 ReleaseDigest。
type RuntimeLicenseReleaseApproval struct {
	ReleaseGeneration  string                   `json:"release_generation,omitempty"`
	Components         []RuntimeLicenseApproval `json:"components"`
	RequiredServiceIDs []string                 `json:"required_service_ids"`
	RetireServiceIDs   []string                 `json:"retire_service_ids"`
}

// RuntimeLicenseLifecycleApprovalSource 提供带发行代次的完整生命周期审批；
// 缺失时协调层只能做无 CAS 的注册，不允许用于成员变更。
type RuntimeLicenseLifecycleApprovalSource interface {
	ApprovedRuntimeLicenseLifecycle(context.Context, string, string) (RuntimeLicenseReleaseApproval, error)
}

type runtimeLicenseLifecycleVerifier struct {
	source RuntimeLicenseLifecycleApprovalSource
}

// NewRuntimeLicenseLifecycleVerifier 构造协调层的审批校验器；source 缺失按
// ErrValidation 拒绝，绝不允许出现"无审批源也放行"的校验器。
func NewRuntimeLicenseLifecycleVerifier(source RuntimeLicenseLifecycleApprovalSource) (coordination.LifecycleApprovalVerifier, error) {
	if source == nil {
		return nil, ErrValidation
	}
	return runtimeLicenseLifecycleVerifier{source}, nil
}

// RuntimeLicenseClientID 从应用、环境与审批内容推导 V1 组件的稳定机器身份；
// 不含凭据代次，因此凭据轮换不改变客户端标识。
func RuntimeLicenseClientID(app, env string, item RuntimeLicenseApproval) string {
	return runtimeLicenseClientIDWithGeneration(app, env, item, "")
}

// runtimeLicenseClientIDWithGeneration 在 V1 基础上混入凭据代次派生客户端 ID。
// 前缀截断到 95 字符以兼容 OAuth 客户端 ID 长度约束，剩余区分度由摘要保证。
func runtimeLicenseClientIDWithGeneration(app, env string, item RuntimeLicenseApproval, generation string) string {
	raw, _ := json.Marshal([]any{app, env, item.ServiceID, item.Protocol, item.CoverageDigest, item.ImageDigest, generation})
	digest := sha256.Sum256(raw)
	prefix := app + "-" + env + "-license-" + item.ServiceID
	if len(prefix) > 95 {
		prefix = prefix[:95]
	}
	return prefix + "-" + hex.EncodeToString(digest[:16])
}

// canonicalRuntimeRelease 校验并规范化一次发行审批：组件/必选/退役清单通过
// 校验、必选项必须都在组件内、退役项不得与组件重叠；随后深拷贝并按 ServiceID
// 排序，保证同一审批内容无论字段顺序如何都得到同一摘要，供 CAS 比对。
func canonicalRuntimeRelease(release RuntimeLicenseReleaseApproval) (RuntimeLicenseReleaseApproval, string, error) {
	if ValidateRuntimeLicenseApprovals(release.Components) != nil || len(release.RequiredServiceIDs) == 0 || (release.ReleaseGeneration != "" && !runtimeLicenseID.MatchString(release.ReleaseGeneration)) {
		return release, "", ErrValidation
	}
	release.Components = append([]RuntimeLicenseApproval(nil), release.Components...)
	release.RequiredServiceIDs = append([]string(nil), release.RequiredServiceIDs...)
	release.RetireServiceIDs = append([]string(nil), release.RetireServiceIDs...)
	ids := map[string]bool{}
	for _, item := range release.Components {
		ids[item.ServiceID] = true
	}
	seen := map[string]bool{}
	for _, id := range release.RequiredServiceIDs {
		if !ids[id] || seen[id] {
			return release, "", ErrValidation
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range release.RetireServiceIDs {
		if !runtimeLicenseID.MatchString(id) || ids[id] || seen[id] {
			return release, "", ErrValidation
		}
		seen[id] = true
	}
	sort.Slice(release.Components, func(i, j int) bool { return release.Components[i].ServiceID < release.Components[j].ServiceID })
	sort.Strings(release.RequiredServiceIDs)
	sort.Strings(release.RetireServiceIDs)
	raw, err := json.Marshal(release)
	if err != nil {
		return release, "", err
	}
	digest := sha256.Sum256(raw)
	return release, "sha256:" + hex.EncodeToString(digest[:]), nil
}

// RuntimeLicenseReleaseDigest 把交付与退役绑定到同一份完整审批（含组件选择、
// 必选清单与发行代次），作为协调层 CAS 的期望摘要；审批内容任何改动都会
// 使旧摘要失效，防止两次发行被拼接执行。
func RuntimeLicenseReleaseDigest(release RuntimeLicenseReleaseApproval) (string, error) {
	_, digest, err := canonicalRuntimeRelease(release)
	return digest, err
}

// VerifyRuntimeLifecycle 实现 coordination.LifecycleApprovalVerifier：
// 读取审批并重算规范化摘要，与输入的 ReleaseDigest 不一致即拒绝，
// 保证协调层只执行与审批摘要完全一致的成员变更。
func (v runtimeLicenseLifecycleVerifier) VerifyRuntimeLifecycle(ctx context.Context, in coordination.LifecycleInput) (coordination.LifecycleApproval, error) {
	release, err := v.source.ApprovedRuntimeLicenseLifecycle(ctx, in.Application, in.Environment)
	if err != nil {
		return coordination.LifecycleApproval{}, err
	}
	release, digest, err := canonicalRuntimeRelease(release)
	if err != nil || digest != in.ReleaseDigest {
		return coordination.LifecycleApproval{}, coordination.ErrForbidden
	}
	approved := coordination.LifecycleApproval{RequiredServiceIDs: release.RequiredServiceIDs, RetireServiceIDs: release.RetireServiceIDs}
	for _, item := range release.Components {
		approved.Specs = append(approved.Specs, coordination.ServiceSpec{Application: in.Application, Environment: in.Environment, ServiceID: item.ServiceID, OAuthClientID: runtimeLicenseClientIDWithGeneration(in.Application, in.Environment, item, release.ReleaseGeneration), CoverageDigest: item.CoverageDigest, ImageDigest: item.ImageDigest})
	}
	return approved, nil
}

// runtimeLicenseLifecycleCoordinator 是 V1 路径所需的协调器能力子集：
// 读状态与显式 CAS 成员调和；不支持时回退为逐组件 Register。
type runtimeLicenseLifecycleCoordinator interface {
	Status(context.Context, string) (coordination.Status, error)
	ReconcileComponents(context.Context, coordination.LifecycleInput, domain.Actor) error
}

// RuntimeLicenseClientManager 暴露登记流程所需的 OAuth 客户端管理能力；
// 实现必须把明文 Secret 仅经返回值交付一次，不得持久化明文。
type RuntimeLicenseClientManager interface {
	ListOAuthClients(context.Context, string) ([]OAuthClientView, error)
	CreateOAuthClient(context.Context, OAuthClientCreateInput) (OAuthClientCreateResult, error)
	CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput) (OAuthClientSecretResult, error)
}

// RuntimeLicenseRegistrar 向许可协调层登记组件成员；实现不得隐式覆盖
// 既有成员，覆盖必须走显式 CAS 调和。
type RuntimeLicenseRegistrar interface {
	Register(context.Context, coordination.ServiceSpec) error
}

// RuntimeLicenseEnrollmentService 把已评审发行转换为可交付的组件凭据：
// 建立或复用机器客户端、执行受控成员调和，并在注册成功后才轮换 Secret。
type RuntimeLicenseEnrollmentService struct {
	source    RuntimeLicenseApprovalSource
	catalog   RuntimeComponentCatalogSource
	clients   RuntimeLicenseClientManager
	registrar RuntimeLicenseRegistrar
}

// ErrRuntimeLicenseNotInitialized 表示商业运行实例尚未初始化（无任何审批源）；
// 它是用户可诊断的根因，不得折叠为通用部署失败。
var ErrRuntimeLicenseNotInitialized = errors.New("commercial runtime instance not initialized")

// NewRuntimeLicenseEnrollmentService 构造仅含 V1 审批源的登记服务，
// 供尚未启用内置目录的既有安装使用。
func NewRuntimeLicenseEnrollmentService(source RuntimeLicenseApprovalSource, clients RuntimeLicenseClientManager, registrar RuntimeLicenseRegistrar) (*RuntimeLicenseEnrollmentService, error) {
	return NewRuntimeLicenseEnrollmentServiceWithCatalog(source, nil, clients, registrar)
}

// NewRuntimeLicenseEnrollmentServiceWithCatalog 为新 V2 安装启用内置目录路径，
// 同时保留 V1 审批源服务遗留应用；既有 V2 应用的桥接仍由 coordination 层的
// 受控调和完成（Task 5）。两个来源同时缺失时拒绝构造。
func NewRuntimeLicenseEnrollmentServiceWithCatalog(source RuntimeLicenseApprovalSource, catalogSource RuntimeComponentCatalogSource, clients RuntimeLicenseClientManager, registrar RuntimeLicenseRegistrar) (*RuntimeLicenseEnrollmentService, error) {
	if (source == nil && catalogSource == nil) || clients == nil || registrar == nil {
		return nil, ErrValidation
	}
	return &RuntimeLicenseEnrollmentService{source: source, catalog: catalogSource, clients: clients, registrar: registrar}, nil
}

var runtimeLicenseID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var runtimeLicenseDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidateRuntimeLicenseApprovals 校验 V1 审批清单：数量 1..32、ServiceID 命名
// 合规且不重复、协议固定为 1、两个摘要必须是规范的 sha256 表示。
// 任一不满足返回 ErrValidation。
func ValidateRuntimeLicenseApprovals(items []RuntimeLicenseApproval) error {
	if len(items) == 0 || len(items) > 32 {
		return ErrValidation
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !runtimeLicenseID.MatchString(item.ServiceID) || item.Protocol != 1 || !runtimeLicenseDigest.MatchString(item.CoverageDigest) || !runtimeLicenseDigest.MatchString(item.ImageDigest) || seen[item.ServiceID] {
			return ErrValidation
		}
		seen[item.ServiceID] = true
	}
	return nil
}

// Prepare 以空设置交付已评审完整发行中的独立机器身份。
// 既有成员变更一律走显式 CAS 调和，绝不用 Register 覆盖；
// 已激活应用在灰度期间保持限制口径，不因准备动作放开。
func (s *RuntimeLicenseEnrollmentService) Prepare(ctx context.Context, tenant, applicationID, environmentID, app, env, operator string) ([]RuntimeLicenseCredential, error) {
	return s.PrepareWithSettings(ctx, tenant, applicationID, environmentID, app, env, operator, RuntimeLicenseSettings{})
}

// PrepareWithSettings 为新安装、已持久化的 V2/桥接应用选择内置目录路径，
// 为遗留 V1 应用选择审批源路径（受控升级经 ReconcileBuiltinComponents 桥接，
// 不再要求重新提供审批文件）。返回的凭据只在注册成功后包含明文 Secret。
func (s *RuntimeLicenseEnrollmentService) PrepareWithSettings(ctx context.Context, tenant, applicationID, environmentID, app, env, operator string, settings RuntimeLicenseSettings) ([]RuntimeLicenseCredential, error) {
	if ctx == nil || strings.TrimSpace(tenant) == "" || applicationID == "" || environmentID == "" || operator == "" || !runtimeLicenseID.MatchString(app) || !runtimeLicenseID.MatchString(env) {
		return nil, ErrValidation
	}
	if s.catalog != nil {
		if coordinator, ok := s.registrar.(interface {
			Status(context.Context, string) (coordination.Status, error)
		}); ok {
			status, statusErr := coordinator.Status(ctx, app)
			if errors.Is(statusErr, coordination.ErrNotReady) {
				return s.prepareBuiltin(ctx, tenant, applicationID, environmentID, app, env, operator, settings)
			}
			if statusErr == nil {
				switch status.Application.RegistrationMode {
				case catalogBridgingMode, catalogBuiltinMode, catalogLegacyMode, "":
					return s.prepareBuiltin(ctx, tenant, applicationID, environmentID, app, env, operator, settings)
				default:
					return nil, ErrValidation
				}
			} else if !errors.Is(statusErr, coordination.ErrNotReady) {
				return nil, statusErr
			}
		} else if settings.InstanceID != "" {
			return s.prepareBuiltin(ctx, tenant, applicationID, environmentID, app, env, operator, settings)
		}
	}
	return s.prepareApproved(ctx, tenant, applicationID, environmentID, app, env, operator)
}

const (
	// 运行时登记的三种目录模式：遗留镜像（V1）、桥接组件（V2 过渡）、内置组件（V2 目标态）。
	catalogLegacyMode   = "LEGACY_IMAGE_V1"
	catalogBridgingMode = "BRIDGING_COMPONENT_V2"
	catalogBuiltinMode  = "BUILTIN_COMPONENT_V2"
)

func runtimeComponentSetDigest(set RuntimeComponentSet) string {
	raw, _ := json.Marshal(set)
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (s *RuntimeLicenseEnrollmentService) prepareBuiltin(ctx context.Context, tenant, applicationID, environmentID, app, env, operator string, settings RuntimeLicenseSettings) ([]RuntimeLicenseCredential, error) {
	if s.catalog == nil || strings.TrimSpace(settings.InstanceID) == "" {
		return nil, ErrValidation
	}
	set, err := s.catalog.RuntimeComponentSet(ctx, app, env)
	if err != nil || ValidateRuntimeComponentSet(set) != nil {
		return nil, ErrRuntimeLicenseApprovalMismatch
	}
	set, err = SelectRuntimeComponentSet(set, settings.ServiceIDs)
	if err != nil {
		return nil, ErrValidation
	}
	clients, err := s.clients.ListOAuthClients(ctx, tenant)
	if err != nil {
		return nil, errors.New("commercial runtime client lookup failed")
	}
	byID := map[string]OAuthClientView{}
	for _, client := range clients {
		byID[client.ClientID] = client
	}
	result := make([]RuntimeLicenseCredential, 0, len(set.Components))
	specs := make([]coordination.ServiceSpec, 0, len(set.Components))
	rotate := make([]OAuthClientView, 0, len(set.Components))
	for _, component := range set.Components {
		id := RuntimeLicenseClientIDV2(settings.InstanceID, app, env, component.ServiceID, component.CredentialGeneration)
		client, found := byID[id]
		secret := ""
		if !found {
			created, createErr := s.clients.CreateOAuthClient(ctx, OAuthClientCreateInput{TenantID: tenant, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operator, ClientID: id, ClientName: app + " Runtime " + component.ServiceID, ClientType: "service", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 300, GrantTypes: []string{"client_credentials"}, Scopes: []string{"license.runtime"}})
			if createErr != nil {
				return nil, errors.New("commercial runtime client creation failed")
			}
			client, secret = created.Client, created.PlaintextSecret
		}
		if client.ClientID != id || client.ApplicationID != applicationID || client.EnvironmentID != environmentID || client.Status != "ACTIVE" || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || len(client.Scopes) != 1 || client.Scopes[0] != "license.runtime" || len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" {
			return nil, ErrConflict
		}
		if !found && secret == "" {
			return nil, errors.New("commercial runtime credential incomplete")
		}
		specs = append(specs, coordination.ServiceSpec{Application: app, Environment: env, ServiceID: component.ServiceID, OAuthClientID: client.ClientID, RegistrationProtocol: component.RegistrationProtocol, CatalogVersion: component.CatalogVersion, CredentialGeneration: component.CredentialGeneration})
		rotate = append(rotate, client)
		result = append(result, RuntimeLicenseCredential{Approval: RuntimeLicenseApproval{ServiceID: component.ServiceID, Protocol: component.RegistrationProtocol}, RegistrationProtocol: component.RegistrationProtocol, CatalogVersion: component.CatalogVersion, CredentialGeneration: component.CredentialGeneration, ClientID: client.ClientID, ClientSecret: secret, ReleaseDigest: runtimeComponentSetDigest(set)})
	}
	registered := false
	retirementNotRequired := false
	if coordinator, ok := s.registrar.(interface {
		Status(context.Context, string) (coordination.Status, error)
	}); ok {
		status, statusErr := coordinator.Status(ctx, app)
		if errors.Is(statusErr, coordination.ErrNotReady) {
			retirementNotRequired = true
		}
		if statusErr == nil {
			// Status excludes tombstones. An independent persisted-history check is
			// required, so a failed retirement cannot disappear on a later retry.
			retirementNotRequired = false
			if checker, ok := s.registrar.(interface {
				RuntimeRetirementNotRequired(context.Context, string, uint64, []string) (bool, error)
			}); ok {
				ids := make([]string, 0, len(set.Components))
				for _, component := range set.Components {
					ids = append(ids, component.ServiceID)
				}
				safe, err := checker.RuntimeRetirementNotRequired(ctx, app, status.Revision, ids)
				if err != nil {
					return nil, errors.New("commercial runtime retirement history unavailable")
				}
				retirementNotRequired = safe
			}
			reconciler, ok := s.registrar.(interface {
				ReconcileBuiltinComponents(context.Context, coordination.BuiltinReconcileInput, domain.Actor) error
			})
			if !ok {
				return nil, errors.New("commercial runtime bridge coordinator unavailable")
			}
			generation := ""
			if len(set.Components) > 0 {
				generation = set.Components[0].CredentialGeneration
			}
			operationHash := sha256.Sum256([]byte(app + "\x00" + env + "\x00" + runtimeComponentSetDigest(set)))
			operationID := fmt.Sprintf("builtin-%x", operationHash)
			if reconcileErr := reconciler.ReconcileBuiltinComponents(ctx, coordination.BuiltinReconcileInput{
				Application: app, Environment: env, OperationID: operationID, ExpectedRevision: status.Revision,
				CatalogVersion: set.CatalogVersion, CredentialGeneration: generation, ServiceIDs: settings.ServiceIDs,
			}, domain.Actor{TenantID: tenant, UserID: operator}); reconcileErr != nil {
				return nil, errors.New("commercial runtime controlled component bridge failed")
			}
			registered = true
		} else if !errors.Is(statusErr, coordination.ErrNotReady) {
			return nil, errors.New("commercial runtime component status unavailable")
		}
	}
	if !registered {
		for _, spec := range specs {
			if err := s.registrar.Register(ctx, spec); err != nil {
				return nil, errors.New("commercial runtime component registration failed")
			}
		}
	}
	for index, client := range rotate {
		result[index].RetirementNotRequired = retirementNotRequired
		if client.ID != "" {
			rotated, rotateErr := s.clients.CreateOAuthClientSecret(ctx, OAuthClientSecretCreateInput{TenantID: tenant, OAuthClientID: client.ID, OperatorID: operator})
			if rotateErr != nil || rotated.PlaintextSecret == "" {
				return nil, errors.New("commercial runtime credential delivery failed")
			}
			result[index].ClientSecret = rotated.PlaintextSecret
		}
	}
	return result, nil
}

func (s *RuntimeLicenseEnrollmentService) prepareApproved(ctx context.Context, tenant, applicationID, environmentID, app, env, operator string) ([]RuntimeLicenseCredential, error) {
	if s.source == nil {
		return nil, ErrRuntimeLicenseNotInitialized
	}
	items, err := s.source.ApprovedRuntimeLicenseComponents(ctx, app, env)
	if err != nil {
		return nil, RuntimeLicenseApprovalFailure(err)
	}
	if ValidateRuntimeLicenseApprovals(items) != nil {
		return nil, ErrRuntimeLicenseApprovalMismatch
	}
	var release RuntimeLicenseReleaseApproval
	var releaseDigest string
	if source, ok := s.source.(RuntimeLicenseLifecycleApprovalSource); ok {
		release, err = source.ApprovedRuntimeLicenseLifecycle(ctx, app, env)
		if err != nil {
			return nil, RuntimeLicenseApprovalFailure(err)
		}
		release, releaseDigest, err = canonicalRuntimeRelease(release)
		if err != nil {
			return nil, ErrRuntimeLicenseApprovalMismatch
		}
		// 重读可变审批时必须核对一致性：绝不允许把一次发行的凭据与另一次
		// 发行的退役权限拼接使用。
		base, _, e := canonicalRuntimeRelease(RuntimeLicenseReleaseApproval{Components: items, RequiredServiceIDs: release.RequiredServiceIDs})
		if e != nil {
			return nil, ErrRuntimeLicenseApprovalMismatch
		}
		baseJSON, _ := json.Marshal(base.Components)
		releaseJSON, _ := json.Marshal(release.Components)
		if string(baseJSON) != string(releaseJSON) {
			return nil, ErrRuntimeLicenseApprovalMismatch
		}
		items = release.Components
	}
	clients, err := s.clients.ListOAuthClients(ctx, tenant)
	if err != nil {
		return nil, errors.New("commercial runtime client lookup failed")
	}
	byID := map[string]OAuthClientView{}
	for _, c := range clients {
		byID[c.ClientID] = c
	}
	result := make([]RuntimeLicenseCredential, 0, len(items))
	specs := make([]coordination.ServiceSpec, 0, len(items))
	rotate := make([]OAuthClientView, 0, len(items))
	for _, item := range items {
		id := runtimeLicenseClientIDWithGeneration(app, env, item, release.ReleaseGeneration)
		client, found := byID[id]
		var secret string
		if !found {
			created, e := s.clients.CreateOAuthClient(ctx, OAuthClientCreateInput{TenantID: tenant, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operator, ClientID: id, ClientName: app + " Runtime " + item.ServiceID, ClientType: "service", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 300, GrantTypes: []string{"client_credentials"}, Scopes: []string{"license.runtime"}})
			if e != nil {
				return nil, errors.New("commercial runtime client creation failed")
			}
			client, secret = created.Client, created.PlaintextSecret
		}
		if client.ClientID != id || client.ApplicationID != applicationID || client.EnvironmentID != environmentID || client.Status != "ACTIVE" || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || len(client.Scopes) != 1 || client.Scopes[0] != "license.runtime" || len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" {
			return nil, ErrConflict
		}
		if client.ClientID == "" || (!found && secret == "") {
			return nil, errors.New("commercial runtime credential incomplete")
		}
		specs = append(specs, coordination.ServiceSpec{Application: app, Environment: env, ServiceID: item.ServiceID, OAuthClientID: client.ClientID, CoverageDigest: item.CoverageDigest, ImageDigest: item.ImageDigest})
		if found {
			rotate = append(rotate, client)
		} else {
			rotate = append(rotate, OAuthClientView{})
		}
		result = append(result, RuntimeLicenseCredential{Approval: item, ClientID: client.ClientID, ClientSecret: secret, ReleaseDigest: releaseDigest})
	}
	registered := false
	if coordinator, ok := s.registrar.(runtimeLicenseLifecycleCoordinator); ok {
		status, e := coordinator.Status(ctx, app)
		if e != nil && !errors.Is(e, coordination.ErrNotReady) {
			return nil, errors.New("commercial runtime component status unavailable")
		}
		if e == nil {
			current := map[string]coordination.ServiceSpec{}
			for _, member := range status.Members {
				if member.RetiredAt == nil {
					current[member.ServiceID] = coordination.ServiceSpec{Application: member.Application, Environment: member.Environment, ServiceID: member.ServiceID, OAuthClientID: member.OAuthClientID, CoverageDigest: member.CoverageDigest, ImageDigest: member.ImageDigest}
				}
			}
			changed := len(current) != len(specs)
			for _, spec := range specs {
				if current[spec.ServiceID] != spec {
					changed = true
				}
			}
			if changed {
				if releaseDigest == "" {
					return nil, errors.New("commercial runtime explicit replacement approval required")
				}
				operationDigest := sha256.Sum256([]byte(app + "\x00" + env + "\x00" + releaseDigest))
				operationID := fmt.Sprintf("release-%x-%d", operationDigest, status.Revision)
				if e = coordinator.ReconcileComponents(ctx, coordination.LifecycleInput{Application: app, Environment: env, ExpectedRevision: status.Revision, OperationID: operationID, ReleaseDigest: releaseDigest}, domain.Actor{TenantID: tenant, UserID: operator}); e != nil {
					return nil, errors.New("commercial runtime controlled component replacement failed")
				}
			}
			registered = true
		}
	}
	if !registered {
		for _, spec := range specs {
			if e := s.registrar.Register(ctx, spec); e != nil {
				return nil, errors.New("commercial runtime component registration failed")
			}
		}
	}
	// 只有注册/CAS 全部成功后才轮换 Secret；被拒绝的发行绝不能毁掉
	// 当前仍在工作的组件凭据。
	for i, client := range rotate {
		if client.ID != "" {
			rotated, e := s.clients.CreateOAuthClientSecret(ctx, OAuthClientSecretCreateInput{TenantID: tenant, OAuthClientID: client.ID, OperatorID: operator})
			if e != nil || rotated.PlaintextSecret == "" {
				return nil, errors.New("commercial runtime credential delivery failed")
			}
			result[i].ClientSecret = rotated.PlaintextSecret
		}
	}
	return result, nil
}
