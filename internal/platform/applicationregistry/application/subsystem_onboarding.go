package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
)

const (
	defaultSubsystemAccessTokenTTLSeconds  = 15 * 60
	defaultSubsystemRefreshTokenTTLSeconds = 30 * 24 * 60 * 60
	integratedCustomerApplicationCode      = "customer_and_opportunity"
	integratedCustomerPathPrefix           = "/customer-opportunity"
	integratedCustomerUpstreamURL          = "http://customer-api:8090"
	legacyCustomerPathPrefix               = "/customer_and_opportunity"
	legacyCustomerUpstreamURL              = "http://opportunity-api:8082"
	integratedPortalApplicationCode        = "customer_portal"
	integratedPortalPathPrefix             = "/customer-portal"
	integratedPortalUpstreamURL            = "http://portal-api:8091"
	integratedContractApplicationCode      = "contract_management"
	integratedProjectApplicationCode       = "project_management"
)

// defaultBackchannelLogoutPath 是平台与各子系统约定的 Back-Channel Logout 端点后缀
// （子系统接收端点：/<prefix>/auth/backchannel-logout）。接入时据此自动登记，
// 不再依赖人工调用管理 API（BIZ-XEVT2-07）。
const defaultBackchannelLogoutPath = "/auth/backchannel-logout"

// ServiceCredential* 常量枚举受管子系统按用途绑定的全部机器凭据用途，
// 同时是“凭据最小权限”的协议标识：部署 Agent 按用途把不同密钥写入对应子系统，
// 不能把其中任一机器客户端当成通用平台客户端复用。
const (
	// ServiceCredentialAuditIngest 是所有接入环境的基线能力，只能追加审计事件；
	// 它与浏览器登录、授权目录发布和子系统间业务调用使用不同客户端，
	// 避免任一密钥泄露后权限横向扩散。
	ServiceCredentialAuditIngest                        = "audit_ingest"
	ServiceCredentialNotificationIngest                 = "notification_ingest"
	ServiceCredentialExternalUserProvision              = "external_user_provision"
	ServiceCredentialApplicationRoleAssign              = "application_role_assign"
	ServiceCredentialApplicationRoleRevoke              = "application_role_revoke"
	ServiceCredentialPortalMappingProvision             = "portal_mapping_provision"
	ServiceCredentialPortalMappingDisable               = "portal_mapping_disable"
	ServiceCredentialPortalInviteVerify                 = "portal_invite_verify"
	ServiceCredentialOwnerDirectoryRead                 = "owner_directory_read"
	ServiceCredentialContractOpportunitySignedWrite     = "contract_opportunity_signed_write"
	ServiceCredentialContractSummaryRead                = "contract_summary_read"
	ServiceCredentialContractOpportunitySignedCountRead = "contract_opportunity_signed_count_read"
	ServiceCredentialCRMContractReferenceRead           = "crm_contract_reference_read"
	// ServiceCredentialCRMContractLinkWrite 允许合同系统把商机签约关联结果回写
	// CRM 的契约端点；CRM 机器入口固定要求 scope opportunity.signed.write，
	// 与合同机器入口共用同一最小权限语义但使用独立专用客户端
	// （BIZ-XEVT2-02）。静态 Bearer 不再被接受。
	ServiceCredentialCRMContractLinkWrite = "crm_contract_link_write"
	// ServiceCredentialCRMCustomerImportProvision / ServiceCredentialCRMOpportunityImportProvision
	// 是合同历史导入自动开立 CRM 客户/商机所用的两个专用用途：scope 必须分别与
	// CRM 内部导入通道的机器入口白名单（customer.import_provision /
	// opportunity.import_provision）完全一致，否则开启导入开立即 401。平台按
	// “一用途一 client 一 scope”授予，客户与商机凭据分离，且都不复用合同核验与
	// 签约回写凭据。
	ServiceCredentialCRMCustomerImportProvision         = "crm_customer_import_provision"
	ServiceCredentialCRMOpportunityImportProvision      = "crm_opportunity_import_provision"
	ServiceCredentialContractImportReportRead           = "contract_import_report_read"
	ServiceCredentialAnalysisContractImportReportStatus = "analysis_contract_import_report_status"
	ServiceCredentialContractDashboardRead              = "contract_dashboard_read"
	ServiceCredentialProjectDashboardRead               = "project_dashboard_read"
	ServiceCredentialContractApprovedRead               = "contract_approved_read"
	ServiceCredentialProjectContractImport              = "project_contract_import"
	ServiceCredentialReimbursementProjectReferenceRead  = "reimbursement_project_reference_read"
	ServiceCredentialProjectReimbursementCostWrite      = "project_reimbursement_cost_write"
	ServiceCredentialSettlementProjectReferenceRead     = "settlement_project_reference_read"
	ServiceCredentialSettlementContractIngest           = "settlement_contract_ingest"
	// ServiceCredentialCRMCreditPaymentIngest 是结算→CRM 回款/冲销信用事实写入的
	// 受控用途。scope 必须与 CRM 机器入口权限白名单（customer.credit.payment.ingest）
	// 完全一致，否则结算启用同步即 401（BIZ-XAPI-02 / BIZ-XWIRE-06）。
	ServiceCredentialCRMCreditPaymentIngest           = "crm_credit_payment_ingest"
	ServiceCredentialPortalReportFileRead             = "portal_report_file_read"
	ServiceCredentialPortalReportDownloadIdentityRead = "portal_report_download_identity_read"
	// ServiceCredentialFileGatewayWrite 是业务子系统上传并绑定自身文件的最小组合凭据；
	// 精确包含上传、绑定和下载三scope，仍受同应用资源绑定与业务ACL约束。
	ServiceCredentialFileGatewayWrite = "file_gateway_write"
)

// 上述用途常量同时是“凭据最小权限”的协议标识：部署 Agent 按用途把不同密钥写入
// 对应子系统，不能把其中任一机器客户端当成通用平台客户端复用。

// 接入阶段对 Back-Channel Logout 登记的决定。三种决定都必须可审计：登记成功是
// 默认路径，opt-out 与"端点校验失败被跳过"都会让该 RP 收不到登出通知。
const (
	// BackchannelLogoutDecisionRegistered 表示已自动登记 logout_uri。
	BackchannelLogoutDecisionRegistered = "REGISTERED"
	// BackchannelLogoutDecisionOptOut 表示调用方显式声明该 RP 不支持 Back-Channel Logout。
	BackchannelLogoutDecisionOptOut = "OPT_OUT"
	// BackchannelLogoutDecisionSkipped 表示自动登记被跳过（端点未通过协议校验，
	// 例如非回环明文 HTTP）。任务生成侧会以 NO_ENDPOINT 暴露该缺口。
	BackchannelLogoutDecisionSkipped = "SKIPPED_INVALID_ENDPOINT"
)

// ErrBackchannelLogoutAuditUnavailable 表示显式 opt-out 缺少审计写入端。
// opt-out 会让离职停用/中央登出无法通知该 RP，属安全相关决策，必须留痕；
// 没有审计通道时失败关闭，而不是静默放行。
var ErrBackchannelLogoutAuditUnavailable = errors.New("back-channel logout opt-out requires an audit sink")

// BackchannelLogoutAuditEvent 是接入阶段 Back-Channel Logout 决定的最小审计事实，
// 由组合根适配到平台审计服务，不携带任何凭据。
type BackchannelLogoutAuditEvent struct {
	TenantID        string
	OperatorID      string
	ApplicationCode string
	Environment     string
	OAuthClientID   string
	ClientID        string
	Decision        string
	LogoutURI       string
	Reason          string
	// Metadata 中恒含 missing_logout_uri 键：决定不是 REGISTERED 时该 RP 本应被
	// 通知却无登记端点，告警与检索据此聚合，不得退化为自由文本。
	Metadata map[string]any
}

// BackchannelLogoutAuditSink 由组合根适配到平台审计服务；opt-out 与登记跳过
// 必须经它留痕。
type BackchannelLogoutAuditSink interface {
	RecordBackchannelLogoutDecision(context.Context, BackchannelLogoutAuditEvent) error
}

// 接入是仅创建操作；同租户的应用环境已经存在时返回可识别冲突，调用方可以提示改走
// 更新或重试流程，同时不把内部 ID、数据库信息或凭据材料带入错误响应。
var ErrSubsystemOnboardingAlreadyExists = errors.New("subsystem onboarding environment already exists")

// 冲突对象只记录操作者已经知道的应用编码、环境编码及非敏感状态，供接口生成可操作提示；
// OAuth 客户端 ID 和密钥不会进入该对象，避免错误日志或 JSON 响应意外泄露凭据。
type SubsystemOnboardingConflict struct {
	ApplicationCode string
	Environment     string
	Status          string
}

// Error 返回只含应用编码、环境与非敏感状态的冲突摘要，可直接进入日志与 API 响应。
func (conflict *SubsystemOnboardingConflict) Error() string {
	return fmt.Sprintf("subsystem onboarding environment already exists: application=%s environment=%s status=%s", conflict.ApplicationCode, conflict.Environment, conflict.Status)
}

// 让传输层可用 errors.Is 识别接入冲突，而不依赖具体错误文本。
func (*SubsystemOnboardingConflict) Is(target error) bool {
	return target == ErrSubsystemOnboardingAlreadyExists
}

// 输入只接收管理员需要决定的公开信息；资源 ID、门户目标、OAuth 回调及凭据由服务端
// 统一推导，避免分别调用多个管理接口时形成环境错配或遗漏安全参数。
type SubsystemOnboardingInput struct {
	// AccessOperation 仅由持久向导执行器设置，绑定原子登记与部署代次。
	AccessOperation *SubsystemAccessOperation
	TenantID        string
	OperatorID      string
	// InitialAdminUserID 随部署状态持久化，首次部署失败后安全重试时
	// 仍能把管理员角色授予最初选定的人，而不是重试发起者。
	InitialAdminUserID string
	ApplicationCode    string
	ApplicationName    string
	Description        *string
	Environment        string
	PublicBaseURL      string
	UpstreamURL        string
	PathPrefix         string
	ClientType         string
	// IssuerAlias 是所选认证提供方，随环境持久化；后续部署/重试据此使用
	// 明确且可审计的选择，而不是从当前进程配置反推。
	IssuerAlias *string
	// AllowedServiceBindings 是该应用清单声明的可创建服务凭据用途（不含 audit_ingest 基线）。
	// 为空且清单未声明时回退到平台硬编码默认，保证既有行为不变。
	AllowedServiceBindings []string
	// BackchannelLogoutPath 是 Back-Channel Logout 端点相对公开基址的路径后缀。
	// nil 表示未声明：使用 defaultBackchannelLogoutPath 并自动登记
	// （BIZ-XEVT2-07：登记不再依赖人工调用管理 API）。
	// 显式空串表示该 RP 不支持 Back-Channel Logout，属于有意的 opt-out：不写登记行，
	// 但必须留下审计痕迹，不允许静默跳过。
	BackchannelLogoutPath *string
}

// 写模型汇总一次接入产生的控制面对象，仓储在一个数据库事务内持久化；明文密钥不进入
// 写模型，持久化边界只接收经过保护的 SecretWrite。
type SubsystemOnboardingWrite struct {
	AccessOperation    *SubsystemAccessOperation
	InitialAdminUserID string
	Application        ApplicationCreateInput
	ApplicationID      string
	Environment        EnvironmentCreateInput
	EnvironmentID      string
	LoginTarget        LoginTargetCreateInput
	LoginTargetID      string
	OAuthClient        OAuthClientCreateInput
	OAuthClientID      string
	OAuthClientSecret  *SecretWrite
	// BackchannelLogoutURI 与浏览器客户端在同一次数据库事务内登记；空表示显式
	// opt-out（由应用层审计），仓储不得自行补登记或改写协议校验。
	BackchannelLogoutURI string

	// 授权目录发布使用独立机器客户端，不能与浏览器 OIDC 回调或其他系统集成复用。
	CatalogPublisherOAuthClient       OAuthClientCreateInput
	CatalogPublisherOAuthClientID     string
	CatalogPublisherOAuthClientSecret *SecretWrite

	// 每个环境都会获得单 scope 的审计写入客户端；已知集成应用再按用途增加独立客户端：
	// CRM 增加负责人目录读取，客户门户增加六个身份及门户协作能力。每种用途使用独立密钥。
	ServiceClients []SubsystemServiceClientWrite
}

// SubsystemDirectoryRegistrationInput 是应用集成的非认证部分。刻意不含 OAuth
// 客户端类型、回调 URI 或凭据字段：这些由所选身份提供方（如 Keycloak）持有，
// 经其独立的集成工作流创建。
//
// 遗留的 SubsystemOnboardingInput 仍供已部署安装使用。新调用方应先登记目录，
// 再用 Keycloak 集成 API 创建/同步提供方客户端，最后下发运行时部署配置。
type SubsystemDirectoryRegistrationInput struct {
	TenantID        string
	OperatorID      string
	ApplicationCode string
	ApplicationName string
	Description     *string
	Environment     string
	PublicBaseURL   string
	UpstreamURL     string
	PathPrefix      string
	IssuerAlias     *string
}

// SubsystemDirectoryRegistrationWrite 是应用目录信息的原子持久化边界。
// OAuth 客户端、服务凭据与部署状态刻意不在其中，分别归各自的工作流管。
type SubsystemDirectoryRegistrationWrite struct {
	Application   ApplicationCreateInput
	ApplicationID string
	Environment   EnvironmentCreateInput
	EnvironmentID string
	LoginTarget   LoginTargetCreateInput
	LoginTargetID string
}

// SubsystemDirectoryRegistrationResult 可安全展示给管理员：
// 只含应用目录、环境与落地目标，不含凭据或运行时细节。
type SubsystemDirectoryRegistrationResult struct {
	Application Application
	Environment Environment
	LoginTarget LoginTargetManagementItem
	PublicURL   string
}

// SubsystemServiceClientWrite 是按用途创建的机器客户端写模型；
// 每种用途一把密钥，写入与 Secret 哈希同事务持久化。
type SubsystemServiceClientWrite struct {
	Purpose           string
	OAuthClient       OAuthClientCreateInput
	OAuthClientID     string
	OAuthClientSecret *SecretWrite
}

// SubsystemServiceCredential 是按用途交付的机器凭据：明文 Secret 仅存在于
// 一次性交付链路，消费方必须按 Purpose 写入对应配置项，不得交叉复用。
type SubsystemServiceCredential struct {
	Purpose         string
	OAuthClient     OAuthClientView
	PlaintextSecret string
}

// 结果中的明文密钥只在首次接入成功后交付一次，后续列表接口无法恢复；调用方不得记录
// 或再次持久化这些值，只能直接交给可信部署器完成运行时配置。
type SubsystemOnboardingResult struct {
	Application Application
	Environment Environment
	LoginTarget LoginTargetManagementItem
	OAuthClient OAuthClientView
	// 目录发布客户端对象用于审计追踪，明文则单独承载并只转交可信部署器。
	CatalogPublisherOAuthClient     OAuthClientView
	PlaintextSecret                 string
	CatalogPublisherPlaintextSecret string
	ServiceCredentials              []SubsystemServiceCredential
	RedirectURI                     string
	// BackchannelLogoutURI 是本次接入自动登记的 Back-Channel Logout 端点；
	// 空串表示显式 opt-out 或端点未通过校验，具体决定见 BackchannelLogoutDecision。
	BackchannelLogoutURI      string
	BackchannelLogoutDecision string
	PublicURL                 string
}

// 门户投影只包含已认证用户可见且可解析的跳转信息，不携带控制面配置或任何凭据。
type PortalApplication struct {
	ApplicationID string
	Code          string
	Name          string
	Description   *string
	EnvironmentID string
	Environment   string
	PathPrefix    *string
	TargetCode    string
	TargetURI     string
	PublicURL     string
	Allowed       bool
	Projection    PortalProjectionReadiness
}

// PortalProjectionReadiness 是当前主体对某个精确应用环境的用户级 Keycloak 投影门禁。
// 门户客户端必须使用 Ready/Allowed 判定，不能从"应用是否可见"反推就绪状态。
type PortalProjectionReadiness struct {
	Status     string
	Ready      bool
	NextAction string
}

// 仓储负责在单个数据库事务内写入接入聚合，并按租户读取有效门户登记。
type SubsystemOnboardingRepository interface {
	CreateSubsystem(context.Context, SubsystemOnboardingWrite, time.Time) (SubsystemOnboardingResult, error)
	CreateSubsystemDirectory(context.Context, SubsystemDirectoryRegistrationWrite, time.Time) (SubsystemDirectoryRegistrationResult, error)
	ListRegisteredApplicationCodes(context.Context, string) ([]string, error)
	ListPortalApplications(context.Context, string, string, string) ([]PortalApplication, error)
	ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error)
}

// ListRegisteredApplicationCodes 返回租户内活跃应用身份，不套用用户门户可见性，
// 也不替用户选择环境。发现属于管理盘点：已在任一环境接管的应用，不能仅因
// 当前操作者缺少其业务角色、或 Docker 上报了不同环境标签，就被当作新子系统再次提供。
func (service *SubsystemOnboardingService) ListRegisteredApplicationCodes(ctx context.Context, tenantID string) ([]string, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, ErrValidation
	}
	items, err := service.repository.ListRegisteredApplicationCodes(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		code := strings.ToLower(strings.TrimSpace(item))
		if code == "" {
			continue
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		result = append(result, code)
	}
	return result, nil
}

// 服务把应用、环境、门户入口、浏览器客户端及用途隔离的机器客户端编排成一次接入。
type SubsystemOnboardingService struct {
	repository                  SubsystemOnboardingRepository
	ids                         ManagementIdentifierGenerator
	clock                       Clock
	redirectURIValidationPolicy RedirectURIValidationPolicy
	// postLoginRedirectPolicy 判定接入时登记的公开基址与登录目标是否落在批准来源内；
	// 未注入时只做结构校验（装配层始终注入真实集合）。
	postLoginRedirectPolicy PostLoginRedirectPolicy
	// backchannelAudit 在模块组装后注入；显式 opt-out 没有它即失败关闭。
	backchannelAudit BackchannelLogoutAuditSink
}

// SubsystemOnboardingServiceOption 调整接入服务的可选行为，不改变既有构造调用。
type SubsystemOnboardingServiceOption func(*SubsystemOnboardingService)

// WithSubsystemOnboardingRedirectPolicy 注入登录后跳转的绝对地址来源集合，使接入
// 路径对环境公开基址与登录目标执行与解析路径相同的来源判定。
func WithSubsystemOnboardingRedirectPolicy(policy PostLoginRedirectPolicy) SubsystemOnboardingServiceOption {
	return func(service *SubsystemOnboardingService) { service.postLoginRedirectPolicy = policy }
}

// 构造时要求 ID、时钟和仓储齐备；回调 URI 策略允许由环境控制是否接受受限 HTTP。
func NewSubsystemOnboardingService(repository SubsystemOnboardingRepository, ids ManagementIdentifierGenerator, clock Clock, redirectURIValidationPolicy RedirectURIValidationPolicy, options ...SubsystemOnboardingServiceOption) (*SubsystemOnboardingService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("subsystem onboarding dependencies must not be nil")
	}
	service := &SubsystemOnboardingService{
		repository: repository, ids: ids, clock: clock, redirectURIValidationPolicy: redirectURIValidationPolicy,
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
}

// acceptsEnvironmentRegistration 在结构化环境校验之上追加公开基址来源判定：
// 公开基址与相对登录目标拼接后即浏览器最终跳转地址。
func (service *SubsystemOnboardingService) acceptsEnvironmentRegistration(input EnvironmentCreateInput) bool {
	return validEnvironmentCreate(input) && service.postLoginRedirectPolicy.ValidOptionalBaseURL(input.BaseURL)
}

// acceptsLoginTargetRegistration 拒绝接入时登记的未批准绝对目标。
func (service *SubsystemOnboardingService) acceptsLoginTargetRegistration(input LoginTargetCreateInput) bool {
	return validLoginTargetCreate(input) && service.postLoginRedirectPolicy.ValidTargetURI(input.TargetURI)
}

// SetBackchannelLogoutAuditSink 在模块组装完成后注入 Back-Channel Logout 决定的
// 审计写入端。显式 opt-out 属于安全相关决策，必须在写控制面之前可用。
func (service *SubsystemOnboardingService) SetBackchannelLogoutAuditSink(sink BackchannelLogoutAuditSink) {
	service.backchannelAudit = sink
}

// 预检只规范化并验证公开接入参数，不生成 ID、密钥或数据库记录，便于部署器在真正写入
// 控制面事务前先检查运行环境。
func ValidateSubsystemOnboardingInput(input SubsystemOnboardingInput) error {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return ErrValidation
	}
	return nil
}

// PreflightValidate 在 ValidateSubsystemOnboardingInput 之上增加数据库级冲突检查：
// 核验应用编码+环境组合可用、路径前缀不与既有登记冲突，供部署器在写入前先行拦截。
func (service *SubsystemOnboardingService) PreflightValidate(ctx context.Context, input SubsystemOnboardingInput) error {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return ErrValidation
	}
	// 检查应用+环境是否已存在。
	_, _, err := service.repository.ResolveApplicationEnvironment(ctx, input.TenantID, input.ApplicationCode, input.Environment)
	if err == nil {
		return ErrSubsystemOnboardingAlreadyExists
	}
	if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("preflight: check application environment: %w", err)
	}
	// 检查路径前缀是否与其他应用冲突。
	items, listErr := service.repository.ListPortalApplications(ctx, input.TenantID, "", "")
	if listErr != nil {
		return fmt.Errorf("preflight: list existing applications: %w", listErr)
	}
	for _, item := range items {
		if item.PathPrefix != nil && strings.EqualFold(strings.TrimSpace(*item.PathPrefix), input.PathPrefix) {
			return fmt.Errorf("preflight: path prefix %s is already used by application %s", input.PathPrefix, item.Code)
		}
	}
	return nil
}

// OnboardSubsystem 用途：原子登记控制面资源及可选任务代次，不代表外部部署已完成。
// 参数：ctx 为上下文，input 为接入目标、操作者及持久任务绑定。
// 返回值：返回登记资源和本次交付凭据；验证或数据库事务失败返回错误。
func (service *SubsystemOnboardingService) OnboardSubsystem(ctx context.Context, input SubsystemOnboardingInput) (SubsystemOnboardingResult, error) {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return SubsystemOnboardingResult{}, ErrValidation
	}

	publicURL := input.PublicBaseURL + input.PathPrefix + "/"
	redirectURI := input.PublicBaseURL + input.PathPrefix + "/auth/callback"
	// Back-Channel Logout 端点由公开基址与接入路径推导，与浏览器客户端同事务登记
	// （BIZ-XEVT2-07）。此前登记只能靠人工调用管理 API，漏登记时离职停用/中央登出
	// 对该 RP 完全没有投递任务。校验不放宽：只接受 HTTPS（本地回环明文仅在进程策略
	// 显式开启时接受），失败即"跳过并留痕"，不写入不可投递的登记行。
	logoutURI, logoutDecision := "", BackchannelLogoutDecisionOptOut
	if path := strings.TrimSpace(derefString(input.BackchannelLogoutPath)); path != "" {
		candidate := input.PublicBaseURL + input.PathPrefix + path
		if ValidateBackchannelLogoutURI(candidate, service.redirectURIValidationPolicy.AllowInsecureHTTP) == nil {
			logoutURI, logoutDecision = candidate, BackchannelLogoutDecisionRegistered
		} else {
			logoutDecision = BackchannelLogoutDecisionSkipped
		}
	}
	// opt-out 会让该 RP 永远收不到登出通知，必须在写入任何控制面之前确认审计可用。
	if logoutDecision == BackchannelLogoutDecisionOptOut && service.backchannelAudit == nil {
		return SubsystemOnboardingResult{}, ErrBackchannelLogoutAuditUnavailable
	}
	clientID := input.ApplicationCode + "-" + input.Environment + "-web"
	// 浏览器公开客户端不能安全保存 secret，因此只能使用 PKCE；机密客户端则由后续的
	// 一次性交付链路把 secret 交给部署 Agent，明文不会进入仓储写模型。
	tokenAuthMethod := "client_secret_basic"
	if input.ClientType == "public" {
		tokenAuthMethod = "none"
	}

	applicationInput := normalizeApplicationCreate(ApplicationCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, Code: input.ApplicationCode,
		Name: input.ApplicationName, ApplicationType: "web", HomepageURL: stringPointer(publicURL),
		Description: input.Description, Status: "ACTIVE",
	})
	if !validApplicationCreate(applicationInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	applicationID, err := service.newID(now, "application")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	environmentID, err := service.newID(now, "application environment")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	loginTargetID, err := service.newID(now, "application login target")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	oauthClientID, err := service.newID(now, "OAuth client")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	catalogPublisherOAuthClientID, err := service.newID(now, "authorization catalog publisher OAuth client")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	environmentInput := normalizeEnvironmentCreate(EnvironmentCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		Environment: input.Environment, BaseURL: stringPointer(input.PublicBaseURL),
		UpstreamURL: stringPointer(input.UpstreamURL), PathPrefix: stringPointer(input.PathPrefix), IssuerAlias: input.IssuerAlias, Status: "ACTIVE",
	})
	if !service.acceptsEnvironmentRegistration(environmentInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}
	loginTargetInput := normalizeLoginTargetCreate(LoginTargetCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, TargetCode: "home", Name: input.ApplicationName + "首页",
		TargetURI: input.PathPrefix + "/", Status: "ACTIVE",
	})
	if !service.acceptsLoginTargetRegistration(loginTargetInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}
	oauthClientInput, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, ClientID: clientID, ClientName: input.ApplicationName + " Web",
		ClientType: input.ClientType, TokenAuthMethod: tokenAuthMethod,
		AccessTokenTTLSeconds:  defaultSubsystemAccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: defaultSubsystemRefreshTokenTTLSeconds,
		RequirePKCE:            true, GrantTypes: []string{"authorization_code", "refresh_token"},
		Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirectURI},
	}, service.redirectURIValidationPolicy)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	var secret *SecretWrite
	plaintextSecret := ""
	if tokenAuthMethod == "client_secret_basic" {
		write, plaintext, secretErr := newOAuthClientSecretWrite(service.ids, now, nil)
		if secretErr != nil {
			return SubsystemOnboardingResult{}, secretErr
		}
		secret, plaintextSecret = &write, plaintext
	}

	// 每个子系统都使用独立的目录发布客户端，避免 Web 登录客户端一旦泄露后同时获得
	// 修改授权目录的控制面能力。
	// 固定为 client_credentials/client_secret_basic 且只有一个 scope，防止目录发布凭据
	// 被拿去执行浏览器授权或其他机器接口。
	catalogPublisherClientInput, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, ClientID: input.ApplicationCode + "-" + input.Environment + "-catalog-publisher",
		ClientName: input.ApplicationName + " Authorization Catalog Publisher",
		ClientType: "service", TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds:  defaultSubsystemAccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: 0, RequirePKCE: false,
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"authorization.catalog.sync"},
	}, service.redirectURIValidationPolicy)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	catalogPublisherSecretWrite, catalogPublisherPlaintextSecret, err := newOAuthClientSecretWrite(service.ids, now, nil)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	serviceClients, serviceCredentials, err := service.buildIntegratedServiceClients(input, applicationID, environmentID, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	result, err := service.repository.CreateSubsystem(ctx, SubsystemOnboardingWrite{
		AccessOperation:    input.AccessOperation,
		InitialAdminUserID: input.InitialAdminUserID,
		Application:        applicationInput, ApplicationID: applicationID,
		Environment: environmentInput, EnvironmentID: environmentID,
		LoginTarget: loginTargetInput, LoginTargetID: loginTargetID,
		OAuthClient: oauthClientInput, OAuthClientID: oauthClientID, OAuthClientSecret: secret,
		BackchannelLogoutURI:              logoutURI,
		CatalogPublisherOAuthClient:       catalogPublisherClientInput,
		CatalogPublisherOAuthClientID:     catalogPublisherOAuthClientID,
		CatalogPublisherOAuthClientSecret: &catalogPublisherSecretWrite,
		ServiceClients:                    serviceClients,
	}, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	// 只有数据库事务成功后才把对应明文装配回结果。仓储始终只看到哈希；调用方必须把
	// 这些明文直接交给可信部署器，任何重试都不能依赖从数据库“找回”旧密钥。
	result.PlaintextSecret = plaintextSecret
	result.CatalogPublisherPlaintextSecret = catalogPublisherPlaintextSecret
	for index := range result.ServiceCredentials {
		if secret, ok := serviceCredentials[result.ServiceCredentials[index].Purpose]; ok {
			result.ServiceCredentials[index].PlaintextSecret = secret
		}
	}
	result.RedirectURI = redirectURI
	result.PublicURL = publicURL
	result.BackchannelLogoutURI = logoutURI
	result.BackchannelLogoutDecision = logoutDecision
	// 登记以外的两种决定都意味着"本应通知却无端点"：必须留痕并可在审计里检索，
	// 不允许因为接入成功就静默。审计写入失败即返回错误，避免做出无法追责的决定。
	if logoutDecision != BackchannelLogoutDecisionRegistered && service.backchannelAudit != nil {
		event := BackchannelLogoutAuditEvent{
			TenantID: input.TenantID, OperatorID: input.OperatorID,
			ApplicationCode: result.Application.Code, Environment: result.Environment.Environment,
			OAuthClientID: result.OAuthClient.ID, ClientID: result.OAuthClient.ClientID,
			Decision: logoutDecision, LogoutURI: logoutURI,
			Reason:   logoutDecisionReason(logoutDecision),
			Metadata: missingLogoutURIAuditMetadata(result.OAuthClient.ID, logoutDecision),
		}
		if err := service.backchannelAudit.RecordBackchannelLogoutDecision(ctx, event); err != nil {
			return SubsystemOnboardingResult{}, fmt.Errorf("record back-channel logout decision: %w", err)
		}
	}
	return result, nil
}

// logoutDecisionReason 为审计事件提供稳定的原因码，便于告警规则区分"有意 opt-out"
// 与"端点校验失败"，两者的处置动作不同。
func logoutDecisionReason(decision string) string {
	switch decision {
	case BackchannelLogoutDecisionOptOut:
		return "opt_out"
	case BackchannelLogoutDecisionSkipped:
		return "invalid_logout_endpoint"
	}
	return "registered"
}

// missingLogoutURIAuditMetadata 复用协议层固定的 missing_logout_uri 键，保证接入期
// 审计与投递期 NO_ENDPOINT 原因码可被同一条告警规则聚合。
func missingLogoutURIAuditMetadata(oauthClientID, decision string) map[string]any {
	metadata := backchannel.MissingLogoutURIMetadata(oauthClientID)
	metadata["reason"] = logoutDecisionReason(decision)
	return metadata
}

// RegisterSubsystemDirectory 只登记业务应用、其环境与一个已批准的登录后落地目标；
// 尤其绝不创建平台浏览器 OAuth 客户端。这使应用目录/授权管理与 Keycloak 客户端
// 生命周期解耦，同时保留上方的一体式遗留接入路径供既有部署使用。
func (service *SubsystemOnboardingService) RegisterSubsystemDirectory(ctx context.Context, input SubsystemDirectoryRegistrationInput) (SubsystemDirectoryRegistrationResult, error) {
	input = normalizeSubsystemDirectoryRegistrationInput(input)
	if !validSubsystemDirectoryRegistrationInput(input) {
		return SubsystemDirectoryRegistrationResult{}, ErrValidation
	}

	publicURL := input.PublicBaseURL + input.PathPrefix + "/"
	now := service.clock.Now().UTC()
	applicationID, err := service.newID(now, "application")
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}
	environmentID, err := service.newID(now, "application environment")
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}
	loginTargetID, err := service.newID(now, "application login target")
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}

	applicationInput := normalizeApplicationCreate(ApplicationCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, Code: input.ApplicationCode,
		Name: input.ApplicationName, ApplicationType: "web", HomepageURL: stringPointer(publicURL),
		Description: input.Description, Status: "ACTIVE",
	})
	environmentInput := normalizeEnvironmentCreate(EnvironmentCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		Environment: input.Environment, BaseURL: stringPointer(input.PublicBaseURL),
		UpstreamURL: stringPointer(input.UpstreamURL), PathPrefix: stringPointer(input.PathPrefix),
		IssuerAlias: input.IssuerAlias, Status: "ACTIVE",
	})
	loginTargetInput := normalizeLoginTargetCreate(LoginTargetCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, TargetCode: "home", Name: input.ApplicationName + "首页",
		TargetURI: input.PathPrefix + "/", Status: "ACTIVE",
	})
	if !validApplicationCreate(applicationInput) || !service.acceptsEnvironmentRegistration(environmentInput) || !service.acceptsLoginTargetRegistration(loginTargetInput) {
		return SubsystemDirectoryRegistrationResult{}, ErrValidation
	}

	result, err := service.repository.CreateSubsystemDirectory(ctx, SubsystemDirectoryRegistrationWrite{
		Application: applicationInput, ApplicationID: applicationID,
		Environment: environmentInput, EnvironmentID: environmentID,
		LoginTarget: loginTargetInput, LoginTargetID: loginTargetID,
	}, now)
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}
	result.PublicURL = publicURL
	return result, nil
}

// integratedServicePurposeDefinition 描述一个服务用途客户端的固定元数据（scope/suffix/name）。
// 这是平台级的通用注册表，不属于任何具体子系统；清单声明 allowed_service_bindings 后，
// 接入服务按注册表创建对应客户端。
type integratedServicePurposeDefinition struct {
	purpose string
	suffix  string
	name    string
	scope   string
}

var integratedServicePurposeRegistry = map[string]integratedServicePurposeDefinition{
	ServiceCredentialReimbursementProjectReferenceRead:  {ServiceCredentialReimbursementProjectReferenceRead, "project-reference", "Reimbursement Project Reference Reader", "reimbursement.project_reference.read"},
	ServiceCredentialProjectReimbursementCostWrite:      {ServiceCredentialProjectReimbursementCostWrite, "project-cost-writer", "Reimbursement Project Cost Writer", "project.reimbursement_cost.write"},
	ServiceCredentialContractImportReportRead:           {ServiceCredentialContractImportReportRead, "contract-import-report-read", "Historical Contract Report Source Reader", "contract.import.report.read"},
	ServiceCredentialAnalysisContractImportReportStatus: {ServiceCredentialAnalysisContractImportReportStatus, "analysis-import-report-status", "Historical Contract Report Status Reader", "analysis.contract-import.report-status"},
	ServiceCredentialSettlementProjectReferenceRead:     {ServiceCredentialSettlementProjectReferenceRead, "project-reference", "Settlement Project Reference Reader", "settlement.project_reference.read"},
	ServiceCredentialAuditIngest:                        {ServiceCredentialAuditIngest, "audit-publisher", "Audit Publisher", "audit.ingest"},
	ServiceCredentialNotificationIngest:                 {ServiceCredentialNotificationIngest, "notification-publisher", "Notification Publisher", "notification.ingest"},
	ServiceCredentialOwnerDirectoryRead:                 {ServiceCredentialOwnerDirectoryRead, "owner-directory", "Owner Directory Reader", "owner_directory.read"},
	ServiceCredentialExternalUserProvision:              {ServiceCredentialExternalUserProvision, "external-user-provision", "External User Provisioner", "external_user.provision"},
	ServiceCredentialApplicationRoleAssign:              {ServiceCredentialApplicationRoleAssign, "role-assign", "Application Role Assigner", "application_role.assign"},
	ServiceCredentialApplicationRoleRevoke:              {ServiceCredentialApplicationRoleRevoke, "role-revoke", "Application Role Revoker", "application_role.revoke"},
	ServiceCredentialPortalMappingProvision:             {ServiceCredentialPortalMappingProvision, "portal-mapping-provision", "Portal Identity Mapping Provisioner", "portal.identity_mapping.provision"},
	ServiceCredentialPortalMappingDisable:               {ServiceCredentialPortalMappingDisable, "portal-mapping-disable", "Portal Identity Mapping Disabler", "portal.identity_mapping.disable"},
	ServiceCredentialPortalInviteVerify:                 {ServiceCredentialPortalInviteVerify, "portal-invite-verify", "Portal Invite Verifier", "portal.invite.verify"},
	ServiceCredentialContractOpportunitySignedWrite:     {ServiceCredentialContractOpportunitySignedWrite, "opportunity-intake", "Opportunity Signed Intake", "opportunity.signed.write"},
	ServiceCredentialContractSummaryRead:                {ServiceCredentialContractSummaryRead, "contract-summary", "Contract Summary Reader", "contract.summary.read"},
	ServiceCredentialContractOpportunitySignedCountRead: {ServiceCredentialContractOpportunitySignedCountRead, "contract-signed-count", "Contract Opportunity Signed Count Reader", "contract.opportunity_signed_count.read"},
	ServiceCredentialCRMContractReferenceRead:           {ServiceCredentialCRMContractReferenceRead, "crm-contract-reference", "CRM Contract Reference Reader", "customer.contract_reference.read"},
	ServiceCredentialCRMContractLinkWrite:               {ServiceCredentialCRMContractLinkWrite, "crm-contract-link", "CRM Contract Link Writer", "opportunity.signed.write"},
	ServiceCredentialCRMCustomerImportProvision:         {ServiceCredentialCRMCustomerImportProvision, "crm-customer-import-provision", "CRM Customer Import Provisioner", "customer.import_provision"},
	ServiceCredentialCRMOpportunityImportProvision:      {ServiceCredentialCRMOpportunityImportProvision, "crm-opportunity-import-provision", "CRM Opportunity Import Provisioner", "opportunity.import_provision"},
	ServiceCredentialContractDashboardRead:              {ServiceCredentialContractDashboardRead, "contract-dashboard", "Contract Dashboard Reader", "dashboard.contract.read"},
	ServiceCredentialProjectDashboardRead:               {ServiceCredentialProjectDashboardRead, "project-dashboard", "Project Dashboard Reader", "dashboard.project.read"},
	ServiceCredentialContractApprovedRead:               {ServiceCredentialContractApprovedRead, "contract-approved-reader", "Approved Contract Reader", "contract.approved.internal.read"},
	ServiceCredentialProjectContractImport:              {ServiceCredentialProjectContractImport, "project-integration", "Project Contract Importer", "project.contract.import"},
	ServiceCredentialSettlementContractIngest:           {ServiceCredentialSettlementContractIngest, "settlement-integration", "Settlement Contract Ingester", "settlement.contract.ingest"},
	ServiceCredentialCRMCreditPaymentIngest:             {ServiceCredentialCRMCreditPaymentIngest, "crm-credit-ingest", "CRM Credit Payment Ingester", "customer.credit.payment.ingest"},
	ServiceCredentialPortalReportFileRead:               {ServiceCredentialPortalReportFileRead, "portal-report-source", "Portal Report Source Reader", "platform:file:portal_report"},
	ServiceCredentialPortalReportDownloadIdentityRead:   {ServiceCredentialPortalReportDownloadIdentityRead, "portal-report-identity", "Portal Report Identity Reader", "portal.report.download_identity.read"},
	ServiceCredentialFileGatewayWrite:                   {ServiceCredentialFileGatewayWrite, "file-gateway-writer", "File Gateway Writer", "platform:file:upload"},
}

// hardcodedIntegratedServicePurposes 返回未声明清单用途时的应用默认集成能力。
// 参数：applicationCode 为目标应用编码。
// 返回值：默认用途，不含所有应用必有的审计基线。
func hardcodedIntegratedServicePurposes(applicationCode string) []string {
	switch strings.TrimSpace(applicationCode) {
	case "reimbursement":
		return []string{ServiceCredentialReimbursementProjectReferenceRead, ServiceCredentialProjectReimbursementCostWrite, ServiceCredentialOwnerDirectoryRead, ServiceCredentialNotificationIngest, ServiceCredentialFileGatewayWrite}
	case integratedCustomerApplicationCode:
		// 合同核验与签约回写是 CRM 的默认跨系统能力：本地/首次接入也必须
		// 用 CRM 自己的 client 身份（customer_and_opportunity-<env>-*）创建，
		// 不能借用合同应用凭据（否则合同 Ed25519 验签的 caller 绑定会 401）。
		return []string{ServiceCredentialOwnerDirectoryRead, ServiceCredentialNotificationIngest, ServiceCredentialContractOpportunitySignedCountRead, ServiceCredentialContractSummaryRead, ServiceCredentialContractOpportunitySignedWrite}
	case integratedPortalApplicationCode:
		return []string{
			ServiceCredentialExternalUserProvision,
			ServiceCredentialApplicationRoleAssign,
			ServiceCredentialApplicationRoleRevoke,
			ServiceCredentialPortalMappingProvision,
			ServiceCredentialPortalMappingDisable,
			ServiceCredentialPortalInviteVerify,
			ServiceCredentialPortalReportFileRead,
			ServiceCredentialPortalReportDownloadIdentityRead,
		}
	case integratedContractApplicationCode:
		// 历史导入开立客户/商机是合同的默认集成能力（由 CONTRACT_IMPORT_CRM_PROVISION_ENABLED
		// 控制是否启用）：首次接入即创建专用客户端，避免开关打开时才发现平台无该用途。
		return []string{ServiceCredentialContractOpportunitySignedWrite, ServiceCredentialContractSummaryRead, ServiceCredentialCRMContractReferenceRead, ServiceCredentialCRMContractLinkWrite, ServiceCredentialCRMCustomerImportProvision, ServiceCredentialCRMOpportunityImportProvision, ServiceCredentialProjectContractImport, ServiceCredentialOwnerDirectoryRead, ServiceCredentialNotificationIngest, ServiceCredentialSettlementContractIngest}
	case "settlement", "settlement_and_invoicing":
		// 信用同步是结算的回款/冲销事实回流能力：默认创建用途客户端但由结算
		// SETTLEMENT_CRM_CREDIT_SYNC_ENABLED 控制是否启用，避免启用时才发现平台无用途。
		return []string{ServiceCredentialSettlementProjectReferenceRead, ServiceCredentialCRMCreditPaymentIngest, ServiceCredentialOwnerDirectoryRead, ServiceCredentialNotificationIngest}
	case integratedProjectApplicationCode:
		// 项目子系统在服务项操作台按人员选择团队负责人/项目经理/工程师，需要只读的
		// 负责人目录权限；不授予通知写入等其它集成能力。
		return []string{ServiceCredentialOwnerDirectoryRead, ServiceCredentialNotificationIngest, ServiceCredentialContractApprovedRead}
	}
	return nil
}

// buildIntegratedServiceClients 按受控用途注册表创建用途隔离的机器客户端。
// 参数：input为接入清单，applicationID/environmentID为目标，now为统一时间。
// 返回值：客户端写入与一次性密钥；未知用途或生成失败返回错误。
func (service *SubsystemOnboardingService) buildIntegratedServiceClients(input SubsystemOnboardingInput, applicationID, environmentID string, now time.Time) ([]SubsystemServiceClientWrite, map[string]string, error) {
	// 集中审计写入是所有接入环境的基线，而不是特定应用的可选集成；每个环境都使用
	// 自己的单 scope 客户端，平台可据此绑定来源应用和环境。
	purposes := []string{ServiceCredentialAuditIngest}
	if input.AllowedServiceBindings != nil {
		// 清单驱动（B3）：按清单声明的用途创建；审计基线恒在。
		for _, purpose := range input.AllowedServiceBindings {
			purposes = append(purposes, strings.TrimSpace(purpose))
		}
	} else {
		// 缺省回退：平台硬编码默认，保证既有行为不变。
		purposes = append(purposes, hardcodedIntegratedServicePurposes(input.ApplicationCode)...)
	}
	definitions := make([]integratedServicePurposeDefinition, 0, len(purposes))
	seen := make(map[string]struct{}, len(purposes))
	for _, purpose := range purposes {
		if purpose == "" {
			continue
		}
		if _, duplicate := seen[purpose]; duplicate {
			continue
		}
		definition, known := integratedServicePurposeRegistry[purpose]
		if !known {
			return nil, nil, fmt.Errorf("subsystem service purpose %q is not registered", purpose)
		}
		seen[purpose] = struct{}{}
		definitions = append(definitions, definition)
	}
	writes := make([]SubsystemServiceClientWrite, 0, len(definitions))
	secrets := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		scopes := []string{definition.scope}
		if definition.purpose == ServiceCredentialFileGatewayWrite {
			scopes = append(scopes, "platform:file:bind", "platform:file:download")
		}
		client, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
			TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
			EnvironmentID: environmentID,
			ClientID:      input.ApplicationCode + "-" + input.Environment + "-" + definition.suffix,
			ClientName:    input.ApplicationName + " " + definition.name,
			ClientType:    "service", TokenAuthMethod: "client_secret_basic",
			AccessTokenTTLSeconds: defaultSubsystemAccessTokenTTLSeconds, RequirePKCE: false,
			GrantTypes: []string{"client_credentials"}, Scopes: scopes,
		}, service.redirectURIValidationPolicy)
		if err != nil {
			return nil, nil, err
		}
		clientID, err := service.newID(now, definition.purpose+" OAuth client")
		if err != nil {
			return nil, nil, err
		}
		secretWrite, plaintext, err := newOAuthClientSecretWrite(service.ids, now, nil)
		if err != nil {
			return nil, nil, err
		}
		writes = append(writes, SubsystemServiceClientWrite{
			Purpose: definition.purpose, OAuthClient: client, OAuthClientID: clientID,
			OAuthClientSecret: &secretWrite,
		})
		secrets[definition.purpose] = plaintext
	}
	return writes, secrets, nil
}

// ResolveApplicationEnvironment 读取已登记的应用/环境对，不套用门户可见性或
// 部署就绪过滤；控制面同步流程必须能借它修复失败环境，因此不得加门禁。
func (service *SubsystemOnboardingService) ResolveApplicationEnvironment(ctx context.Context, tenantID, applicationCode, environment string) (string, string, error) {
	tenantID, applicationCode, environment = strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))
	if tenantID == "" || applicationCode == "" || !validEnvironmentCode(environment) {
		return "", "", ErrValidation
	}
	return service.repository.ResolveApplicationEnvironment(ctx, tenantID, applicationCode, environment)
}

// ResolveApplicationEnvironmentRedirectURI 返回控制面登记的规范 OAuth 回调地址。
// 返回值仅由租户、应用和环境范围内的持久化网关字段推导；字段缺失时失败关闭。
func (service *SubsystemOnboardingService) ResolveApplicationEnvironmentRedirectURI(ctx context.Context, tenantID, applicationID, environmentID string) (string, error) {
	resolver, ok := service.repository.(interface {
		ResolveApplicationEnvironmentGateway(context.Context, string, string, string) (string, string, error)
	})
	if !ok {
		return "", ErrValidation
	}
	baseURL, pathPrefix, err := resolver.ResolveApplicationEnvironmentGateway(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID))
	if err != nil {
		return "", err
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	pathPrefix = strings.TrimRight(strings.TrimSpace(pathPrefix), "/")
	if baseURL == "" || pathPrefix == "" {
		return "", ErrValidation
	}
	return baseURL + pathPrefix + "/auth/callback", nil
}

// 门户列表只返回当前租户、当前用户可见且能安全解析的有效应用；未指定环境时由仓储按
// 固定优先级选择，避免同一登记在不同请求中随机切换环境。
func (service *SubsystemOnboardingService) ListPortalApplications(ctx context.Context, tenantID, userID, environment string) ([]PortalApplication, error) {
	tenantID = strings.TrimSpace(tenantID)
	userID = strings.TrimSpace(userID)
	environment = strings.ToLower(strings.TrimSpace(environment))
	if tenantID == "" || userID == "" || (environment != "" && !validEnvironmentCode(environment)) {
		return nil, ErrValidation
	}
	items, err := service.repository.ListPortalApplications(ctx, tenantID, userID, environment)
	if err != nil {
		return nil, err
	}
	result := make([]PortalApplication, 0, len(items))
	for _, item := range items {
		// 仓储返回的是登记数据，但门户最终跳转前仍重新解析并丢弃异常目标；单条脏数据
		// 不应导致整个门户不可用，更不能把未验证字符串直接返回给浏览器。
		publicURL, resolveErr := resolvePortalTarget(item.PublicURL, item.TargetURI)
		if resolveErr != nil {
			continue
		}
		item.PublicURL = publicURL
		result = append(result, item)
	}
	return result, nil
}

func (service *SubsystemOnboardingService) newID(now time.Time, resource string) (string, error) {
	identifier, err := service.ids.New(now)
	if err != nil {
		return "", fmt.Errorf("generate %s ID: %w", resource, err)
	}
	return identifier, nil
}

func normalizeSubsystemOnboardingInput(input SubsystemOnboardingInput) SubsystemOnboardingInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.InitialAdminUserID = strings.TrimSpace(input.InitialAdminUserID)
	if input.InitialAdminUserID == "" {
		input.InitialAdminUserID = input.OperatorID
	}
	input.ApplicationCode = strings.ToLower(strings.TrimSpace(input.ApplicationCode))
	input.ApplicationName = strings.TrimSpace(input.ApplicationName)
	input.Description = normalizeOptional(input.Description)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	if input.Environment == "" {
		input.Environment = "prod"
	}
	input.PublicBaseURL = strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/")
	input.UpstreamURL = strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/")
	input.PathPrefix = strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/")
	if input.PathPrefix == "" && input.ApplicationCode != "" {
		input.PathPrefix = "/" + input.ApplicationCode
	}
	// 本地工作区把 customer_and_opportunity 纳入统一 frontend/Compose 拓扑；历史部署可能只迁移了
	// upstream 或 path，因此分别兼容已知旧值，避免一次接入因旧配置组合不同而失败。
	if input.ApplicationCode == integratedCustomerApplicationCode {
		if input.PathPrefix == legacyCustomerPathPrefix {
			input.PathPrefix = integratedCustomerPathPrefix
		}
		if input.UpstreamURL == legacyCustomerUpstreamURL {
			input.UpstreamURL = integratedCustomerUpstreamURL
		}
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		if input.PathPrefix == "/customer_portal" {
			input.PathPrefix = integratedPortalPathPrefix
		}
		if input.UpstreamURL == "http://customer-portal-api:8091" {
			input.UpstreamURL = integratedPortalUpstreamURL
		}
	}
	input.ClientType = strings.ToLower(strings.TrimSpace(input.ClientType))
	if input.ClientType == "" {
		input.ClientType = "confidential"
	}
	input.BackchannelLogoutPath = normalizeBackchannelLogoutPath(input.BackchannelLogoutPath)
	return input
}

// normalizeBackchannelLogoutPath 归一化自动登记端点路径：nil 取默认端点；显式空串
// 保留为空（opt-out）；其余去空白、补前导 "/"、去掉尾部 "/"。
func normalizeBackchannelLogoutPath(value *string) *string {
	if value == nil {
		path := defaultBackchannelLogoutPath
		return &path
	}
	path := strings.TrimSpace(*value)
	if path == "" {
		empty := ""
		return &empty
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	return &path
}

func normalizeSubsystemDirectoryRegistrationInput(input SubsystemDirectoryRegistrationInput) SubsystemDirectoryRegistrationInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationCode = strings.ToLower(strings.TrimSpace(input.ApplicationCode))
	input.ApplicationName = strings.TrimSpace(input.ApplicationName)
	input.Description = normalizeOptional(input.Description)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	if input.Environment == "" {
		input.Environment = "prod"
	}
	input.PublicBaseURL = strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/")
	input.UpstreamURL = strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/")
	input.PathPrefix = strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/")
	if input.PathPrefix == "" && input.ApplicationCode != "" {
		input.PathPrefix = "/" + input.ApplicationCode
	}
	if input.ApplicationCode == integratedCustomerApplicationCode {
		if input.PathPrefix == legacyCustomerPathPrefix {
			input.PathPrefix = integratedCustomerPathPrefix
		}
		if input.UpstreamURL == legacyCustomerUpstreamURL {
			input.UpstreamURL = integratedCustomerUpstreamURL
		}
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		if input.PathPrefix == "/customer_portal" {
			input.PathPrefix = integratedPortalPathPrefix
		}
		if input.UpstreamURL == "http://customer-portal-api:8091" {
			input.UpstreamURL = integratedPortalUpstreamURL
		}
	}
	if input.IssuerAlias != nil {
		alias := strings.ToLower(strings.TrimSpace(*input.IssuerAlias))
		if alias == "" || alias == IssuerAliasPlatform || alias == "basic_platform" {
			input.IssuerAlias = nil
		} else {
			input.IssuerAlias = &alias
		}
	}
	return input
}

func validSubsystemOnboardingInput(input SubsystemOnboardingInput) bool {
	baseURL, upstreamURL, pathPrefix := input.PublicBaseURL, input.UpstreamURL, input.PathPrefix
	return len(input.TenantID) == 26 && validIdentifier(input.TenantID) &&
		len(input.OperatorID) == 26 && validIdentifier(input.OperatorID) &&
		len(input.InitialAdminUserID) == 26 && validIdentifier(input.InitialAdminUserID) && validCode(input.ApplicationCode, 64) &&
		validManagementText(input.ApplicationName, 128, false) && validEnvironmentCode(input.Environment) &&
		validOptionalBaseURL(&baseURL) && validOptionalUpstreamURL(&upstreamURL) &&
		validOptionalPathPrefix(&pathPrefix) && validGatewayTripleConsistent(&baseURL, &upstreamURL, &pathPrefix) &&
		oneOf(input.ClientType, "public", "confidential") &&
		validBackchannelLogoutPath(input.BackchannelLogoutPath)
}

// validBackchannelLogoutPath 只接受空（显式 opt-out）或与门户路径同一保守口径的相对路径，
// 避免把 query/fragment/目录穿越拼进自动登记的端点。
func validBackchannelLogoutPath(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	return validPortalPath(*value, 128)
}

func validSubsystemDirectoryRegistrationInput(input SubsystemDirectoryRegistrationInput) bool {
	baseURL, upstreamURL, pathPrefix := input.PublicBaseURL, input.UpstreamURL, input.PathPrefix
	return len(input.TenantID) == 26 && validIdentifier(input.TenantID) &&
		len(input.OperatorID) == 26 && validIdentifier(input.OperatorID) && validCode(input.ApplicationCode, 64) &&
		validManagementText(input.ApplicationName, 128, false) && validEnvironmentCode(input.Environment) &&
		validOptionalBaseURL(&baseURL) && validOptionalUpstreamURL(&upstreamURL) &&
		validOptionalPathPrefix(&pathPrefix) && validGatewayTripleConsistent(&baseURL, &upstreamURL, &pathPrefix) &&
		(input.IssuerAlias == nil || *input.IssuerAlias == IssuerAliasKeycloak)
}

func resolvePortalTarget(baseURL, targetURI string) (string, error) {
	targetURI = strings.TrimSpace(targetURI)
	parsed, err := url.Parse(targetURI)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		if !validRedirectURI(targetURI, RedirectURIValidationPolicy{}) {
			return "", ErrValidation
		}
		return targetURI, nil
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || !strings.HasPrefix(targetURI, "/") {
		return "", ErrValidation
	}
	return baseURL + targetURI, nil
}

func stringPointer(value string) *string { return &value }

// derefString 把可选字符串安全解引用；nil 视为空串，供"未声明即取默认"的归一化使用。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
