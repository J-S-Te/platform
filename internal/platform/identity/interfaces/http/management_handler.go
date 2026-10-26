package identityhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	authzapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// 批量员工导入最多包含 MaxBatchUserCreateItems 行的组织、岗位与角色元数据。
// 保留硬上限的同时，让常规 100 行的 CSV/JSON 载荷能顺利通过 HTTP 解码器，
// 而不是被静默拒绝。
const maxManagementRequestBytes = 2 << 20

// ManagementHandler 是 P0 级 IAM 用户、账号与组织操作的 HTTP 适配器。
type ManagementHandler struct {
	service         managementApplicationService
	logger          *slog.Logger
	scopeAuthorizer managementscope.Authorizer
	importGateway   employeeImportGateway
}

type employeeImportGateway interface {
	StoreCSV(context.Context, string, string, string, []byte) error
}

type managementApplicationService interface {
	ListUsers(context.Context, string, application.PageRequest) (application.PageResult[application.UserView], error)
	CreateUser(context.Context, application.UserCreateInput) (application.UserView, error)
	CreateEmployee(context.Context, application.EmployeeCreateInput) (application.EmployeeCreateResult, error)
	CreateEmployeesBatch(context.Context, application.EmployeeBatchCreateInput) (application.EmployeeBatchCreateResult, error)
	CreateUsersBatch(context.Context, application.UserBatchCreateInput) ([]application.UserView, error)
	GetUser(context.Context, string, string) (application.UserView, error)
	UpdateUser(context.Context, application.UserUpdateInput) (application.UserView, error)
	DeleteUser(context.Context, application.UserDeleteInput) error
	ListAccounts(context.Context, string, application.PageRequest) (application.PageResult[domain.Account], error)
	UpdateAccount(context.Context, application.AccountUpdateInput) (domain.Account, error)
	ListOrgUnits(context.Context, string, application.PageRequest) (application.PageResult[domain.OrgUnit], error)
	CreateOrgUnit(context.Context, application.OrgUnitCreateInput) (domain.OrgUnit, error)
	UpdateOrgUnit(context.Context, application.OrgUnitUpdateInput) (domain.OrgUnit, error)
	DeleteOrgUnit(context.Context, application.OrgUnitDeleteInput) error
	ListPositions(context.Context, string, application.PageRequest) (application.PageResult[domain.Position], error)
	CreatePosition(context.Context, application.PositionCreateInput) (domain.Position, error)
	DeletePosition(context.Context, application.PositionDeleteInput) error
	ListMemberships(context.Context, string, application.PageRequest) (application.PageResult[domain.Membership], error)
	CreateMembership(context.Context, application.MembershipCreateInput) (domain.Membership, error)
	UpdateMembership(context.Context, application.MembershipUpdateInput) (domain.Membership, error)
}

// ConfigureEmployeeImportGateway 启用 fail-closed 的 CSV 持久化与校验。
// 生产环境必须在暴露 multipart 导入路径之前配置该依赖。
func (handler *ManagementHandler) ConfigureEmployeeImportGateway(gateway employeeImportGateway) {
	handler.importGateway = gateway
}

// NewManagementHandler 构造 IAM 管理 HTTP 适配器。
func NewManagementHandler(service managementApplicationService, logger *slog.Logger, scopeAuthorizer managementscope.Authorizer) (*ManagementHandler, error) {
	if service == nil || logger == nil || scopeAuthorizer == nil {
		return nil, errors.New("identity management handler dependencies must not be nil")
	}
	return &ManagementHandler{service: service, logger: logger, scopeAuthorizer: scopeAuthorizer}, nil
}

// userCreateRequest 刻意不含 employee_no 与 version：工号由后端生成，
// 乐观锁版本号只对更新有意义。
type userCreateRequest struct {
	DisplayName string     `json:"display_name"`
	Email       *string    `json:"email"`
	Mobile      *string    `json:"mobile"`
	Status      *string    `json:"status"`
	ValidUntil  *time.Time `json:"valid_until"`
}

type applicationRoleAssignmentRequest struct {
	ApplicationCode string `json:"application_code,omitempty"`
	ApplicationName string `json:"application_name,omitempty"`
	RoleCode        string `json:"role_code,omitempty"`
	RoleName        string `json:"role_name,omitempty"`
}

type userBatchCreateItemRequest struct {
	userCreateRequest
	ApplicationRoles []applicationRoleAssignmentRequest `json:"application_roles,omitempty"`
}

type userRequest struct {
	DisplayName    string     `json:"display_name"`
	EmployeeNo     *string    `json:"employee_no"`
	PMSPersonID    *string    `json:"pms_person_id"`
	Email          *string    `json:"email"`
	Mobile         *string    `json:"mobile"`
	Status         *string    `json:"status"`
	ValidUntil     *time.Time `json:"valid_until"`
	UpdateValidity bool       `json:"update_validity"`
	Version        uint64     `json:"version"`
}

type userBatchCreateRequest struct {
	Items []userBatchCreateItemRequest `json:"items"`
}

// employeeCreateRequest 让员工入职保持原子，同时保留独立的 /users、/accounts、
// /memberships 请求契约，供单项维护操作继续使用。
type employeeCreateRequest struct {
	User       userCreateRequest                `json:"user"`
	Account    *employeeAccountCreateRequest    `json:"account"`
	Membership *employeeMembershipCreateRequest `json:"membership"`
}

type employeeAccountCreateRequest struct {
	AccountName     string     `json:"account_name"`
	InitialPassword string     `json:"initial_password"`
	ValidUntil      *time.Time `json:"valid_until"`
}

type employeeMembershipCreateRequest struct {
	OrgUnitID            string  `json:"org_unit_id"`
	PositionID           string  `json:"position_id"`
	MembershipType       string  `json:"membership_type"`
	EffectiveFrom        *string `json:"effective_from"`
	EffectiveTo          *string `json:"effective_to"`
	InheritAuthorization *bool   `json:"inherit_authorization"`
}

type employeeCreateResponse struct {
	User       userResponse        `json:"user"`
	Account    *accountResponse    `json:"account,omitempty"`
	Membership *membershipResponse `json:"membership,omitempty"`
}

type userBatchCreateResponse struct {
	Items []userResponse `json:"items"`
	Total int            `json:"total"`
	// InitialCredentials 仅在服务端生成随机初始密码的批量导入时返回，供导入管理员
	// 分发；账号首登强制改密，凭据不落日志。
	InitialCredentials []application.EmployeeInitialCredential `json:"initial_credentials,omitempty"`
}

type employeeBatchCreateItemRequest struct {
	userCreateRequest
	LineNo int `json:"line_no,omitempty"`
	// Organization 与 Position 是当前的规范字段。*_name 别名仅为兼容更早的导入
	// 页面和使用更明确命名的 CSV 客户端而保留。
	Organization     string                             `json:"organization"`
	OrganizationName string                             `json:"organization_name"`
	Position         string                             `json:"position"`
	PositionName     string                             `json:"position_name"`
	ApplicationRoles []applicationRoleAssignmentRequest `json:"application_roles,omitempty"`
}

type employeeBatchCreateRequest struct {
	Items []employeeBatchCreateItemRequest `json:"items"`
}

type employeeBatchIssue struct {
	Row     int    `json:"row"`
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type employeeBatchValidationDetails struct {
	Issues []employeeBatchIssue `json:"issues"`
}

type userDeleteRequest struct {
	Version uint64 `json:"version"`
}

type accountUpdateRequest struct {
	Status         string     `json:"status"`
	ValidUntil     *time.Time `json:"valid_until"`
	UpdateValidity bool       `json:"update_validity"`
	Version        uint64     `json:"version"`
}

// orgUnitCreateRequest 刻意不要求组织编码。code 保留为被忽略的兼容字段，使旧前端
// 滚动发布期间不因严格 JSON 解码而失败；权威编码始终由应用服务生成并返回。
type orgUnitCreateRequest struct {
	ParentID  *string `json:"parent_id"`
	Code      *string `json:"code,omitempty"`
	Name      string  `json:"name"`
	SortOrder int     `json:"sort_order"`
}

type orgUnitUpdateRequest struct {
	ParentID  *string `json:"parent_id"`
	Name      string  `json:"name"`
	SortOrder int     `json:"sort_order"`
	Version   uint64  `json:"version"`
}

type orgUnitDeleteRequest struct {
	Version uint64 `json:"version"`
}

// positionCreateRequest 在滚动发布期间把 code 保留为被忽略的兼容字段。
// 权威的 POS-<ULID> 编码始终由后端返回。
type positionCreateRequest struct {
	OrgUnitID string  `json:"org_unit_id"`
	Code      *string `json:"code,omitempty"`
	Name      string  `json:"name"`
}

type positionDeleteRequest struct {
	Version uint64 `json:"version"`
}
type membershipRequest struct {
	UserID               string  `json:"user_id"`
	OrgUnitID            string  `json:"org_unit_id"`
	PositionID           string  `json:"position_id"`
	MembershipType       string  `json:"membership_type"`
	EffectiveFrom        *string `json:"effective_from"`
	EffectiveTo          *string `json:"effective_to"`
	InheritAuthorization *bool   `json:"inherit_authorization"`
	Status               *string `json:"status"`
	Version              uint64  `json:"version"`
}

type referenceResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type userResponse struct {
	UserID       string     `json:"user_id"`
	DisplayName  string     `json:"display_name"`
	EmployeeNo   *string    `json:"employee_no,omitempty"`
	PMSPersonID  *string    `json:"pms_person_id,omitempty"`
	Email        *string    `json:"email,omitempty"`
	MobileMasked *string    `json:"mobile_masked,omitempty"`
	Status       string     `json:"status"`
	ValidUntil   *time.Time `json:"valid_until,omitempty"`
	Version      uint64     `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}
type accountResponse struct {
	AccountID           string             `json:"account_id"`
	UserID              *string            `json:"user_id,omitempty"`
	User                *referenceResponse `json:"user,omitempty"`
	AccountName         string             `json:"account_name"`
	AccountType         string             `json:"account_type"`
	AuthSource          string             `json:"auth_source"`
	PasswordInitialized bool               `json:"password_initialized"`
	Status              string             `json:"status"`
	LastLoginAt         *time.Time         `json:"last_login_at,omitempty"`
	LockedUntil         *time.Time         `json:"locked_until,omitempty"`
	ValidUntil          *time.Time         `json:"valid_until,omitempty"`
	Version             uint64             `json:"version"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
}
type orgUnitResponse struct {
	OrgUnitID string  `json:"org_unit_id"`
	ParentID  *string `json:"parent_id,omitempty"`
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	// Path 与 depth 是持久化的层级元数据。返回它们使导出功能即使结果被
	// 范围限定，也能重建权威组织树。
	Path      string `json:"path,omitempty"`
	Depth     uint   `json:"depth"`
	SortOrder int    `json:"sort_order"`
	Status    string `json:"status"`
	Version   uint64 `json:"version"`
}
type positionResponse struct {
	PositionID string `json:"position_id"`
	OrgUnitID  string `json:"org_unit_id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Version    uint64 `json:"version"`
}
type membershipResponse struct {
	MembershipID         string            `json:"membership_id"`
	User                 referenceResponse `json:"user"`
	OrgUnit              referenceResponse `json:"org_unit"`
	Position             referenceResponse `json:"position"`
	MembershipType       string            `json:"membership_type"`
	IsPrimary            bool              `json:"is_primary"`
	EffectiveFrom        *string           `json:"effective_from,omitempty"`
	EffectiveTo          *string           `json:"effective_to,omitempty"`
	Status               string            `json:"status"`
	InheritAuthorization bool              `json:"inherit_authorization"`
	Version              uint64            `json:"version"`
}
type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// ListUsers 返回当前租户的用户分页列表。
func (handler *ManagementHandler) ListUsers(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	query, err := parsePageRequest(request)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	result, err := handler.service.ListUsers(request.Context(), principal.Tenant.ID, query)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]userResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toUserResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", pageResponse[userResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// CreateUser 创建用户；租户取自会话主体，工号由服务端生成。
func (handler *ManagementHandler) CreateUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload userCreateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	status := domain.StatusActive
	if payload.Status != nil {
		status = *payload.Status
	}
	result, err := handler.service.CreateUser(request.Context(), application.UserCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, DisplayName: payload.DisplayName, Email: payload.Email, Mobile: payload.Mobile, Status: status, ValidUntil: payload.ValidUntil})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	response := toUserResponse(result)
	// 安全审查 SEC-D5：创建类 body寻址操作没有路径 id，登记目标对象 id 供审计
	// 中间件写入 ResourceID 与 metadata（否则审计只有操作者+路由，无法定位目标）。
	middleware.MarkCreatedResource(request, response.UserID)
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "用户已创建", response)
}

// 员工入职会在一个应用事务内创建用户、平台基线角色以及可选账号和首个任职。HTTP 层
// 根据请求实际包含的子资源追加检查账号、任职权限，避免只有用户创建权限的操作者借聚合
// 接口间接创建账号或任职；独立资源接口仅用于维护和例外流程。
func (handler *ManagementHandler) CreateEmployee(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload employeeCreateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	if payload.Account != nil && !principalHasPermission(principal, "platform:account:create") {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	if payload.Membership != nil && !principalHasPermission(principal, "platform:membership:create") {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	status := domain.StatusActive
	if payload.User.Status != nil {
		status = *payload.User.Status
	}
	input := application.EmployeeCreateInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, DisplayName: payload.User.DisplayName,
		Email: payload.User.Email, Mobile: payload.User.Mobile, Status: status, ValidUntil: payload.User.ValidUntil,
	}
	if payload.Account != nil {
		input.Account = &application.EmployeeAccountCreateInput{AccountName: payload.Account.AccountName, InitialPassword: payload.Account.InitialPassword, ValidUntil: payload.Account.ValidUntil}
	}
	if payload.Membership != nil {
		from, err := parseDate(payload.Membership.EffectiveFrom)
		if err != nil {
			handler.validation(writer, request)
			return
		}
		to, err := parseDateEnd(payload.Membership.EffectiveTo)
		if err != nil {
			handler.validation(writer, request)
			return
		}
		input.Membership = &application.EmployeeMembershipCreateInput{
			OrgUnitID: payload.Membership.OrgUnitID, PositionID: payload.Membership.PositionID,
			MembershipType: payload.Membership.MembershipType, EffectiveFrom: from, EffectiveTo: to,
			InheritAuthorization: payload.Membership.InheritAuthorization,
		}
	}
	result, err := handler.service.CreateEmployee(request.Context(), input)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	response := employeeCreateResponse{User: toUserResponse(result.User)}
	if result.Account != nil {
		account := toAccountResponse(*result.Account)
		response.Account = &account
	}
	if result.Membership != nil {
		membership := toMembershipResponse(*result.Membership)
		response.Membership = &membership
	}
	// SEC-D5：登记被创建的用户对象 id 供审计中间件采集。
	middleware.MarkCreatedResource(request, response.User.UserID)
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "员工已创建", response)
}

func principalHasPermission(principal authctx.Principal, expected string) bool {
	for _, permission := range principal.PermissionCodes {
		if permission == expected {
			return true
		}
	}
	return false
}

// 批量导入最多一次提交一百名普通用户并保持整批原子性。应用角色是额外授权动作，只有
// 请求实际携带角色时才要求角色绑定更新权限；工号和平台基线角色仍由应用服务强制生成。
func (handler *ManagementHandler) CreateUsersBatch(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload userBatchCreateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	items := make([]application.UserCreateInput, 0, len(payload.Items))
	hasApplicationRoles := false
	for _, item := range payload.Items {
		status := domain.StatusActive
		if item.Status != nil {
			status = *item.Status
		}
		roles := make([]application.ApplicationRoleAssignment, 0, len(item.ApplicationRoles))
		for _, role := range item.ApplicationRoles {
			hasApplicationRoles = true
			roles = append(roles, application.ApplicationRoleAssignment{
				ApplicationCode: role.ApplicationCode, ApplicationName: role.ApplicationName,
				RoleCode: role.RoleCode, RoleName: role.RoleName,
			})
		}
		items = append(items, application.UserCreateInput{
			DisplayName: item.DisplayName, Email: item.Email, Mobile: item.Mobile, Status: status, ApplicationRoles: roles,
		})
	}
	if hasApplicationRoles && !principalHasPermission(principal, "platform:role-binding:update") {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	result, err := handler.service.CreateUsersBatch(request.Context(), application.UserBatchCreateInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, Items: items,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	responses := make([]userResponse, 0, len(result))
	for _, item := range result {
		responses = append(responses, toUserResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "用户已批量创建", userBatchCreateResponse{Items: responses, Total: len(responses)})
}

// CreateEmployeesBatch 先在租户内解析组织与岗位名称，再把完整员工批次
// 委托给原子的应用服务。
func (handler *ManagementHandler) CreateEmployeesBatch(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload employeeBatchCreateRequest
	if strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "multipart/form-data") {
		request.Body = http.MaxBytesReader(writer, request.Body, maxManagementRequestBytes+(128<<10))
		if err := request.ParseMultipartForm(maxManagementRequestBytes + (128 << 10)); err != nil {
			handler.validation(writer, request)
			return
		}
		file, header, err := request.FormFile("file")
		if err != nil || handler.importGateway == nil {
			httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, httperror.New("IAM_IMPORT_GATEWAY_UNAVAILABLE", "人员导入文件网关暂时不可用", nil))
			return
		}
		defer file.Close()
		content, err := io.ReadAll(io.LimitReader(file, maxManagementRequestBytes+1))
		if err != nil || len(content) == 0 || len(content) > maxManagementRequestBytes || !strings.HasSuffix(strings.ToLower(header.Filename), ".csv") {
			handler.validation(writer, request)
			return
		}
		var selection employeeCSVSelection
		if err = json.Unmarshal([]byte(request.FormValue("payload")), &selection); err != nil {
			handler.validation(writer, request)
			return
		}
		requestID := strings.TrimSpace(request.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = fmt.Sprintf("iam-import-%d", time.Now().UTC().UnixNano())
		}
		if err = handler.importGateway.StoreCSV(request.Context(), requestID, principal.Tenant.ID, principal.User.ID, content); err != nil {
			handler.logger.Error("employee import file gateway failed", "request_id", requestID, "error", err)
			httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, httperror.New("IAM_IMPORT_GATEWAY_UNAVAILABLE", "人员导入文件校验或存储失败，请稍后重试", nil))
			return
		}
		payload, err = parseEmployeeCSV(content, selection.SelectedLines)
		if err != nil {
			handler.validation(writer, request)
			return
		}
	} else if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	items := make([]application.EmployeeCreateInput, 0, len(payload.Items))
	hasRoles := false
	for index, item := range payload.Items {
		row := item.LineNo
		if row <= 0 {
			row = index + 1
		}
		organization := firstNonEmpty(item.Organization, item.OrganizationName)
		position := firstNonEmpty(item.Position, item.PositionName)
		orgID, err := resolveExactOrganization(request.Context(), handler.service, principal.Tenant.ID, organization)
		if err != nil {
			if errors.Is(err, application.ErrValidation) {
				handler.batchValidation(writer, request, employeeBatchIssue{Row: row, Field: "organization", Code: "AMBIGUOUS_ORGANIZATION", Message: "组织名称匹配到多个有效组织，请改用唯一名称"})
				return
			}
			handler.writeError(writer, request, err)
			return
		}
		if orgID == "" {
			handler.batchValidation(writer, request, employeeBatchIssue{Row: row, Field: "organization", Code: "ORGANIZATION_NOT_FOUND", Message: "组织不存在或已停用"})
			return
		}
		positionID, err := resolveExactPosition(request.Context(), handler.service, principal.Tenant.ID, orgID, position)
		if err != nil {
			if errors.Is(err, application.ErrValidation) {
				handler.batchValidation(writer, request, employeeBatchIssue{Row: row, Field: "position", Code: "AMBIGUOUS_POSITION", Message: "岗位名称匹配到多个有效岗位，请改用唯一名称"})
				return
			}
			handler.writeError(writer, request, err)
			return
		}
		if positionID == "" {
			handler.batchValidation(writer, request, employeeBatchIssue{Row: row, Field: "position", Code: "POSITION_NOT_FOUND", Message: "岗位不存在、已停用或不属于该组织"})
			return
		}
		roles := make([]application.ApplicationRoleAssignment, 0, len(item.ApplicationRoles))
		for _, role := range item.ApplicationRoles {
			hasRoles = true
			roles = append(roles, application.ApplicationRoleAssignment{ApplicationCode: role.ApplicationCode, ApplicationName: role.ApplicationName, RoleCode: role.RoleCode, RoleName: role.RoleName})
		}
		items = append(items, application.EmployeeCreateInput{DisplayName: item.DisplayName, Email: item.Email, Mobile: item.Mobile, Status: valueOrDefault(item.Status, domain.StatusActive), Membership: &application.EmployeeMembershipCreateInput{OrgUnitID: orgID, PositionID: positionID, MembershipType: domain.MembershipPrimary}, ApplicationRoles: roles})
	}
	if hasRoles && !principalHasPermission(principal, "platform:role-binding:update") {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.New("AUTH_FORBIDDEN", "没有执行此操作的权限", employeeBatchValidationDetails{Issues: []employeeBatchIssue{{Row: 0, Field: "application_roles", Code: "ROLE_BINDING_FORBIDDEN", Message: "导入应用角色需要 platform:role-binding:update 权限"}}}))
		return
	}
	result, err := handler.service.CreateEmployeesBatch(request.Context(), application.EmployeeBatchCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, Items: items})
	if err != nil {
		if errors.Is(err, application.ErrValidation) {
			handler.batchValidation(writer, request, employeeBatchIssue{Row: 0, Field: "items", Code: "BATCH_VALIDATION_FAILED", Message: "批量数据未通过服务端校验；请检查姓名、邮箱、手机号、应用角色和任职关系"})
			return
		}
		if errors.Is(err, application.ErrNotFound) {
			handler.logger.Error("employee batch reference not found", "error", err)
			handler.batchValidation(writer, request, employeeBatchIssue{Row: 0, Field: "items", Code: "BATCH_REFERENCE_NOT_FOUND", Message: "批量导入依赖的组织、岗位、平台普通用户角色或应用角色不存在，请刷新目录后重试"})
			return
		}
		if errors.Is(err, application.ErrConflict) {
			httpresponse.WriteError(writer, request, http.StatusConflict, httperror.New("IAM_CONFLICT", "批量导入存在账号冲突", employeeBatchValidationDetails{Issues: []employeeBatchIssue{{Row: 0, Field: "display_name", Code: "ACCOUNT_CONFLICT", Message: "根据姓名生成的登录账号已存在或在本批次中重复"}}}))
			return
		}
		handler.writeError(writer, request, err)
		return
	}
	responses := make([]userResponse, 0, len(result.Users))
	for _, item := range result.Users {
		responses = append(responses, toUserResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "用户已批量创建", userBatchCreateResponse{Items: responses, Total: len(responses), InitialCredentials: result.InitialCredentials})
}

// 列表端点是分页的。导入解析必须遍历每一个匹配页：只取前 100 行曾导致租户
// 超过一页后合法名称"消失"。保留精确名称匹配，使部分关键字命中绝不可能选中
// 错误的组织或岗位。
func resolveExactOrganization(ctx context.Context, service managementApplicationService, tenantID, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var found string
	for page := 1; ; page++ {
		result, err := service.ListOrgUnits(ctx, tenantID, application.PageRequest{Page: page, PageSize: 100, Keyword: name, Status: domain.StatusActive})
		if err != nil {
			return "", err
		}
		for _, org := range result.Items {
			if strings.TrimSpace(org.Name) == name {
				if found != "" && found != org.ID {
					return "", application.ErrValidation
				}
				found = org.ID
			}
		}
		if len(result.Items) == 0 || page*result.PageSize >= int(result.Total) || result.PageSize <= 0 {
			break
		}
	}
	return found, nil
}

func resolveExactPosition(ctx context.Context, service managementApplicationService, tenantID, orgID, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var found string
	for page := 1; ; page++ {
		result, err := service.ListPositions(ctx, tenantID, application.PageRequest{Page: page, PageSize: 100, Keyword: name, Status: domain.StatusActive})
		if err != nil {
			return "", err
		}
		for _, position := range result.Items {
			if position.OrgUnitID == orgID && strings.TrimSpace(position.Name) == name {
				if found != "" && found != position.ID {
					return "", application.ErrValidation
				}
				found = position.ID
			}
		}
		if len(result.Items) == 0 || page*result.PageSize >= int(result.Total) || result.PageSize <= 0 {
			break
		}
	}
	return found, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func valueOrDefault(value *string, fallback string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return fallback
	}
	return *value
}

// GetUser 返回当前租户内指定用户的详情。
func (handler *ManagementHandler) GetUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	result, err := handler.service.GetUser(request.Context(), principal.Tenant.ID, request.PathValue("user_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", toUserResponse(result))
}

// mobileUpdateRequested 判断本次请求是否真的要求修改手机号。
//
// 表单会把未填写的手机号序列化成空字符串；如果把「字段存在」当成「改成空值」，
// 任何一次只改状态或姓名的保存都会把手机号密文与摘要写成 NULL。只有提交了非空白
// 值才算一次手机号修改。
func mobileUpdateRequested(mobile *string) bool {
	return mobile != nil && strings.TrimSpace(*mobile) != ""
}

// UpdateUser 更新用户基础信息与有效期；手机号仅在提交非空白值时才被视为修改，
// 乐观锁 Version 不匹配时失败。
func (handler *ManagementHandler) UpdateUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload userRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	result, err := handler.service.UpdateUser(request.Context(), application.UserUpdateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, UserID: request.PathValue("user_id"), DisplayName: payload.DisplayName, EmployeeNo: payload.EmployeeNo, PMSPersonID: payload.PMSPersonID, Email: payload.Email, Mobile: payload.Mobile, Status: payload.Status, ValidUntil: payload.ValidUntil, UpdateValidity: payload.UpdateValidity || payload.ValidUntil != nil, Version: payload.Version, UpdateMobile: mobileUpdateRequested(payload.Mobile)})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "用户已更新", toUserResponse(result))
}

// DeleteUser 逻辑删除用户并停用其关联的全部登录路径。
func (handler *ManagementHandler) DeleteUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload userDeleteRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	if err := handler.service.DeleteUser(request.Context(), application.UserDeleteInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, UserID: request.PathValue("user_id"), Version: payload.Version,
	}); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "用户已删除", map[string]any{})
}

// ListAccounts 返回当前租户的登录账号分页列表。
func (handler *ManagementHandler) ListAccounts(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	query, err := parsePageRequest(request)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	result, err := handler.service.ListAccounts(request.Context(), principal.Tenant.ID, query)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]accountResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toAccountResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", pageResponse[accountResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// UpdateAccount 更新账号状态与有效期；乐观锁 Version 不匹配时失败。
func (handler *ManagementHandler) UpdateAccount(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload accountUpdateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	result, err := handler.service.UpdateAccount(request.Context(), application.AccountUpdateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, AccountID: request.PathValue("account_id"), Status: payload.Status, ValidUntil: payload.ValidUntil, UpdateValidity: payload.UpdateValidity || payload.ValidUntil != nil, Version: payload.Version})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "账号已更新", toAccountResponse(result))
}

// ListOrgUnits 返回组织单元分页列表，结果按操作者的数据管理范围过滤。
func (handler *ManagementHandler) ListOrgUnits(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	query, err := parsePageRequest(request)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:organization:read")
	if !ok {
		return
	}
	query = applyManagementScope(query, scope)
	result, err := handler.service.ListOrgUnits(request.Context(), principal.Tenant.ID, query)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]orgUnitResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toOrgUnitResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", pageResponse[orgUnitResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// CreateOrgUnit 创建组织单元；创建根节点要求不受限范围，指定父节点时校验
// 操作者对父节点拥有写权限。
func (handler *ManagementHandler) CreateOrgUnit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload orgUnitCreateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:organization:create")
	if !ok {
		return
	}
	if payload.ParentID == nil {
		if !scope.Unrestricted {
			handler.forbidden(writer, request)
			return
		}
	} else if !scope.Allows(*payload.ParentID, *payload.ParentID) {
		handler.forbidden(writer, request)
		return
	}
	result, err := handler.service.CreateOrgUnit(request.Context(), application.OrgUnitCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, ParentID: payload.ParentID, Name: payload.Name, SortOrder: payload.SortOrder})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// SEC-D5：登记新建组织单元 id 供审计中间件采集。
	orgUnit := toOrgUnitResponse(result)
	middleware.MarkCreatedResource(request, orgUnit.OrgUnitID)
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "组织单元已创建", orgUnit)
}

// UpdateOrgUnit 更新组织单元；目标节点与移动后的父节点都必须在操作者的
// 数据管理范围内，且受限范围不允许把父节点清空。
func (handler *ManagementHandler) UpdateOrgUnit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload orgUnitUpdateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:organization:update")
	if !ok {
		return
	}
	orgUnitID := request.PathValue("org_unit_id")
	if !scope.Allows(orgUnitID, orgUnitID) || (!scope.Unrestricted && payload.ParentID == nil) || (payload.ParentID != nil && !scope.Allows(*payload.ParentID, *payload.ParentID)) {
		handler.forbidden(writer, request)
		return
	}
	result, err := handler.service.UpdateOrgUnit(request.Context(), application.OrgUnitUpdateInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, OrgUnitID: orgUnitID,
		ParentID: payload.ParentID, Name: payload.Name, SortOrder: payload.SortOrder, Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "组织单元已更新", toOrgUnitResponse(result))
}

// DeleteOrgUnit 逻辑删除组织单元，并同步停用其下岗位与任职关系。
func (handler *ManagementHandler) DeleteOrgUnit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload orgUnitDeleteRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:organization:delete")
	if !ok {
		return
	}
	orgUnitID := request.PathValue("org_unit_id")
	if !scope.Allows(orgUnitID, orgUnitID) {
		handler.forbidden(writer, request)
		return
	}
	if err := handler.service.DeleteOrgUnit(request.Context(), application.OrgUnitDeleteInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, OrgUnitID: orgUnitID, Version: payload.Version,
	}); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "组织单元已删除，相关岗位和任职关系已停用", map[string]any{})
}

// ListPositions 返回岗位分页列表，结果按操作者的数据管理范围过滤。
func (handler *ManagementHandler) ListPositions(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	query, err := parsePageRequest(request)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:position:read")
	if !ok {
		return
	}
	query = applyManagementScope(query, scope)
	result, err := handler.service.ListPositions(request.Context(), principal.Tenant.ID, query)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]positionResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toPositionResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", pageResponse[positionResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// CreatePosition 在指定组织单元下创建岗位；目标组织单元必须在操作者的
// 数据管理范围内。
func (handler *ManagementHandler) CreatePosition(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload positionCreateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:position:create")
	if !ok {
		return
	}
	if !scope.Allows(payload.OrgUnitID, "") {
		handler.forbidden(writer, request)
		return
	}
	result, err := handler.service.CreatePosition(request.Context(), application.PositionCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, OrgUnitID: payload.OrgUnitID, Name: payload.Name})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// SEC-D5：登记新建岗位 id 供审计中间件采集。
	position := toPositionResponse(result)
	middleware.MarkCreatedResource(request, position.PositionID)
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "岗位已创建", position)
}

// DeletePosition 逻辑删除岗位并停用相关任职关系；目标岗位必须先通过
// 数据管理范围的存在性校验。
func (handler *ManagementHandler) DeletePosition(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload positionDeleteRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:position:delete")
	if !ok {
		return
	}
	positionID := request.PathValue("position_id")
	if !handler.requireExistingResourceScope(writer, request, principal.Tenant.ID, scope, managementscope.ResourcePosition, positionID) {
		return
	}
	if err := handler.service.DeletePosition(request.Context(), application.PositionDeleteInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID,
		PositionID: positionID, Version: payload.Version,
	}); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "岗位已删除，相关任职关系已停用", map[string]any{})
}

// ListMemberships 返回任职关系分页列表，结果按操作者的数据管理范围过滤。
func (handler *ManagementHandler) ListMemberships(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	query, err := parsePageRequest(request)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:membership:read")
	if !ok {
		return
	}
	query = applyManagementScope(query, scope)
	result, err := handler.service.ListMemberships(request.Context(), principal.Tenant.ID, query)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]membershipResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toMembershipResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", pageResponse[membershipResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// CreateMembership 创建任职关系，共用 writeMembership 的校验与审计链路。
func (handler *ManagementHandler) CreateMembership(writer http.ResponseWriter, request *http.Request) {
	handler.writeMembership(writer, request, false)
}

// UpdateMembership 更新任职关系，共用 writeMembership 的校验与审计链路。
func (handler *ManagementHandler) UpdateMembership(writer http.ResponseWriter, request *http.Request) {
	handler.writeMembership(writer, request, true)
}

func (handler *ManagementHandler) writeMembership(writer http.ResponseWriter, request *http.Request, update bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	var payload membershipRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	permissionCode := "platform:membership:create"
	if update {
		permissionCode = "platform:membership:update"
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, permissionCode)
	if !ok {
		return
	}
	if update && !handler.requireExistingResourceScope(writer, request, principal.Tenant.ID, scope, managementscope.ResourceMembership, request.PathValue("membership_id")) {
		return
	}
	if !scope.Allows(payload.OrgUnitID, "") {
		handler.forbidden(writer, request)
		return
	}
	from, err := parseDate(payload.EffectiveFrom)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	to, err := parseDateEnd(payload.EffectiveTo)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	input := application.MembershipCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, UserID: payload.UserID, OrgUnitID: payload.OrgUnitID, PositionID: payload.PositionID, MembershipType: payload.MembershipType, EffectiveFrom: from, EffectiveTo: to, InheritAuthorization: payload.InheritAuthorization}
	if !update {
		result, err := handler.service.CreateMembership(request.Context(), input)
		if err != nil {
			handler.writeError(writer, request, err)
			return
		}
		// SEC-D5：登记新建任职 id 供审计中间件采集。
		membership := toMembershipResponse(result)
		middleware.MarkCreatedResource(request, membership.MembershipID)
		httpresponse.WriteSuccess(writer, request, http.StatusCreated, "任职已创建", membership)
		return
	}
	result, err := handler.service.UpdateMembership(request.Context(), application.MembershipUpdateInput{MembershipCreateInput: input, MembershipID: request.PathValue("membership_id"), Status: payload.Status, Version: payload.Version})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "任职已更新", toMembershipResponse(result))
}

func (handler *ManagementHandler) requireManagementScope(writer http.ResponseWriter, request *http.Request, principal authctx.Principal, permissionCode string) (managementscope.Scope, bool) {
	// 会话中的扁平权限码只决定是否可能执行操作；实际可管理的组织和资源范围由服务端
	// 根据当前主体重新解析，避免把前端提交的 org_unit_id 当成授权证据。
	scope, err := handler.scopeAuthorizer.Resolve(request.Context(), managementscope.Subject{
		TenantID:  principal.Tenant.ID,
		UserID:    principal.User.ID,
		AccountID: principal.Account.ID,
	}, permissionCode)
	if err != nil {
		handler.logger.Error("resolve management authorization scope", "permission", permissionCode, "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return managementscope.Scope{}, false
	}
	if scope.Empty() {
		handler.forbidden(writer, request)
		return managementscope.Scope{}, false
	}
	return scope, true
}

func applyManagementScope(query application.PageRequest, scope managementscope.Scope) application.PageRequest {
	// 受限范围被下推到仓储查询，而不是先读取全租户数据再在内存中过滤，防止分页总数、
	// 搜索结果或错误分支泄露范围外资源。
	if scope.Unrestricted {
		return query
	}
	query.ScopeRestricted = true
	query.AllowedOrgUnitIDs = append([]string(nil), scope.OrgUnitIDs...)
	query.AllowedResourceIDs = append([]string(nil), scope.ResourceIDs...)
	return query
}

func (handler *ManagementHandler) requireExistingResourceScope(writer http.ResponseWriter, request *http.Request, tenantID string, scope managementscope.Scope, resourceType, resourceID string) bool {
	if scope.Unrestricted {
		return true
	}
	// 更新和删除必须用路径中的资源 ID 回查其真实组织归属；客户端提交的组织字段可能被
	// 篡改，不能用于证明该资源位于操作者的授权范围内。
	resource, err := handler.scopeAuthorizer.ResourceContext(request.Context(), tenantID, resourceType, resourceID)
	if errors.Is(err, managementscope.ErrResourceNotFound) {
		handler.forbidden(writer, request)
		return false
	}
	if err != nil {
		handler.logger.Error("load management authorization resource", "resource_type", resourceType, "resource_id", resourceID, "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return false
	}
	if !scope.Allows(resource.OrgUnitID, resource.ResourceID) {
		handler.forbidden(writer, request)
		return false
	}
	return true
}

func (handler *ManagementHandler) forbidden(writer http.ResponseWriter, request *http.Request) {
	httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
}

func (handler *ManagementHandler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, authzapplication.ErrForbidden):
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
	case errors.Is(err, application.ErrValidation):
		handler.validation(writer, request)
	case errors.Is(err, application.ErrMembershipRequired):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	case errors.Is(err, bootstrapadmin.ErrProtected):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.BootstrapAdminProtected)
	case errors.Is(err, bootstrapadmin.ErrConfiguration):
		handler.logger.Error("bootstrap administrator configuration is invalid", "error", err, "path", request.URL.Path)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.BootstrapAdminConfiguration)
	default:
		handler.logger.Error("identity management request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}
func (handler *ManagementHandler) validation(w http.ResponseWriter, r *http.Request) {
	httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
}

func (handler *ManagementHandler) batchValidation(w http.ResponseWriter, r *http.Request, issues ...employeeBatchIssue) {
	httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.New(
		"PLATFORM_BATCH_VALIDATION_ERROR",
		"批量导入参数不合法",
		employeeBatchValidationDetails{Issues: issues},
	))
}
func (handler *ManagementHandler) unauthenticated(w http.ResponseWriter, r *http.Request) {
	httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
}

func decodeManagementRequest(writer http.ResponseWriter, request *http.Request, destination any) bool {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxManagementRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	return ensureSingleJSONValue(decoder) == nil
}
func parsePageRequest(request *http.Request) (application.PageRequest, error) {
	query := request.URL.Query()
	page, err := positiveQuery(query.Get("page"), 1)
	if err != nil {
		return application.PageRequest{}, err
	}
	pageSize, err := positiveQuery(query.Get("page_size"), 20)
	if err != nil || pageSize > 100 {
		return application.PageRequest{}, errors.New("invalid page size")
	}
	status := strings.TrimSpace(query.Get("filter[status]"))
	if status != "" && status != domain.StatusActive && status != domain.StatusDisabled {
		return application.PageRequest{}, errors.New("invalid status")
	}
	return application.PageRequest{Page: page, PageSize: pageSize, Keyword: strings.TrimSpace(query.Get("keyword")), Status: status}, nil
}
func positiveQuery(value string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, errors.New("invalid positive query")
	}
	return parsed, nil
}
func parseDate(value *string) (*time.Time, error) {
	if value == nil || *value == "" {
		return nil, nil
	}
	parsed, err := time.Parse("2006-01-02", *value)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

// parseDateEnd 把"仅日期"的到期值视为包含当天全天的闭区间。授权查询以
// valid_until 与当前时刻比较。
func parseDateEnd(value *string) (*time.Time, error) {
	parsed, err := parseDate(value)
	if err != nil || parsed == nil {
		return parsed, err
	}
	end := parsed.Add(24*time.Hour - time.Nanosecond)
	return &end, nil
}
func dateString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02")
	return &formatted
}
func toUserResponse(value application.UserView) userResponse {
	return userResponse{UserID: value.ID, DisplayName: value.DisplayName, EmployeeNo: value.EmployeeNo, PMSPersonID: value.PMSPersonID, Email: value.Email, MobileMasked: value.MobileMasked, Status: value.Status, ValidUntil: value.ValidUntil, Version: value.Version, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}
func toAccountResponse(value domain.Account) accountResponse {
	response := accountResponse{AccountID: value.ID, UserID: value.UserID, AccountName: value.AccountName, AccountType: value.AccountType, AuthSource: value.AuthSource, PasswordInitialized: value.PasswordInitialized, Status: value.Status, LastLoginAt: value.LastLoginAt, LockedUntil: value.LockedUntil, ValidUntil: value.ValidUntil, Version: value.Version, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
	if value.User != nil {
		response.User = &referenceResponse{ID: value.User.ID, Name: value.User.Name}
	}
	return response
}
func toOrgUnitResponse(value domain.OrgUnit) orgUnitResponse {
	return orgUnitResponse{OrgUnitID: value.ID, ParentID: value.ParentID, Code: value.Code, Name: value.Name, Path: value.Path, Depth: value.Depth, SortOrder: value.SortOrder, Status: value.Status, Version: value.Version}
}
func toPositionResponse(value domain.Position) positionResponse {
	return positionResponse{PositionID: value.ID, OrgUnitID: value.OrgUnitID, Code: value.Code, Name: value.Name, Status: value.Status, Version: value.Version}
}
func toMembershipResponse(value domain.Membership) membershipResponse {
	return membershipResponse{MembershipID: value.ID, User: referenceResponse{ID: value.User.ID, Name: value.User.Name}, OrgUnit: referenceResponse{ID: value.OrgUnit.ID, Name: value.OrgUnit.Name}, Position: referenceResponse{ID: value.Position.ID, Name: value.Position.Name}, MembershipType: value.MembershipType, IsPrimary: value.IsPrimary, EffectiveFrom: dateString(value.EffectiveFrom), EffectiveTo: dateString(value.EffectiveTo), Status: value.Status, InheritAuthorization: value.InheritAuthorization, Version: value.Version}
}
