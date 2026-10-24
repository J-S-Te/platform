package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	safeUpstreamURLPattern = regexp.MustCompile(`^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~-]+)(:([0-9]{1,5}))?(/[A-Za-z0-9._~!%+/@-]*)?$`)

	// ErrNotFound 表示租户范围内的应用登记资源不存在；跨租户访问同样折叠为该错误。
	ErrNotFound = errors.New("application registry resource not found")
	// ErrConflict 表示登记变更违反唯一性或生命周期约束（如重复编码、非法状态迁移）。
	ErrConflict = errors.New("application registry resource conflict")
	// ErrVersionConflict 表示变更基于过期的聚合版本，调用方必须重读后重试。
	ErrVersionConflict = errors.New("application registry version conflict")
	// ErrValidation 表示管理请求存在无法安全落库的输入问题，调用方可自行修正。
	ErrValidation = errors.New("invalid application registry input")
	// ErrEnvironmentDeletionBlocked 表示环境仍有必须保留的配置或审计证据，删除被拒绝。
	ErrEnvironmentDeletionBlocked = errors.New("application environment deletion blocked by retained records")
	// ErrEnvironmentNotOffboarded 在部署控制面为该精确租户/应用/环境记录运行时拆除成功之前，
	// 阻止不可逆清理。
	ErrEnvironmentNotOffboarded = errors.New("application environment is not offboarded")
	// ErrEnvironmentRetentionApprovalInvalid 要求先完成限定到该租户与应用的平台审计保留清理任务，
	// 才允许删除其审计回执记录。
	ErrEnvironmentRetentionApprovalInvalid = errors.New("application environment retention approval is invalid")
)

const (
	defaultManagementPageSize = 20
	maxManagementPageSize     = 100
	maxMetadataBytes          = 64 << 10
	builtInApplicationCode    = "platform"

	// IssuerAliasPlatform/IssuerAliasKeycloak 是应用环境可选的全部认证提供方。
	// 刻意沿用既有 issuer_alias 字段承载提供方选择：再设一个 issuer 类别字段
	// 会在切换或回滚期间造成两个互相竞争的事实来源。
	IssuerAliasPlatform = "platform"
	IssuerAliasKeycloak = "keycloak"
)

// IdentifierGenerator 为应用与环境提供可排序标识符；实现须时间有序且租户内唯一。
type IdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// PageRequest 承载应用管理列表查询的受边界约束的过滤参数。
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
	Status   string
}

// PageResult 是管理查询返回的租户安全列表结果，可跨管理接口复用。
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// Application 是租户范围内的业务系统登记。刻意不含任何 OAuth 客户端、授权类型、
// 回调或凭据数据，目录视图与凭据面完全隔离。
type Application struct {
	ID              string
	TenantID        string
	Code            string
	Name            string
	ApplicationType string
	OwnerOrgID      *string
	OwnerUserID     *string
	HomepageURL     *string
	Description     *string
	Status          string
	Version         uint64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Environment 是应用下的一个部署边界。Metadata 只允许非机密的 JSON 对象数据；
// OAuth 凭据与私钥刻意不在本结构建模。
//
// 网关字段把对外地址与内部上游分开，使平台可作为单入口反向代理：
//   - BaseURL:     外部调用方（浏览器、OAuth 客户端）看到的公开基址；
//     LoginTarget.TargetURI 为相对路径时以它为前缀解析。
//   - UpstreamURL: 已登记子系统的内部地址，仅门户主机可达（如 http://127.0.0.1:8081），
//     用于渲染 nginx upstream 映射。
//   - PathPrefix:  子系统在门户下的路径前缀（如 /contract）；设置后由门户网关
//     剥离并转发到 UpstreamURL。
type Environment struct {
	ID            string
	TenantID      string
	ApplicationID string
	Environment   string
	BaseURL       *string
	UpstreamURL   *string
	PathPrefix    *string
	IssuerAlias   *string
	Metadata      json.RawMessage
	Status        string
	Version       uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ApplicationCreateInput 包含新应用登记的全部可写字段；稳定编码一经创建不可改。
type ApplicationCreateInput struct {
	TenantID        string
	OperatorID      string
	Code            string
	Name            string
	ApplicationType string
	OwnerOrgID      *string
	OwnerUserID     *string
	HomepageURL     *string
	Description     *string
	Status          string
}

// ApplicationUpdateInput 以乐观锁更新应用。稳定编码不可写：权限、审计溯源与
// 配置命名空间都以它为键，变更会静默断开既有引用。
type ApplicationUpdateInput struct {
	TenantID        string
	OperatorID      string
	ApplicationID   string
	Name            string
	ApplicationType string
	OwnerOrgID      *string
	OwnerUserID     *string
	HomepageURL     *string
	Description     *string
	Status          string
	Version         uint64
}

// ApplicationDeleteInput 在调用方确认稳定编码后退役应用登记。刻意做成逻辑删除：
// 关联环境、OAuth 客户端、登录目标与审计历史保留可追溯。
type ApplicationDeleteInput struct {
	TenantID         string
	OperatorID       string
	ApplicationID    string
	ConfirmationCode string
	Version          uint64
}

// EnvironmentCreateInput 包含单个部署环境的全部可写字段。
type EnvironmentCreateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	Environment   string
	BaseURL       *string
	UpstreamURL   *string
	PathPrefix    *string
	IssuerAlias   *string
	Metadata      json.RawMessage
	Status        string
}

// EnvironmentUpdateInput 以乐观锁更新环境；版本冲突须重读，不得覆盖并发修改。
type EnvironmentUpdateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	EnvironmentID string
	BaseURL       *string
	UpstreamURL   *string
	PathPrefix    *string
	IssuerAlias   *string
	Metadata      json.RawMessage
	Status        string
	Version       uint64
}

// EnvironmentDeleteInput 在显式范围确认后，物理移除一个非开发部署环境及其派生的
// 登录目标/OAuth 集成记录；存在保留证据时仓储会拒绝。
type EnvironmentDeleteInput struct {
	TenantID         string
	OperatorID       string
	ApplicationID    string
	EnvironmentID    string
	ConfirmationCode string
	Version          uint64
}

// EnvironmentPurgeInput 是不可逆、需评审的清理操作。刻意与 DeleteEnvironment 分离：
// 调用方必须显式确认子系统已下线，且保留配置与审计证据将被销毁。
type EnvironmentPurgeInput struct {
	TenantID            string
	OperatorID          string
	ApplicationID       string
	EnvironmentID       string
	ConfirmationCode    string
	RetentionApprovalID string
	RetentionConfirmed  bool
	OffboardedConfirmed bool
	Version             uint64
}

// ManagementRepository 负责应用与环境管理的租户范围持久化。
// 不暴露 OAuth 客户端或凭据变更操作，目录管理与凭据生命周期分层隔离。
type ManagementRepository interface {
	ListApplications(context.Context, string, PageRequest) (PageResult[Application], error)
	CreateApplication(context.Context, ApplicationCreateInput, string, time.Time) (Application, error)
	GetApplication(context.Context, string, string) (Application, error)
	UpdateApplication(context.Context, ApplicationUpdateInput, time.Time) (Application, error)

	ListEnvironments(context.Context, string, string, PageRequest) (PageResult[Environment], error)
	CreateEnvironment(context.Context, EnvironmentCreateInput, string, time.Time) (Environment, error)
	GetEnvironment(context.Context, string, string, string) (Environment, error)
	UpdateEnvironment(context.Context, EnvironmentUpdateInput, time.Time) (Environment, error)
	DeleteEnvironment(context.Context, EnvironmentDeleteInput) (Environment, error)
}

// ManagementService 串联受控且租户隔离的应用与环境变更；跨租户读取一律按 not found 处理。
type ManagementService struct {
	repository ManagementRepository
	ids        IdentifierGenerator
	clock      Clock
}

// NewManagementService 构造应用/环境管理用例；任一依赖缺失即拒绝构造。
func NewManagementService(repository ManagementRepository, ids IdentifierGenerator, clock Clock) (*ManagementService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("application registry management dependencies must not be nil")
	}
	return &ManagementService{repository: repository, ids: ids, clock: clock}, nil
}

// ListApplications 返回认证租户内可见的应用，分页参数有硬上限防刷。
func (service *ManagementService) ListApplications(ctx context.Context, tenantID string, query PageRequest) (PageResult[Application], error) {
	tenantID = strings.TrimSpace(tenantID)
	query = normalizePageRequest(query)
	if tenantID == "" || !validApplicationStatusFilter(query.Status) {
		return PageResult[Application]{}, ErrValidation
	}
	return service.repository.ListApplications(ctx, tenantID, query)
}

// CreateApplication 以稳定编码登记租户范围内的业务系统；编码冲突返回 ErrConflict。
func (service *ManagementService) CreateApplication(ctx context.Context, input ApplicationCreateInput) (Application, error) {
	input = normalizeApplicationCreate(input)
	if !validApplicationCreate(input) {
		return Application{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	applicationID, err := service.ids.New(now)
	if err != nil {
		return Application{}, fmt.Errorf("generate application ID: %w", err)
	}
	return service.repository.CreateApplication(ctx, input, applicationID, now)
}

// GetApplication 返回租户范围内的单个应用。
func (service *ManagementService) GetApplication(ctx context.Context, tenantID, applicationID string) (Application, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	if tenantID == "" || applicationID == "" {
		return Application{}, ErrValidation
	}
	return service.repository.GetApplication(ctx, tenantID, applicationID)
}

// UpdateApplication 修改可变的登记信息，稳定编码保持不变。
func (service *ManagementService) UpdateApplication(ctx context.Context, input ApplicationUpdateInput) (Application, error) {
	input = normalizeApplicationUpdate(input)
	if !validApplicationUpdate(input) {
		return Application{}, ErrValidation
	}
	// PATCH 与 DELETE 必须共享同一生命周期保护：内置平台应用是登录与授权控制面的根节点，
	// 只有普通应用更新权限的人把 status 改成 SUSPENDED/RETIRED 会让门户入口、登录目标和
	// 机器客户端因父应用不再 ACTIVE 而失效，因此这里按稳定应用编码拒绝非 ACTIVE 变更。
	current, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Application{}, err
	}
	if current.Code == builtInApplicationCode && input.Status != "ACTIVE" {
		return Application{}, ErrConflict
	}
	return service.repository.UpdateApplication(ctx, input, service.clock.Now().UTC())
}

// DeleteApplication 把应用登记迁移到 RETIRED 完成逻辑删除。刻意避免物理删除：
// 环境、OAuth 客户端、登录目标与审计记录仍可能引用稳定的应用身份。
func (service *ManagementService) DeleteApplication(ctx context.Context, input ApplicationDeleteInput) (Application, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.ConfirmationCode = strings.TrimSpace(input.ConfirmationCode)
	if input.TenantID == "" || input.OperatorID == "" || input.ApplicationID == "" || input.ConfirmationCode == "" || input.Version == 0 {
		return Application{}, ErrValidation
	}

	current, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Application{}, err
	}
	if current.Code == builtInApplicationCode {
		// 平台自身是登录与授权控制面根节点，禁止通过普通应用管理接口退役；否则会同时
		// 破坏 OIDC 发行方、门户入口以及后续审计所依赖的父资源。
		return Application{}, ErrConflict
	}
	if input.ConfirmationCode != current.Code {
		return Application{}, ErrValidation
	}
	if input.Version != current.Version {
		return Application{}, ErrVersionConflict
	}
	if current.Status == "RETIRED" {
		return current, nil
	}

	return service.repository.UpdateApplication(ctx, ApplicationUpdateInput{
		TenantID:        current.TenantID,
		OperatorID:      input.OperatorID,
		ApplicationID:   current.ID,
		Name:            current.Name,
		ApplicationType: current.ApplicationType,
		OwnerOrgID:      current.OwnerOrgID,
		OwnerUserID:     current.OwnerUserID,
		HomepageURL:     current.HomepageURL,
		Description:     current.Description,
		Status:          "RETIRED",
		Version:         input.Version,
	}, service.clock.Now().UTC())
}

// ListEnvironments 返回某租户范围应用下登记的全部环境。
func (service *ManagementService) ListEnvironments(ctx context.Context, tenantID, applicationID string, query PageRequest) (PageResult[Environment], error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	query = normalizePageRequest(query)
	if tenantID == "" || applicationID == "" || !validEnvironmentStatusFilter(query.Status) {
		return PageResult[Environment]{}, ErrValidation
	}
	if _, err := service.repository.GetApplication(ctx, tenantID, applicationID); err != nil {
		return PageResult[Environment]{}, err
	}
	return service.repository.ListEnvironments(ctx, tenantID, applicationID, query)
}

// CreateEnvironment 登记一个隔离的部署环境。已退役/暂停应用不得新增环境；
// DRAFT 应用允许在上线前预置。
func (service *ManagementService) CreateEnvironment(ctx context.Context, input EnvironmentCreateInput) (Environment, error) {
	input = normalizeEnvironmentCreate(input)
	if !validEnvironmentCreate(input) {
		return Environment{}, ErrValidation
	}

	parent, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Environment{}, err
	}
	if parent.Status != "DRAFT" && parent.Status != "ACTIVE" {
		return Environment{}, ErrConflict
	}

	now := service.clock.Now().UTC()
	environmentID, err := service.ids.New(now)
	if err != nil {
		return Environment{}, fmt.Errorf("generate application environment ID: %w", err)
	}
	return service.repository.CreateEnvironment(ctx, input, environmentID, now)
}

// GetEnvironment 返回租户/应用范围内的单个环境。
func (service *ManagementService) GetEnvironment(ctx context.Context, tenantID, applicationID, environmentID string) (Environment, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	environmentID = strings.TrimSpace(environmentID)
	if tenantID == "" || applicationID == "" || environmentID == "" {
		return Environment{}, ErrValidation
	}
	return service.repository.GetEnvironment(ctx, tenantID, applicationID, environmentID)
}

// UpdateEnvironment 以乐观锁变更部署元数据与生命周期状态。
func (service *ManagementService) UpdateEnvironment(ctx context.Context, input EnvironmentUpdateInput) (Environment, error) {
	input = normalizeEnvironmentUpdate(input)
	if !validEnvironmentUpdate(input) {
		return Environment{}, ErrValidation
	}
	return service.repository.UpdateEnvironment(ctx, input, service.clock.Now().UTC())
}

// DeleteEnvironment 移除一个非开发环境，保留父应用与其他环境不受影响。
// 仓储只原子移除派生集成记录；存在配置或审计证据时拒绝删除。
func (service *ManagementService) DeleteEnvironment(ctx context.Context, input EnvironmentDeleteInput) (Environment, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.ConfirmationCode = strings.TrimSpace(input.ConfirmationCode)
	if input.TenantID == "" || input.OperatorID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || input.ConfirmationCode == "" || input.Version == 0 {
		return Environment{}, ErrValidation
	}

	registeredApplication, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Environment{}, err
	}
	environment, err := service.repository.GetEnvironment(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
	if err != nil {
		return Environment{}, err
	}
	if registeredApplication.Code == builtInApplicationCode || environment.Environment == "dev" {
		return Environment{}, ErrConflict
	}
	if input.ConfirmationCode != registeredApplication.Code+"/"+environment.Environment {
		return Environment{}, ErrValidation
	}
	if input.Version != environment.Version {
		return Environment{}, ErrVersionConflict
	}
	return service.repository.DeleteEnvironment(ctx, input)
}

// PurgeEnvironment 仅在严格且限定范围的确认之后才调用可选的破坏性仓储操作；
// 未实现 purge 的既有仓储保持安全（按不支持处理）。
func (service *ManagementService) PurgeEnvironment(ctx context.Context, input EnvironmentPurgeInput) (Environment, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.ConfirmationCode = strings.TrimSpace(input.ConfirmationCode)
	input.RetentionApprovalID = strings.TrimSpace(input.RetentionApprovalID)
	if input.TenantID == "" || input.OperatorID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || input.Version == 0 || input.RetentionApprovalID == "" || !input.RetentionConfirmed || !input.OffboardedConfirmed {
		return Environment{}, ErrValidation
	}
	app, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Environment{}, err
	}
	if app.Status != "RETIRED" {
		return Environment{}, ErrConflict
	}
	environment, err := service.repository.GetEnvironment(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
	if err != nil {
		return Environment{}, err
	}
	expected := "PURGE/" + app.Code + "/" + environment.Environment
	if input.ConfirmationCode != expected || input.Version != environment.Version || app.Code == builtInApplicationCode || environment.Environment == "dev" {
		return Environment{}, ErrValidation
	}
	purger, ok := service.repository.(interface {
		PurgeEnvironment(context.Context, EnvironmentPurgeInput) (Environment, error)
	})
	if !ok {
		return Environment{}, errors.New("environment purge is unavailable")
	}
	return purger.PurgeEnvironment(ctx, input)
}

func normalizePageRequest(query PageRequest) PageRequest {
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.Status = strings.ToUpper(strings.TrimSpace(query.Status))
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = defaultManagementPageSize
	}
	if query.PageSize > maxManagementPageSize {
		query.PageSize = maxManagementPageSize
	}
	return query
}

func normalizeApplicationCreate(input ApplicationCreateInput) ApplicationCreateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.ApplicationType = strings.ToLower(strings.TrimSpace(input.ApplicationType))
	input.OwnerOrgID = normalizeOptional(input.OwnerOrgID)
	input.OwnerUserID = normalizeOptional(input.OwnerUserID)
	input.HomepageURL = normalizeOptional(input.HomepageURL)
	input.Description = normalizeOptional(input.Description)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeApplicationUpdate(input ApplicationUpdateInput) ApplicationUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.Name = strings.TrimSpace(input.Name)
	input.ApplicationType = strings.ToLower(strings.TrimSpace(input.ApplicationType))
	input.OwnerOrgID = normalizeOptional(input.OwnerOrgID)
	input.OwnerUserID = normalizeOptional(input.OwnerUserID)
	input.HomepageURL = normalizeOptional(input.HomepageURL)
	input.Description = normalizeOptional(input.Description)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeEnvironmentCreate(input EnvironmentCreateInput) EnvironmentCreateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	input.BaseURL = normalizeOptionalBaseURL(input.BaseURL)
	input.UpstreamURL = normalizeOptionalUpstreamURL(input.UpstreamURL)
	input.PathPrefix = normalizeOptionalPathPrefix(input.PathPrefix)
	input.IssuerAlias = normalizeIssuerAlias(input.IssuerAlias)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeEnvironmentUpdate(input EnvironmentUpdateInput) EnvironmentUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.BaseURL = normalizeOptionalBaseURL(input.BaseURL)
	input.UpstreamURL = normalizeOptionalUpstreamURL(input.UpstreamURL)
	input.PathPrefix = normalizeOptionalPathPrefix(input.PathPrefix)
	input.IssuerAlias = normalizeIssuerAlias(input.IssuerAlias)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func validApplicationCreate(input ApplicationCreateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && validCode(input.Code, 64) &&
		validManagementText(input.Name, 128, false) && validApplicationType(input.ApplicationType) &&
		validOptionalIdentifier(input.OwnerOrgID) && validOptionalIdentifier(input.OwnerUserID) &&
		validOptionalURL(input.HomepageURL) && validOptionalText(input.Description, 1000) &&
		validApplicationStatus(input.Status)
}

func validApplicationUpdate(input ApplicationUpdateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.ApplicationID != "" && input.Version > 0 &&
		validManagementText(input.Name, 128, false) && validApplicationType(input.ApplicationType) &&
		validOptionalIdentifier(input.OwnerOrgID) && validOptionalIdentifier(input.OwnerUserID) &&
		validOptionalURL(input.HomepageURL) && validOptionalText(input.Description, 1000) &&
		validApplicationStatus(input.Status)
}

func validEnvironmentCreate(input EnvironmentCreateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.ApplicationID != "" &&
		validEnvironmentCode(input.Environment) && validOptionalBaseURL(input.BaseURL) &&
		validOptionalUpstreamURL(input.UpstreamURL) && validOptionalPathPrefix(input.PathPrefix) &&
		validGatewayTripleConsistent(input.BaseURL, input.UpstreamURL, input.PathPrefix) &&
		validIssuerAlias(input.IssuerAlias) && validMetadata(input.Metadata) && validEnvironmentStatus(input.Status)
}

func validEnvironmentUpdate(input EnvironmentUpdateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.ApplicationID != "" && input.EnvironmentID != "" && input.Version > 0 &&
		validOptionalBaseURL(input.BaseURL) && validOptionalUpstreamURL(input.UpstreamURL) &&
		validOptionalPathPrefix(input.PathPrefix) &&
		validGatewayTripleConsistent(input.BaseURL, input.UpstreamURL, input.PathPrefix) &&
		validIssuerAlias(input.IssuerAlias) && validMetadata(input.Metadata) && validEnvironmentStatus(input.Status)
}

func validApplicationStatus(value string) bool {
	switch value {
	case "DRAFT", "ACTIVE", "SUSPENDED", "RETIRED":
		return true
	default:
		return false
	}
}

func validApplicationStatusFilter(value string) bool {
	return value == "" || validApplicationStatus(value)
}

func validEnvironmentStatus(value string) bool { return value == "ACTIVE" || value == "DISABLED" }

func validEnvironmentStatusFilter(value string) bool {
	return value == "" || validEnvironmentStatus(value)
}

func validApplicationType(value string) bool {
	switch value {
	case "spa", "web", "backend", "mobile", "third_party":
		return true
	default:
		return false
	}
}

func validEnvironmentCode(value string) bool {
	switch value {
	case "dev", "test", "staging", "prod":
		return true
	default:
		return false
	}
}

func validCode(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (index > 0 && character >= '0' && character <= '9') ||
			(index > 0 && (character == '-' || character == '_' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func validOptionalIdentifier(value *string) bool {
	return value == nil || (len(*value) == 26 && validIdentifier(*value))
}

func validIdentifier(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'A' && character <= 'Z')) {
			return false
		}
	}
	return true
}

func validManagementText(value string, limit int, allowEmpty bool) bool {
	return len(value) <= limit && (allowEmpty || value != "")
}

func validOptionalText(value *string, limit int) bool {
	return value == nil || validManagementText(*value, limit, true)
}

func validOptionalURL(value *string) bool {
	if value == nil {
		return true
	}
	if len(*value) > 512 {
		return false
	}
	parsed, err := url.ParseRequestURI(*value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// validOptionalBaseURL 校验用于拼装浏览器跳转的公开门户地址。与普通主页 URL 不同，
// 它不得包含凭据、query 或 fragment。
func validOptionalBaseURL(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	if len(*value) > 512 || strings.Contains(*value, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(*value)
	return err == nil && parsed.Host != "" && parsed.User == nil && parsed.Opaque == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeOptionalBaseURL(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	normalized = strings.TrimRight(normalized, "/")
	return &normalized
}

// validOptionalUpstreamURL 是公开地址校验的严格内部对应版本，仅用于反向代理目标，
// 允许指向私网/回环地址，因此不适用公开 BaseURL 的公网后缀约束。
func validOptionalUpstreamURL(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	if len(*value) > 512 {
		return false
	}
	matches := safeUpstreamURLPattern.FindStringSubmatch(*value)
	if matches == nil {
		return false
	}
	if matches[3] != "" {
		port, err := strconv.Atoi(matches[3])
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	parsed, err := url.ParseRequestURI(*value)
	return err == nil && parsed.Host != "" && parsed.User == nil && parsed.Opaque == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeOptionalUpstreamURL(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	normalized = strings.TrimRight(normalized, "/")
	return &normalized
}

// validOptionalPathPrefix 接受门户根下的绝对路径，尾部斜杠可选；
// 协议相对（//）、Windows 盘符（\\）与任何 :// 协议形式一律拒绝。
func validOptionalPathPrefix(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	// 门户根路径保留给门户 UI 自身，不能分配给子系统。
	return *value != "/" && validPortalPath(*value, 128)
}

// validPortalPath 接受单根、无 query 的路径，字符集与 nginx 网关渲染器保持同一保守口径；
// 百分号编码与目录穿越一律拒绝。
func validPortalPath(value string, limit int) bool {
	if value == "" || len(value) > limit || !strings.HasPrefix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	// 字符集必须与 scripts/portal-gateway.sh 保持一致。除规避 nginx 配置元字符外，
	// 拒绝百分号编码还能防止 LoginTarget 拼入公开 BaseURL 之前，浏览器与 nginx
	// 对双重解码产生分歧。
	for _, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("/._~!+-", rune(character)) {
			continue
		}
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Path != "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeOptionalPathPrefix(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	normalized = strings.TrimRight(normalized, "/")
	if normalized == "" {
		return nil
	}
	return &normalized
}

// validGatewayTripleConsistent 对网关三字段执行粗粒度不变式：
//   - path_prefix 只有配合 BaseURL 才有意义（相对 LoginTarget.TargetURI 以 BaseURL 解析），
//     因此出现 path_prefix 即要求 BaseURL 已设置。
//   - upstream_url 与 path_prefix 共同描述一个反向代理条目，必须同时设置
//     （环境仅作逻辑边界时两者都留空）。
func validGatewayTripleConsistent(baseURL, upstreamURL, pathPrefix *string) bool {
	// 网关三元组必须保持可组合：路径前缀必须有公网基址，且内部上游与路径前缀必须
	// 同时出现，防止形成“门户可见但无法代理”的半配置状态。
	hasBaseURL := baseURL != nil && *baseURL != ""
	hasUpstream := upstreamURL != nil && *upstreamURL != ""
	hasPrefix := pathPrefix != nil && *pathPrefix != ""
	if hasPrefix && !hasBaseURL {
		return false
	}
	if hasUpstream != hasPrefix {
		return false
	}
	return true
}

func validMetadata(value json.RawMessage) bool {
	if len(value) == 0 {
		return true
	}
	if len(value) > maxMetadataBytes || !json.Valid(value) {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return false
	}
	return !containsSensitiveMetadataKey(object)
}

func containsSensitiveMetadataKey(object map[string]json.RawMessage) bool {
	for key, value := range object {
		if sensitiveMetadataKey(key) {
			return true
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) == nil && nested != nil && containsSensitiveMetadataKey(nested) {
			return true
		}
	}
	return false
}

func sensitiveMetadataKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, fragment := range []string{"secret", "password", "private_key", "access_token", "refresh_token", "api_key"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func normalizeOptional(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	return &normalized
}

// normalizeIssuerAlias 规范化持久化的认证提供方。缺失或空白值是基础平台 OIDC 的
// 历史表示，因此显式回填为 platform 而不是保留 NULL，使新写入全部满足
// issuer-alias 迁移引入的数据库不变式。
func normalizeIssuerAlias(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return stringPointer(IssuerAliasPlatform)
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	return &normalized
}

func validIssuerAlias(value *string) bool {
	if value == nil {
		return false
	}
	switch *value {
	case IssuerAliasPlatform, IssuerAliasKeycloak:
		return true
	default:
		return false
	}
}
