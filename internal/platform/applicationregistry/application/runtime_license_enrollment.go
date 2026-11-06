package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// RuntimeLicenseEnrollmentService 把内置运行时目录转换为可交付的组件凭据：
// 建立或复用机器客户端、执行受控成员调和，并在注册成功后才轮换 Secret。
// V1 遗留审批源已下线：组件身份只来自编译进构建的目录（catalog.Version）。
type RuntimeLicenseEnrollmentService struct {
	catalog   RuntimeComponentCatalogSource
	clients   RuntimeLicenseClientManager
	registrar RuntimeLicenseRegistrar
}

// ErrRuntimeLicenseNotInitialized 表示商业运行实例尚未初始化；
// 它是用户可诊断的根因，不得折叠为通用部署失败。
var ErrRuntimeLicenseNotInitialized = errors.New("commercial runtime instance not initialized")

// NewRuntimeLicenseEnrollmentService 构造只按内置目录登记的服务。
// catalogSource 缺失时拒绝构造，不再回退到已下线的 V1 审批源路径。
func NewRuntimeLicenseEnrollmentService(catalogSource RuntimeComponentCatalogSource, clients RuntimeLicenseClientManager, registrar RuntimeLicenseRegistrar) (*RuntimeLicenseEnrollmentService, error) {
	if catalogSource == nil || clients == nil || registrar == nil {
		return nil, ErrValidation
	}
	return &RuntimeLicenseEnrollmentService{catalog: catalogSource, clients: clients, registrar: registrar}, nil
}

var runtimeLicenseID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var runtimeLicenseDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidateRuntimeLicenseApprovals 只保留对历史存档组件清单的格式校验；
// 运行组件登记不再使用 V1 审批清单。
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

// PrepareWithSettings 只按内置运行时目录交付组件身份。V1 审批源已下线：
// 目录缺失、商业实例未初始化或应用未迁移到 V2 时一律 fail-closed，
// 绝不回退到任何按文件审批的路径。返回的凭据只在注册成功后包含明文 Secret。
func (s *RuntimeLicenseEnrollmentService) PrepareWithSettings(ctx context.Context, tenant, applicationID, environmentID, app, env, operator string, settings RuntimeLicenseSettings) ([]RuntimeLicenseCredential, error) {
	if ctx == nil || strings.TrimSpace(tenant) == "" || applicationID == "" || environmentID == "" || operator == "" || !runtimeLicenseID.MatchString(app) || !runtimeLicenseID.MatchString(env) {
		return nil, ErrValidation
	}
	if s.catalog == nil {
		return nil, ErrRuntimeComponentCatalogUnavailable
	}
	if strings.TrimSpace(settings.InstanceID) == "" {
		return nil, ErrRuntimeLicenseNotInitialized
	}
	if coordinator, ok := s.registrar.(interface {
		Status(context.Context, string) (coordination.Status, error)
	}); ok {
		status, statusErr := coordinator.Status(ctx, app)
		if errors.Is(statusErr, coordination.ErrNotReady) {
			return s.prepareBuiltin(ctx, tenant, applicationID, environmentID, app, env, operator, settings)
		}
		if statusErr != nil {
			return nil, statusErr
		}
		// LEGACY_IMAGE_V1 仍在白名单内：存量应用第一次走 V2 时由本次登记
		// 迁到 BRIDGING_COMPONENT_V2，这是 V1→V2 的唯一入口，不能被误拒。
		switch status.Application.RegistrationMode {
		case catalogBridgingMode, catalogBuiltinMode, catalogLegacyMode, "":
			return s.prepareBuiltin(ctx, tenant, applicationID, environmentID, app, env, operator, settings)
		default:
			return nil, ErrValidation
		}
	}
	return s.prepareBuiltin(ctx, tenant, applicationID, environmentID, app, env, operator, settings)
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
