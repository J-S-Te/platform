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

// ErrSubsystemDeploymentTransition 拒绝不符合状态机规则的生命周期转移。仓储层在写入前
// 校验当前状态是否是目标状态的合法前驱，防止并发操作或误用把状态表写坏。
var ErrSubsystemDeploymentTransition = errors.New("subsystem deployment status transition not allowed")

// subsystemDeploymentTransitions 定义每个目标状态允许的前驱状态。同状态重复转移视为幂等
// 成功，不在此表内。
var subsystemDeploymentTransitions = map[string][]string{
	SubsystemDeploymentStatusProvisioning: {
		SubsystemDeploymentStatusReady, SubsystemDeploymentStatusFailed,
		SubsystemDeploymentStatusOffboarded, SubsystemDeploymentStatusUnmanaged,
	},
	SubsystemDeploymentStatusUpdating: {
		SubsystemDeploymentStatusReady, SubsystemDeploymentStatusFailed,
		SubsystemDeploymentStatusDraining, SubsystemDeploymentStatusOffboarded,
		SubsystemDeploymentStatusUnmanaged,
	},
	SubsystemDeploymentStatusVerifying: {
		SubsystemDeploymentStatusProvisioning, SubsystemDeploymentStatusUpdating,
		// 看护任务需要能把僵死的 VERIFYING 收口为失败；卡在校验态的记录没有任何
		// 活动编排可以继续推进它，只允许转回部署态会让看护永远无法恢复。
		SubsystemDeploymentStatusFailed,
	},
	SubsystemDeploymentStatusReady: {
		SubsystemDeploymentStatusProvisioning, SubsystemDeploymentStatusUpdating,
		SubsystemDeploymentStatusVerifying, SubsystemDeploymentStatusDraining,
	},
	SubsystemDeploymentStatusFailed: {
		SubsystemDeploymentStatusProvisioning, SubsystemDeploymentStatusUpdating,
		SubsystemDeploymentStatusVerifying, SubsystemDeploymentStatusDraining,
		// 看护任务把中断的丢弃占用态收口回失败，使运维可以重新发起丢弃或重试部署。
		SubsystemDeploymentStatusDiscarding,
	},
	SubsystemDeploymentStatusDiscarding: {
		SubsystemDeploymentStatusFailed,
	},
	SubsystemDeploymentStatusDraining: {
		SubsystemDeploymentStatusReady, SubsystemDeploymentStatusFailed,
		SubsystemDeploymentStatusUnmanaged,
	},
	SubsystemDeploymentStatusOffboarded: {
		SubsystemDeploymentStatusDraining, SubsystemDeploymentStatusFailed,
	},
}

// SubsystemDeploymentTransitionAllowed 判定 from→to 是否为合法生命周期迁移；
// 未登记的目标状态一律拒绝，调用方不得绕过该门禁直接改库。
func SubsystemDeploymentTransitionAllowed(from, to string) bool {
	allowed, ok := subsystemDeploymentTransitions[to]
	if !ok {
		return false
	}
	for _, status := range allowed {
		if status == from {
			return true
		}
	}
	return false
}

const (
	// SubsystemDeploymentStatusProvisioning 表示首次部署进行中。
	SubsystemDeploymentStatusProvisioning = "PROVISIONING"
	// SubsystemDeploymentStatusUpdating 表示受控更新进行中。
	SubsystemDeploymentStatusUpdating = "UPDATING"
	// SubsystemDeploymentStatusVerifying 表示部署命令已完成、就绪核验尚未通过；
	// 只有核验通过才允许进入 READY。
	SubsystemDeploymentStatusVerifying = "VERIFYING"
	// SubsystemDeploymentStatusReady 表示运行就绪且通过核验。
	SubsystemDeploymentStatusReady = "READY"
	// SubsystemDeploymentStatusFailed 表示部署失败，等待重试、接管或丢弃；
	// 值沿用历史字符串 PROVISION_FAILED 以兼容既有数据。
	SubsystemDeploymentStatusFailed = "PROVISION_FAILED"
	// SubsystemDeploymentStatusDraining 表示下线进行中：Teardown 已开始但尚未完成。
	SubsystemDeploymentStatusDraining = "DRAINING"
	// SubsystemDeploymentStatusOffboarded 表示下线完成，运行时资源与网关配置均已移除。
	SubsystemDeploymentStatusOffboarded = "OFFBOARDED"
	// SubsystemDeploymentStatusDiscarding 是"失败部署正在被丢弃"的占用态。
	//
	// 丢弃必须先原子占用再销毁基础设施：否则「丢弃」（先 Teardown 再按 FAILED 删记录）与
	// 「重试」（把 FAILED 推进为 UPDATING 并重新部署）并发时，删除会被正确地拒绝，但
	// Teardown 已经拆掉了重试刚建起来的环境。占用态让重试在丢弃进行中无法接管。
	SubsystemDeploymentStatusDiscarding = "DISCARDING"
	// SubsystemDeploymentStatusUnmanaged 表示应用/环境已在控制面登记，
	// 但还没有任何受管部署尝试创建生命周期记录。刻意不等于 READY：
	// 运行中的服务仍须走正常流程核验或接管。
	SubsystemDeploymentStatusUnmanaged = "UNMANAGED"
	// SubsystemServiceStatus* 是发现实例的探活状态：DISCOVERED 表示容器已发现但
	// 探活尚未通过；HEALTHY 表示探活通过、可参与网关路由选择；UNAVAILABLE 表示
	// 探活失败或实例已被下线，不得参与路由。
	SubsystemServiceStatusDiscovered  = "DISCOVERED"
	SubsystemServiceStatusHealthy     = "HEALTHY"
	SubsystemServiceStatusUnavailable = "UNAVAILABLE"
)

// SubsystemHealthTarget 是一次运行时探活的目标投影：只含应用编码、环境与内部健康检查
// URL，凭据和宿主机路径不进入该结构。
type SubsystemHealthTarget struct {
	ApplicationCode string
	Environment     string
	HealthURL       string
}

// SubsystemHealthTargetResolver 是可选能力接口：由具体仓储实现，健康看板通过类型断言
// 使用，无需在装配层显式注入。
type SubsystemHealthTargetResolver interface {
	ResolveSubsystemHealthTargets(ctx context.Context, tenantID string) ([]SubsystemHealthTarget, error)
}

// SubsystemServiceInstance 是发现结果在控制面的轻量投影，只含路由元数据；凭据和宿主机路径
// 永远不跨越发现边界，避免只读盘点意外升级为敏感信息读取。
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

// UpstreamURL 在请求时从发现元数据生成网关可用地址，不落盘为服务端配置文件；协议、主机和
// 端口均先校验，调用方不能借此把任意字符串直接写入代理配置。
func (instance SubsystemServiceInstance) UpstreamURL() (string, error) {
	if instance.Protocol != "http" && instance.Protocol != "https" && instance.Protocol != "tcp" {
		return "", errors.New("unsupported service protocol")
	}
	if instance.InternalHost == "" || instance.InternalPort == 0 || instance.InternalPort > 65535 {
		return "", errors.New("invalid service address")
	}
	return fmt.Sprintf("%s://%s", instance.Protocol, net.JoinHostPort(instance.InternalHost, fmt.Sprintf("%d", instance.InternalPort))), nil
}

// SubsystemServiceRegistry 持久化发现到的服务实例；部署 Agent 将在下一步迁移中
// 经既有本地传输使用该契约，实现必须按租户边界过滤读取。
type SubsystemServiceRegistry interface {
	UpsertSubsystemServiceInstance(context.Context, SubsystemServiceInstance) error
	ListSubsystemServiceInstances(context.Context, string, string, string) ([]SubsystemServiceInstance, error)
	MarkUnavailableSubsystemServiceInstances(context.Context, time.Time, time.Time) error
}

// SubsystemDiscoveryCandidate 是子系统在被平台登记之前经 Docker 标签发布的
// 非敏感自述。刻意不含租户、数据库、文件系统、镜像或凭据数据：
// 发现只是盘点操作，接入仍然是经过审计的控制面写入。
type SubsystemDiscoveryCandidate struct {
	ApplicationCode       string `json:"application_code"`
	ApplicationName       string `json:"application_name"`
	Environment           string `json:"environment"`
	ServiceName           string `json:"service_name"`
	ServiceRole           string `json:"service_role"`
	Protocol              string `json:"protocol"`
	InternalHost          string `json:"internal_host"`
	InternalPort          uint   `json:"internal_port"`
	HealthEndpoint        string `json:"health_endpoint,omitempty"`
	OIDCCallbackPath      string `json:"oidc_callback_path"`
	OIDCCallbackSupported bool   `json:"oidc_callback_supported"`
	Version               string `json:"version,omitempty"`
	Status                string `json:"status"`
}

// SubsystemCandidateDiscoverer 与部署器故意分离：容器只能提供待审核候选，不能触发命令或写入；
// 只有管理员通过正常审计接入后，候选才会进入受控部署生命周期。
type SubsystemCandidateDiscoverer interface {
	DiscoverSubsystemCandidates(context.Context) ([]SubsystemDiscoveryCandidate, error)
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
	DesiredManifestChecksum string
	AppliedManifestChecksum string
	ManifestDriftStatus     string
	ManifestLastAppliedAt   *time.Time
	ManifestLastVerifiedAt  *time.Time
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
	ClaimSubsystemDeployment(context.Context, string, string, string, string, time.Time) (uint64, error)
	CompleteSubsystemDeployment(context.Context, string, string, string, uint64, string, string, string, string, time.Time) error
	RecoverStaleSubsystemDeployment(context.Context, SubsystemDeploymentState, time.Time, time.Time) (bool, error)
	TransitionSubsystemDeployment(context.Context, string, string, string, string, string, string, string, time.Time) error
	MarkSubsystemInitialAccessAssigned(context.Context, string, string, string, string, time.Time) error
	GetSubsystemDeploymentContext(context.Context, string, string, string) (SubsystemDeploymentState, error)
	GetSubsystemDeploymentState(context.Context, string, string, string) (SubsystemDeploymentState, error)
	// ClaimFailedSubsystemDeploymentForDiscard 原子占用一条失败部署记录用于丢弃：
	// 仅当记录仍处于 PROVISION_FAILED 且 generation 与读取时一致时推进为 DISCARDING。
	// 返回 false 表示状态已被并发推进（通常是重试已接管），调用方不得执行 Teardown。
	ClaimFailedSubsystemDeploymentForDiscard(context.Context, string, string, string, uint64, time.Time) (bool, error)
	// DiscardClaimedSubsystemDeployment 删除已占用的失败记录，释放 code+environment 以便
	// 重新接入。只匹配 DISCARDING + 占用时拿到的 generation。
	DiscardClaimedSubsystemDeployment(context.Context, string, string, string, uint64) error
}

// SubsystemDeploymentStaleAfter 是在途部署状态被判定为“编排已死”的阈值。HTTP 层的
// stale 恢复与后台看护任务必须共用同一数值，避免两边对同一行状态得出不同结论。
const SubsystemDeploymentStaleAfter = 20 * time.Minute

// SubsystemDeploymentInFlightLister 供后台看护任务枚举仍在途的部署状态；是可选能力，
// 旧的内存/测试存储不必实现。
type SubsystemDeploymentInFlightLister interface {
	ListInFlightSubsystemDeployments(ctx context.Context) ([]SubsystemDeploymentState, error)
}

// SubsystemManifestStateStore 持久化 API/Agent 清单握手结果；旧部署适配器可不实现。
type SubsystemManifestStateStore interface {
	SetSubsystemDesiredManifest(context.Context, string, string, string, string, time.Time) error
	MarkSubsystemManifestApplied(context.Context, string, string, string, string, time.Time) error
}

// SubsystemProvisioningCapabilities 描述暴露给管理控制台的安全部署边界。
// 刻意不含宿主机路径、租户绑定、镜像引用、凭据或任意命令/服务名。
// 目标只能从服务端已评审的部署清单加载；浏览器可以选择其一，但不能创建新目标。
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
	// ReleaseIntegrity 是 Agent 已接线的发布完整性能力名；空表示未启用。
	// 它只声明能力，不表示某个候选已经验签通过。
	ReleaseIntegrity string `json:"release_integrity,omitempty"`
}

// SubsystemProvisioningTarget 是一份已评审部署清单的非敏感投影。PublicBaseURL 由
// 操作者录入，因为浏览器侧 origin 可能随主机、端口或反向代理部署而变化；
// 部署 Agent 仍会校验已评审的路径/上游，以及由该 origin 推导出的精确回调地址。
type SubsystemProvisioningTarget struct {
	ApplicationCode string
	ApplicationName string
	Description     string
	Environment     string
	UpstreamURL     string
	PathPrefix      string
	ClientType      string
	// ManifestChecksum 是规范化审核清单的 SHA-256；API 与部署 Agent 不一致时必须在写文件前拒绝。
	ManifestChecksum string
	// InitialAdminRoles 是清单声明的接入初始管理员角色；nil 表示未声明（由平台默认决定）。
	InitialAdminRoles []string
	// AllowedServiceBindings 是清单声明的服务用途白名单（不含 audit_ingest 基线）；nil 表示未声明。
	AllowedServiceBindings []string
}

// SubsystemAccessDeploymentReceipt 仅携带部署完成事实的绑定信息，不包含运行凭据。
type SubsystemAccessDeploymentReceipt struct {
	TenantID         string
	OperationID      string
	ApplicationCode  string
	Environment      string
	Generation       uint64
	ManifestChecksum string
}

// SubsystemProvisioningInput 是一次性交付给子系统运行时的配置封套。ClientSecret 仅允许经过
// 进程内接口与受限 Unix socket 到达部署 Agent，不得写日志、返回浏览器或出现在命令行参数中。
type SubsystemProvisioningInput struct {
	AccessReceipt    *SubsystemAccessDeploymentReceipt `json:"access_receipt,omitempty"`
	TenantID         string
	ApplicationID    string
	EnvironmentID    string
	ApplicationCode  string
	Environment      string
	ManifestChecksum string
	// ReleaseDigest 是 API 本次要求 Agent 绑定的已安装 release manifest 摘要；
	// 空表示由 Agent 本机配置决定（兼容旧客户端）。
	ReleaseDigest string `json:"release_digest,omitempty"`
	Issuer        string
	// AuthenticationProvider 由控制面从持久化环境绑定解析，绝不来自 Agent 配置
	// 或仅存于浏览器的提示。
	AuthenticationProvider string
	ClientID               string
	ClientSecret           string
	// AuthenticationRuntimeUpdate 只由专用的切换/回滚端点设置；
	// 普通重建不得覆盖 Issuer 或浏览器凭据。
	AuthenticationRuntimeUpdate bool
	// InitialRuntimeProvisioning 由控制面仅为"首次发布从未完成"的已评审
	// 接管/重试推导；它不是浏览器生命周期请求字段。
	InitialRuntimeProvisioning bool
	// CatalogPublisherClientID/Secret 是授权目录同步专用的独立服务凭据，
	// 与应用主凭据分离以收敛泄露影响面。
	CatalogPublisherClientID     string
	CatalogPublisherClientSecret string
	// ServiceCredentials 是接入时创建的一次性、按用途绑定的凭据：仅交付给隔离
	// 部署 Agent 并写入 mode-0600 运行时环境文件；浏览器响应与运维日志绝不可见。
	ServiceCredentials []SubsystemServiceCredential
	// RelatedServiceClients 仅含已核验元数据，绝不含其他应用的明文 Secret
	// 或浏览器随意提供的客户端 ID。
	RelatedServiceClients []RelatedSubsystemServiceClient
	// 组件凭据与应用级 env 文件相互隔离，避免共享文件放大泄露范围。
	RuntimeLicenseCredentials []RuntimeLicenseCredential
	RuntimeLicenseSettings    RuntimeLicenseSettings
	RedirectURI               string
	PublicURL                 string
	PathPrefix                string
	UpstreamURL               string
}

// RelatedSubsystemServiceClient 是关联应用服务客户端的只读元数据投影，
// 仅在跨应用集成时随一次性交付封套下发，绝不携带对端明文 Secret。
type RelatedSubsystemServiceClient struct {
	ApplicationCode string
	ApplicationID   string
	Environment     string
	EnvironmentID   string
	Purpose         string
	Client          OAuthClientView
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
	TenantID         string
	ApplicationCode  string
	Environment      string
	ManifestChecksum string
	Issuer           string
	PublicBaseURL    string
	UpstreamURL      string
	PathPrefix       string
	ClientType       string
}

// SubsystemProvisioner 校验并执行运行时生命周期。数据库登记由应用服务负责，部署器只处理运行时
// 文件、容器和网关；两者不是跨系统事务，失败补偿必须依靠持久部署状态、幂等重试和明确下线步骤。
// Preflight 只做无副作用检查；Provision 写配置并启动；Update 复用已保存密钥重建；Teardown
// 停止并清理运行时资源。任何阶段失败都不能伪装成数据库回滚，调用方需按状态继续重试或补偿。
type SubsystemProvisioner interface {
	Preflight(context.Context, SubsystemPreflightInput) error
	Provision(context.Context, SubsystemProvisioningInput) error
	Update(context.Context, SubsystemProvisioningInput) error
	Teardown(context.Context, string, string, string) error
}

// RegisteredServiceCredential 即使在受控更新刻意沿用已交付 Secret 时，也返回
// 持久化的客户端元数据。消费方必须核验目标并要求其既有运行时 Secret；
// 对全新安装而言，本方法返回的空 Secret 不构成可用凭据。
func (input SubsystemProvisioningInput) RegisteredServiceCredential(purpose string) (SubsystemServiceCredential, bool) {
	var found SubsystemServiceCredential
	count := 0
	for _, credential := range input.ServiceCredentials {
		if credential.Purpose == purpose && credential.OAuthClient.ClientID != "" {
			found = credential
			count++
		}
	}
	return found, count == 1
}
