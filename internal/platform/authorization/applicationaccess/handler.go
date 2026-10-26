package applicationaccess

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	authapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxAccessRequestBytes = 32 << 10

// Handler 提供应用访问授权与授权目录同步的 HTTP 入口。
// 所有操作以会话主体所属租户为边界；授权目录写操作仅接受已验证的应用 Bearer 凭据。
// strictRevision 是方案兼容发布第 2 步的开关：环境变量
// IAM_APPLICATION_ACCESS_REVISION_STRICT 置为 1/true/yes 时，写接口缺省
// expected_revision 一律 422（全部调用方传条件后才允许开启，开启后不得为回退
// 重新开放无条件覆盖）。
type Handler struct {
	service        *Service
	strictRevision bool
}

// strictRevisionEnvironmentVariable 是严格模式的部署配置项。
const strictRevisionEnvironmentVariable = "IAM_APPLICATION_ACCESS_REVISION_STRICT"

func strictRevisionFromEnvironment(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// NewHandler 构造 HTTP 适配器；service 为空时返回错误。
func NewHandler(service *Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("application authorization HTTP handler service must not be nil")
	}
	return &Handler{service: service, strictRevision: strictRevisionFromEnvironment(os.Getenv(strictRevisionEnvironmentVariable))}, nil
}

type rolePayload struct {
	RoleCode        string     `json:"role_code"`
	ScopeType       string     `json:"scope_type"`
	EnvironmentCode string     `json:"environment_code"`
	ValidFrom       *time.Time `json:"valid_from"`
	ValidUntil      *time.Time `json:"valid_until"`
}

type updatePayload struct {
	Roles             *[]rolePayload `json:"roles"`
	RoleCode          string         `json:"role_code"`
	CustomPermissions *[]string      `json:"custom_permissions"`
	// ExpectedRevision 是整组保存的乐观并发条件（GET 响应的 edit_revision）。
	// 兼容发布第一阶段允许缺省；携带但陈旧返回 409 APPLICATION_ACCESS_REVISION_CONFLICT，
	// 格式非法返回 422。该字段是状态摘要，不承担认证或授权能力。
	ExpectedRevision string `json:"expected_revision"`
}

// GetAccess 返回某用户在指定应用上的有效授权视图（含直绑、继承与有效权限）。
// 未配置授权时返回空视图而非 404，便于管理界面直接编辑。
func (h *Handler) GetAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	access, err := h.service.GetAccess(r.Context(), principal.Tenant.ID, strings.TrimSpace(r.PathValue("user_id")), strings.TrimSpace(r.PathValue("application_code")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", access)
}

// UpdateAccess 替换用户的"人工直绑"应用角色。部分更新语义：未提供 roles 时
// 不修改角色；提供 custom_permissions 会被拒绝，权限只能来自应用目录的角色映射。
func (h *Handler) UpdateAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload updatePayload
	if !decodePayload(w, r, &payload) {
		return
	}
	roles, provided, err := payloadRoles(payload)
	if err != nil {
		writeError(w, r, err)
		return
	}
	access, err := h.service.UpdateAccess(r.Context(), UpdateAccessInput{
		TenantID: principal.Tenant.ID, UserID: strings.TrimSpace(r.PathValue("user_id")), OperatorID: principal.User.ID, OperatorName: principal.User.Name,
		Roles: roles, RolesProvided: provided,
		CustomPermissions: valueOrEmpty(payload.CustomPermissions), CustomPermissionsProvided: payload.CustomPermissions != nil,
		ExpectedRevision: payload.ExpectedRevision, StrictRevisionRequired: h.strictRevision,
	}, strings.TrimSpace(r.PathValue("application_code")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "应用权限已更新", access)
}

// GetSubjectAccess 返回组织单元或岗位上的直接应用角色绑定。与 GetAccess 不同，
// 本接口不展开成员关系——被查询的主体本身就是授权管理对象。
func (h *Handler) GetSubjectAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	subjectType := strings.TrimSpace(r.PathValue("subject_type"))
	subjectID := strings.TrimSpace(r.PathValue("subject_id"))
	applicationCode := strings.TrimSpace(r.PathValue("application_code"))
	access, err := h.service.GetSubjectAccess(r.Context(), principal.Tenant.ID, subjectType, subjectID, applicationCode)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", access)
}

// UpdateSubjectAccess 兼容保留：标准人事授权已改由岗位授权模板物化，本入口
// 恒定拒绝手写组织/岗位绑定（原因见 Service.UpdateSubjectAccess）。
func (h *Handler) UpdateSubjectAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload updatePayload
	if !decodePayload(w, r, &payload) {
		return
	}
	if payload.CustomPermissions != nil {
		writeError(w, r, validation("custom_permissions are not supported; assign an application role instead"))
		return
	}
	roles, provided, err := payloadRoles(payload)
	if err != nil {
		writeError(w, r, err)
		return
	}
	access, err := h.service.UpdateSubjectAccess(r.Context(), UpdateSubjectAccessInput{
		TenantID: principal.Tenant.ID, SubjectType: strings.TrimSpace(r.PathValue("subject_type")),
		SubjectID: strings.TrimSpace(r.PathValue("subject_id")), OperatorID: principal.User.ID, OperatorName: principal.User.Name,
		Roles: roles, RolesProvided: provided,
	}, strings.TrimSpace(r.PathValue("application_code")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "主体应用权限已更新", access)
}

// DeleteSubjectAccess 仅停用所选组织/岗位的直接应用角色绑定，并对受影响的
// 成员触发 Keycloak 授权重投影。
func (h *Handler) DeleteSubjectAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	subjectType := strings.TrimSpace(r.PathValue("subject_type"))
	subjectID := strings.TrimSpace(r.PathValue("subject_id"))
	applicationCode := strings.TrimSpace(r.PathValue("application_code"))
	err := h.service.DeleteSubjectAccess(r.Context(), DeleteSubjectAccessInput{
		TenantID: principal.Tenant.ID, SubjectType: subjectType, SubjectID: subjectID, OperatorID: principal.User.ID, OperatorName: principal.User.Name,
	}, applicationCode)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "主体直接授权已撤销", map[string]any{
		"application_code": applicationCode, "subject_type": subjectType, "subject_id": subjectID,
	})
}

// DeleteAccess 逻辑撤销用户在指定应用上的全部人工授权；继承角色不受影响。
func (h *Handler) DeleteAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	err := h.service.DeleteAccess(r.Context(), DeleteAccessInput{
		TenantID: principal.Tenant.ID, UserID: strings.TrimSpace(r.PathValue("user_id")), OperatorID: principal.User.ID, OperatorName: principal.User.Name,
		// DELETE 的并发条件经查询参数承载，避免依赖 DELETE body；参数只是状态摘要。
		ExpectedRevision: r.URL.Query().Get("expected_revision"), StrictRevisionRequired: h.strictRevision,
	}, strings.TrimSpace(r.PathValue("application_code")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "应用访问已撤销", map[string]any{
		"application_code": strings.TrimSpace(r.PathValue("application_code")), "user_id": strings.TrimSpace(r.PathValue("user_id")),
	})
}

// payloadRoles 归一化角色提交载荷：roles 数组优先；单 role_code 视为应用级直绑。
// 载荷同时携带 custom_permissions 时故意返回空角色（不报错），让请求因不支持
// 的字段被拒绝，而不是被误报为"清空全部角色"。
func payloadRoles(payload updatePayload) ([]RoleInput, bool, error) {
	if payload.Roles != nil {
		roles := make([]RoleInput, 0, len(*payload.Roles))
		for _, role := range *payload.Roles {
			roles = append(roles, RoleInput{RoleCode: role.RoleCode, ScopeType: role.ScopeType, EnvironmentCode: role.EnvironmentCode, ValidFrom: role.ValidFrom, ValidUntil: role.ValidUntil})
		}
		return roles, true, nil
	}
	if strings.TrimSpace(payload.RoleCode) != "" {
		return []RoleInput{{RoleCode: payload.RoleCode, ScopeType: "APPLICATION"}}, true, nil
	}
	if payload.CustomPermissions != nil {
		// 服务层拒绝直接权限更新。此处刻意保持"有角色载荷"的返回值，
		// 让请求因不支持的字段被拒绝，而不是被误报为"清空全部角色"。
		return nil, false, nil
	}
	return nil, false, validation("roles is required")
}

func hasPermission(principal authctx.Principal, permissionCode string) bool {
	for _, code := range principal.PermissionCodes {
		if code == permissionCode {
			return true
		}
	}
	return false
}

func valueOrEmpty(values *[]string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), (*values)...)
}

func decodePayload(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAccessRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		// 校验消息刻意限定为静态、对客户端安全的契约解释（如角色不可用、scope 非法）。
		// 返回 detail 可让 IAM 前端区分"目录已过期"与"选择非法"，
		// 同时保证基础设施错误与凭据不外泄到响应中。
		detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrValidation.Error()+": "))
		apiError := httperror.Validation
		if detail != "" {
			apiError = httperror.New(apiError.Code, apiError.Message, map[string]string{"reason": detail})
		}
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, apiError)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotConfigured):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, ErrAccessDenied), errors.Is(err, authapplication.ErrForbidden):
		httpresponse.WriteError(w, r, http.StatusForbidden, httperror.Forbidden)
	case errors.Is(err, ErrRevisionConflict):
		// 稳定错误标识供前端区分"并发冲突（保留草稿，提示重新读取）"与其他失败；
		// 前端不得自动换用最新版本重提。
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.New(
			"APPLICATION_ACCESS_REVISION_CONFLICT",
			"应用授权已被其他管理员修改，请刷新读取最新授权后重试。",
			nil,
		))
	case errors.Is(err, ErrAccessReadback):
		// 写事务已提交；不得让调用方误以为写失败并重复提交。
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.New(
			"APPLICATION_ACCESS_READBACK_FAILED",
			"授权已保存，但最新结果读取失败，请刷新核对，勿重复提交。",
			map[string]string{"write_committed": "true", "next_action": "refresh"},
		))
	default:
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}

// GetCatalog 返回指定应用最近一次同步的授权目录与平台侧同步状态。
func (h *Handler) GetCatalog(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("application_id"))
	tenantID, _, err := h.catalogPrincipal(r, applicationID, "authorization.catalog.read", "platform:application:read")
	if err != nil {
		writeError(w, r, err)
		return
	}
	catalog, err := h.service.GetCatalog(r.Context(), tenantID, applicationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", catalog)
}

// SyncCatalog 校验并原子替换应用自有的授权目录。目录来源以已验证的应用 Bearer
// 为准；请求体中的来源声明仅作兼容解码，入库前被覆盖（见方法内的行内说明）。
func (h *Handler) SyncCatalog(w http.ResponseWriter, r *http.Request) {
	applicationID := strings.TrimSpace(r.PathValue("application_id"))
	principal, err := catalogSyncApplicationPrincipal(r, applicationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var payload CatalogInput
	if !decodePayload(w, r, &payload) {
		return
	}
	// 目录归属以已验证的应用 Bearer token 为准。绝不信任调用方提供的来源字段：
	// 这些字段仅为向后兼容的请求解码而保留，入库前一律被覆盖。
	payload.SourceType, payload.SourceIdentifier = catalogSourceFromApplicationPrincipal(principal)
	// 操作者持久化为 OAuth client 的平台侧主键（26 位 ULID），确保能完整放入
	// CHAR(26) 的 created_by/updated_by 列而不被截断；人类可读的 client_id
	// 另存于目录的 source_identifier 列。
	catalog, err := h.service.SyncCatalog(r.Context(), principal.TenantID, applicationID, principal.OAuthClientID, payload)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "应用授权目录已同步", catalog)
}

// catalogSyncApplicationPrincipal 校验目录写入权限。授权目录是应用自有的声明，
// 因此平台控制台身份（包括管理员或应用属主）刻意永远不足以完成写入；
// 必须由绑定到目标应用、且显式携带 authorization.catalog.sync scope 的
// 应用 Bearer 才能同步。
func catalogSyncApplicationPrincipal(r *http.Request, applicationID string) (appctx.Principal, error) {
	principal, ok := appctx.PrincipalFromContext(r.Context())
	if !ok || principal.ApplicationID != applicationID || !principal.HasScope("authorization.catalog.sync") {
		return appctx.Principal{}, ErrAccessDenied
	}
	return principal, nil
}

func catalogSourceFromApplicationPrincipal(principal appctx.Principal) (sourceType, sourceIdentifier string) {
	return "APPLICATION", "oauth_client:" + principal.OAuthClientID
}

// catalogPrincipal 授权目录读取：平台控制台用户（具备指定权限或为应用属主）
// 与持有该目录的 OAuth 应用（携带指定 scope）二选一。写入走
// catalogSyncApplicationPrincipal，不接受控制台身份。
func (h *Handler) catalogPrincipal(r *http.Request, applicationID, applicationScope, consolePermission string) (tenantID, operatorID string, err error) {
	if principal, ok := authctx.PrincipalFromContext(r.Context()); ok {
		if hasPermission(principal, consolePermission) {
			return principal.Tenant.ID, principal.User.ID, nil
		}
		isOwner, ownerErr := h.service.IsApplicationOwner(r.Context(), principal.Tenant.ID, applicationID, principal.User.ID)
		if ownerErr != nil {
			return "", "", ownerErr
		}
		if !isOwner {
			return "", "", ErrAccessDenied
		}
		return principal.Tenant.ID, principal.User.ID, nil
	}
	if principal, ok := appctx.PrincipalFromContext(r.Context()); ok {
		if principal.ApplicationID != applicationID || !principal.HasScope(applicationScope) {
			return "", "", ErrAccessDenied
		}
		return principal.TenantID, "application:" + principal.ClientID, nil
	}
	return "", "", ErrAccessDenied
}
