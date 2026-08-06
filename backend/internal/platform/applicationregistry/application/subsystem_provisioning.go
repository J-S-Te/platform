package application

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrSubsystemProvisioningUnavailable 将部署自动化失败收敛为稳定错误。HTTP 层只返回依赖不可用，
// 不把命令输出、文件路径、凭据或其他基础设施细节暴露给浏览器。
var ErrSubsystemProvisioningUnavailable = errors.New("subsystem provisioning unavailable")

const (
	SubsystemDeploymentStatusProvisioning = "PROVISIONING"
	SubsystemDeploymentStatusUpdating     = "UPDATING"
	SubsystemDeploymentStatusVerifying    = "VERIFYING"
	SubsystemDeploymentStatusReady        = "READY"
	SubsystemDeploymentStatusFailed       = "PROVISION_FAILED"
	SubsystemDeploymentStatusDraining     = "DRAINING"
	SubsystemDeploymentStatusOffboarded   = "OFFBOARDED"
	SubsystemServiceStatusDiscovered      = "DISCOVERED"
	SubsystemServiceStatusHealthy         = "HEALTHY"
	SubsystemServiceStatusUnavailable     = "UNAVAILABLE"
)

// SubsystemServiceInstance is the lightweight control-plane view of a discovered
// service. It contains routing metadata only; credentials and host filesystem paths
// are deliberately excluded.
type SubsystemServiceInstance struct {
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	Environment     string
	ServiceName     string
	ServiceRole     string
	Protocol        string
	InternalHost    string
	InternalPort    uint
	PathPrefix      string
	HealthEndpoint  string
	Version         string
	Status          string
	LastSeenAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UpstreamURL returns the service address that a gateway/proxy may use. It is derived from
// discovery data at request time and is never persisted as a generated server configuration file.
func (instance SubsystemServiceInstance) UpstreamURL() (string, error) {
	if instance.Protocol != "http" && instance.Protocol != "https" && instance.Protocol != "tcp" {
		return "", errors.New("unsupported service protocol")
	}
	if instance.InternalHost == "" || instance.InternalPort == 0 || instance.InternalPort > 65535 {
		return "", errors.New("invalid service address")
	}
	return fmt.Sprintf("%s://%s", instance.Protocol, net.JoinHostPort(instance.InternalHost, fmt.Sprintf("%d", instance.InternalPort))), nil
}

// SubsystemServiceRegistry persists discovered service instances. The Agent will use
// this contract through the existing local transport in the next migration step.
type SubsystemServiceRegistry interface {
	UpsertSubsystemServiceInstance(context.Context, SubsystemServiceInstance) error
	ListSubsystemServiceInstances(context.Context, string, string, string) ([]SubsystemServiceInstance, error)
	MarkUnavailableSubsystemServiceInstances(context.Context, time.Time, time.Time) error
}

// SubsystemDeploymentState 是最近一次部署尝试的持久控制面视图。Generation 区分重试轮次，
// 状态中刻意不保存凭据和原始命令输出，避免轮询接口成为秘密或主机信息泄露面。
type SubsystemDeploymentState struct {
	TenantID                string
	ApplicationID           string
	EnvironmentID           string
	ApplicationCode         string
	Environment             string
	InitialAdminUserID      string
	InitialAccessAssignedAt *time.Time
	Status                  string
	Operation               string
	Generation              uint64
	AttemptCount            uint
	LastErrorCode           string
	LastError               string
	StartedAt               *time.Time
	CompletedAt             *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// SubsystemDeploymentStateStore 将生命周期状态与耗时部署 Agent 解耦，使失败后可以仅重试部署，
// 而不重新创建无法恢复明文的 OAuth 凭据或重复执行首次接入。
type SubsystemDeploymentStateStore interface {
	TransitionSubsystemDeployment(context.Context, string, string, string, string, string, string, string, time.Time) error
	MarkSubsystemInitialAccessAssigned(context.Context, string, string, string, time.Time) error
	GetSubsystemDeploymentContext(context.Context, string, string, string) (SubsystemDeploymentState, error)
	GetSubsystemDeploymentState(context.Context, string, string, string) (SubsystemDeploymentState, error)
}

// SubsystemProvisioningCapabilities describes the safe deployment boundary exposed to the
// management console. It intentionally contains no host paths, tenant binding, image reference,
// credentials, or arbitrary command/service names. Targets are loaded from reviewed server-side
// deployment manifests; the browser can select one of them but cannot create a new target.
type SubsystemProvisioningCapabilities struct {
	Enabled                   bool
	Mode                      string
	SupportedApplicationCodes []string
	SupportedEnvironments     []string
	Targets                   []SubsystemProvisioningTarget
	DefaultApplicationCode    string
	DefaultApplicationName    string
	DefaultDescription        string
	DefaultEnvironment        string
	DefaultUpstreamURL        string
	DefaultPathPrefix         string
	DefaultClientType         string
}

// SubsystemProvisioningTarget is the non-sensitive projection of one reviewed deployment
// manifest. PublicBaseURL is entered by the operator because the browser-facing origin may vary
// by host, port or reverse-proxy deployment; the Agent still validates the reviewed path/upstream
// and the exact callback derived from that origin.
type SubsystemProvisioningTarget struct {
	ApplicationCode string
	ApplicationName string
	Description     string
	Environment     string
	UpstreamURL     string
	PathPrefix      string
	ClientType      string
	// InitialAdminRoles 是清单声明的接入初始管理员角色；nil 表示未声明（由平台默认决定）。
	InitialAdminRoles []string
	// AllowedServiceBindings 是清单声明的服务用途白名单（不含 audit_ingest 基线）；nil 表示未声明。
	AllowedServiceBindings []string
}

// SubsystemProvisioningInput 是一次性交付给子系统运行时的配置封套。ClientSecret 仅允许经过
// 进程内接口与受限 Unix socket 到达部署 Agent，不得写日志、返回浏览器或出现在命令行参数中。
type SubsystemProvisioningInput struct {
	TenantID        string
	ApplicationID   string
	ApplicationCode string
	Environment     string
	Issuer          string
	ClientID        string
	ClientSecret    string
	// CatalogPublisherClientID and CatalogPublisherClientSecret are a separate
	// service credential for authorization catalog synchronization.
	CatalogPublisherClientID     string
	CatalogPublisherClientSecret string
	// ServiceCredentials are one-time, purpose-bound credentials created during onboarding.
	// They are delivered only to the isolated deployment Agent and written to mode-0600 runtime
	// environment files; the browser response and operational logs never receive them.
	ServiceCredentials []SubsystemServiceCredential
	RedirectURI        string
	PublicURL          string
	PathPrefix         string
	UpstreamURL        string
}

// ServiceCredential 按业务用途取凭据，而不是按切片位置取值。这样新增集成能力时不会
// 因双方顺序不一致而把高权限密钥写入错误配置项。
func (input SubsystemProvisioningInput) ServiceCredential(purpose string) (SubsystemServiceCredential, bool) {
	for _, credential := range input.ServiceCredentials {
		if credential.Purpose == purpose && credential.OAuthClient.ClientID != "" && credential.PlaintextSecret != "" {
			return credential, true
		}
	}
	return SubsystemServiceCredential{}, false
}

// SubsystemPreflightInput 是控制面写入前交给部署 Agent 的公开参数。它不包含任何尚未
// 生成的 Secret，因此 Agent 可以在不可恢复凭据落库前校验生产模式、租户和固定拓扑。
type SubsystemPreflightInput struct {
	TenantID        string
	ApplicationCode string
	Environment     string
	Issuer          string
	PublicBaseURL   string
	UpstreamURL     string
	PathPrefix      string
	ClientType      string
}

// SubsystemProvisioner 校验并执行子系统部署生命周期。数据库登记由应用服务负责，部署器只处理
// 运行时文件、容器与网关；二者失败补偿必须通过持久部署状态协调，不能假设跨系统事务。
//
// Lifecycle (called via Unix-socket transport from the API process):
//   - Preflight: cheap environment checks before any state-changing operation.
//   - Provision: full atomic onboard (write .env.local + docker compose up + reload nginx).
//   - Update:    re-apply the integration to a subsystem that is already onboarded (rewrite
//     .env.local, rebuild containers, reload nginx). DB rows are not touched; caller must
//     have already updated them via PATCH /environments when BaseURL/UpstreamURL/PathPrefix
//     changed.
//   - Teardown:  full atomic offboard of the subsystem (docker compose down + remove
//     .env.local + remove gateway include + reload nginx). DB rows are not touched; the
//     HTTP layer is responsible for the subsequent DELETE on /environments and
//     /applications.
type SubsystemProvisioner interface {
	Preflight(context.Context, SubsystemPreflightInput) error
	Provision(context.Context, SubsystemProvisioningInput) error
	Update(context.Context, SubsystemProvisioningInput) error
	Teardown(context.Context, string, string, string) error
}
