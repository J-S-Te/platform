// Package http exposes authorization management through the platform JSON envelope.
package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxRequestBytes = 64 << 10

// Handler adapts authorization application operations to HTTP without exposing persistence models.
type Handler struct {
	service *application.Service
	logger  *slog.Logger
}

// NewHandler creates an authorization HTTP handler.
func NewHandler(service *application.Service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("authorization HTTP handler dependencies must not be nil")
	}

	return &Handler{service: service, logger: logger}, nil
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

type referenceResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

type resourceResponse struct {
	ID              string `json:"resource_id"`
	ApplicationCode string `json:"application_code"`
	Code            string `json:"code"`
	Name            string `json:"name"`
	ResourceType    string `json:"resource_type"`
	Version         uint64 `json:"version"`
}

type permissionResponse struct {
	ID       string            `json:"permission_id"`
	Code     string            `json:"code"`
	Name     string            `json:"name"`
	Action   string            `json:"action"`
	Resource referenceResponse `json:"resource"`
	Version  uint64            `json:"version"`
}

type roleResponse struct {
	ID          string              `json:"role_id"`
	Code        string              `json:"code"`
	Name        string              `json:"name"`
	Description *string             `json:"description,omitempty"`
	Status      string              `json:"status"`
	BuiltIn     bool                `json:"built_in"`
	Permissions []referenceResponse `json:"permissions"`
	Version     uint64              `json:"version"`
}

type roleBindingResponse struct {
	ID          string            `json:"binding_id"`
	Role        referenceResponse `json:"role"`
	SubjectType string            `json:"subject_type"`
	Subject     referenceResponse `json:"subject"`
	ScopeType   string            `json:"scope_type"`
	ScopeID     *string           `json:"scope_id,omitempty"`
	Status      string            `json:"status"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	Version     uint64            `json:"version"`
}

type decisionResponse struct {
	Allowed        bool   `json:"allowed"`
	PermissionCode string `json:"permission_code"`
	PolicyVersion  uint64 `json:"policy_version"`
	ReasonCode     string `json:"reason_code"`
}

type accessSourceResponse struct {
	BindingID   string            `json:"binding_id"`
	SubjectType string            `json:"subject_type"`
	Subject     referenceResponse `json:"subject"`
	ScopeType   string            `json:"scope_type"`
	ScopeID     *string           `json:"scope_id,omitempty"`
}

type effectiveRoleResponse struct {
	Role    referenceResponse      `json:"role"`
	Sources []accessSourceResponse `json:"sources"`
}

type effectivePermissionResponse struct {
	Permission permissionResponse     `json:"permission"`
	Sources    []accessSourceResponse `json:"sources"`
}

type effectiveAccessPreviewResponse struct {
	User                      referenceResponse             `json:"user"`
	Account                   referenceResponse             `json:"account"`
	LoginEligible             bool                          `json:"login_eligible"`
	PolicyVersion             uint64                        `json:"policy_version"`
	GeneratedAt               time.Time                     `json:"generated_at"`
	Roles                     []effectiveRoleResponse       `json:"roles"`
	Permissions               []effectivePermissionResponse `json:"permissions"`
	ExternalIdentityProviders []referenceResponse           `json:"external_identity_providers"`
	ExternalIdentityNote      string                        `json:"external_identity_note"`
}

type roleBindingImpactResponse struct {
	Role               referenceResponse   `json:"role"`
	Permissions        []referenceResponse `json:"permissions"`
	SubjectType        string              `json:"subject_type"`
	Subject            referenceResponse   `json:"subject"`
	ScopeType          string              `json:"scope_type"`
	ScopeID            *string             `json:"scope_id,omitempty"`
	ExpiresAt          *time.Time          `json:"expires_at,omitempty"`
	TotalAffectedUsers int64               `json:"total_affected_users"`
	Users              []referenceResponse `json:"users"`
	Truncated          bool                `json:"truncated"`
	GeneratedAt        time.Time           `json:"generated_at"`
}

type resourceCreatePayload struct {
	ApplicationCode string `json:"application_code"`
	Code            string `json:"code"`
	Name            string `json:"name"`
	ResourceType    string `json:"resource_type"`
}

type permissionCreatePayload struct {
	ResourceID string `json:"resource_id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	Action     string `json:"action"`
}

type roleCreatePayload struct {
	// Code is accepted only for rolling compatibility and is ignored.
	Code          *string  `json:"code,omitempty"`
	Name          string   `json:"name"`
	Description   *string  `json:"description"`
	PermissionIDs []string `json:"permission_ids"`
}

type roleUpdatePayload struct {
	roleCreatePayload
	Status  string `json:"status"`
	Version uint64 `json:"version"`
}

type roleBindingPayload struct {
	RoleID      string     `json:"role_id"`
	SubjectType string     `json:"subject_type"`
	SubjectID   string     `json:"subject_id"`
	ScopeType   string     `json:"scope_type"`
	ScopeID     *string    `json:"scope_id"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Version     uint64     `json:"version"`
}

type checkPayload struct {
	PermissionCode string         `json:"permission_code"`
	ResourceType   string         `json:"resource_type"`
	ResourceID     string         `json:"resource_id"`
	Context        map[string]any `json:"context"`
}

type batchCheckPayload struct {
	Requests []checkPayload `json:"requests"`
}

type roleBindingImpactPayload struct {
	RoleID      string     `json:"role_id"`
	SubjectType string     `json:"subject_type"`
	SubjectID   string     `json:"subject_id"`
	ScopeType   string     `json:"scope_type"`
	ScopeID     *string    `json:"scope_id"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// ListResources returns resources available in the current tenant.
func (h *Handler) ListResources(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	result, err := h.service.ListResources(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	items := make([]resourceResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, resourceToResponse(item))
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", newPageResponse(items, result))
}

// CreateResource creates a resource for an enabled application in the current tenant.
func (h *Handler) CreateResource(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload resourceCreatePayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.service.CreateResource(r.Context(), application.ResourceCreateInput{
		TenantID:        principal.Tenant.ID,
		OperatorID:      principal.User.ID,
		ApplicationCode: payload.ApplicationCode,
		Code:            payload.Code,
		Name:            payload.Name,
		ResourceType:    payload.ResourceType,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusCreated, "资源已创建", resourceToResponse(result))
}

// ListPermissions returns permissions available in the current tenant.
func (h *Handler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	result, err := h.service.ListPermissions(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	items := make([]permissionResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, permissionToResponse(item))
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", newPageResponse(items, result))
}

// CreatePermission creates a permission attached to a tenant resource.
func (h *Handler) CreatePermission(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload permissionCreatePayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.service.CreatePermission(r.Context(), application.PermissionCreateInput{
		TenantID:   principal.Tenant.ID,
		OperatorID: principal.User.ID,
		ResourceID: payload.ResourceID,
		Code:       payload.Code,
		Name:       payload.Name,
		Action:     payload.Action,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusCreated, "权限已创建", permissionToResponse(result))
}

// ListRoles returns roles available in the current tenant.
func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	result, err := h.service.ListRoles(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	items := make([]roleResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, roleToResponse(item))
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", newPageResponse(items, result))
}

// HTTP 层只传递经过认证的操作者身份和请求权限集合；自定义角色是否仅包含操作者可委派
// 权限、是否触及受保护权限，必须由应用服务按当前数据库状态再次校验。
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload roleCreatePayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.service.CreateRole(r.Context(), application.RoleCreateInput{
		TenantID:          principal.Tenant.ID,
		OperatorID:        principal.User.ID,
		OperatorAccountID: principal.Account.ID,
		Name:              payload.Name,
		Description:       payload.Description,
		PermissionIDs:     payload.PermissionIDs,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusCreated, "角色已创建", roleToResponse(result))
}

// GetRole returns one tenant role and its allow-permission set.
func (h *Handler) GetRole(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	result, err := h.service.GetRole(r.Context(), principal.Tenant.ID, r.PathValue("role_id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", roleToResponse(result))
}

// 更新携带版本号防止覆盖并发编辑；内置角色保护和新权限集合的可委派性仍由应用服务
// 判断，不能仅依赖路由上的“角色更新”权限。
func (h *Handler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload roleUpdatePayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.service.UpdateRole(r.Context(), application.RoleUpdateInput{
		TenantID:          principal.Tenant.ID,
		OperatorID:        principal.User.ID,
		OperatorAccountID: principal.Account.ID,
		RoleID:            r.PathValue("role_id"),
		Name:              payload.Name,
		Description:       payload.Description,
		Status:            payload.Status,
		PermissionIDs:     payload.PermissionIDs,
		Version:           payload.Version,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "角色已更新", roleToResponse(result))
}

// ListRoleBindings returns role bindings in the current tenant.
func (h *Handler) ListRoleBindings(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	result, err := h.service.ListRoleBindings(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	items := make([]roleBindingResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, roleBindingToResponse(item))
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", newPageResponse(items, result))
}

// 绑定接口不会信任请求中的角色、主体或 scope 声明；应用服务会校验租户归属、受保护
// 角色、操作者可委派资格及资源范围，HTTP 层只注入可信的当前用户作为操作者。
func (h *Handler) CreateRoleBinding(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload roleBindingPayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.service.CreateRoleBinding(r.Context(), application.RoleBindingCreateInput{
		TenantID:    principal.Tenant.ID,
		OperatorID:  principal.User.ID,
		RoleID:      payload.RoleID,
		SubjectType: payload.SubjectType,
		SubjectID:   payload.SubjectID,
		ScopeType:   payload.ScopeType,
		ScopeID:     payload.ScopeID,
		ExpiresAt:   payload.ExpiresAt,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusCreated, "角色绑定已创建", roleBindingToResponse(result))
}

// 更新可能同时替换角色、主体和范围，因此必须按“新绑定”重新执行完整委派检查；版本号
// 仅解决并发覆盖，不能代替高权角色和资源范围校验。
func (h *Handler) UpdateRoleBinding(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload roleBindingPayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.service.UpdateRoleBinding(r.Context(), application.RoleBindingUpdateInput{
		TenantID:    principal.Tenant.ID,
		OperatorID:  principal.User.ID,
		BindingID:   r.PathValue("binding_id"),
		RoleID:      payload.RoleID,
		SubjectType: payload.SubjectType,
		SubjectID:   payload.SubjectID,
		ScopeType:   payload.ScopeType,
		ScopeID:     payload.ScopeID,
		Status:      payload.Status,
		ExpiresAt:   payload.ExpiresAt,
		Version:     payload.Version,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "角色绑定已更新", roleBindingToResponse(result))
}

// PreviewEffectiveAccess explains active role and permission sources for one login account.
func (h *Handler) PreviewEffectiveAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	result, err := h.service.PreviewEffectiveAccess(r.Context(), principal.Tenant.ID, strings.TrimSpace(r.URL.Query().Get("user_id")), strings.TrimSpace(r.URL.Query().Get("account_id")))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", effectiveAccessPreviewToResponse(result))
}

// PreviewRoleBindingImpact calculates the currently affected active users before a binding is saved.
func (h *Handler) PreviewRoleBindingImpact(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var payload roleBindingImpactPayload
	if !decode(w, r, &payload) {
		return
	}
	result, err := h.service.PreviewRoleBindingImpact(r.Context(), application.RoleBindingImpactInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, RoleID: payload.RoleID,
		SubjectType: payload.SubjectType, SubjectID: payload.SubjectID, ScopeType: payload.ScopeType,
		ScopeID: payload.ScopeID, ExpiresAt: payload.ExpiresAt,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", roleBindingImpactToResponse(result))
}

// 权限决策使用会话中的可信用户/账号作为主体，同时把资源类型和 ID 交给策略服务解析；
// 客户端 context 只能提供策略输入，不能自行证明资源归属或扩大授权范围。
func (h *Handler) Check(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload checkPayload
	if !decode(w, r, &payload) {
		return
	}

	result, err := h.check(r, principal, payload)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", decisionToResponse(result))
}

// BatchCheck evaluates up to one hundred authorization checks in input order.
func (h *Handler) BatchCheck(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}

	var payload batchCheckPayload
	if !decode(w, r, &payload) {
		return
	}
	if len(payload.Requests) == 0 || len(payload.Requests) > 100 {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}

	decisions := make([]decisionResponse, 0, len(payload.Requests))
	for _, request := range payload.Requests {
		result, err := h.check(r, principal, request)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		decisions = append(decisions, decisionToResponse(result))
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", decisions)
}

func (h *Handler) check(r *http.Request, principal authctx.Principal, payload checkPayload) (domain.Decision, error) {
	return h.service.Check(r.Context(), application.CheckInput{
		TenantID:       principal.Tenant.ID,
		UserID:         principal.User.ID,
		AccountID:      principal.Account.ID,
		PermissionCode: payload.PermissionCode,
		ResourceType:   payload.ResourceType,
		ResourceID:     payload.ResourceID,
		Context:        payload.Context,
	})
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	}

	return principal, ok
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrForbidden):
		httpresponse.WriteError(w, r, http.StatusForbidden, httperror.Forbidden)
	case errors.Is(err, application.ErrVersionConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.VersionConflict)
	default:
		h.logger.Error("authorization HTTP operation failed", "error", err, "path", r.URL.Path)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
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

func pageQuery(r *http.Request) application.PageRequest {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	return application.PageRequest{
		Page:     page,
		PageSize: pageSize,
		Keyword:  strings.TrimSpace(r.URL.Query().Get("keyword")),
		Status:   strings.TrimSpace(r.URL.Query().Get("filter[status]")),
	}
}

func newPageResponse[Response any, Domain any](
	items []Response,
	result application.PageResult[Domain],
) pageResponse[Response] {
	return pageResponse[Response]{
		Items:    items,
		Page:     result.Page,
		PageSize: result.PageSize,
		Total:    result.Total,
	}
}

func referenceToResponse(value domain.Reference) referenceResponse {
	return referenceResponse{ID: value.ID, Name: value.Name, Code: value.Code}
}

func resourceToResponse(value domain.Resource) resourceResponse {
	return resourceResponse{
		ID:              value.ID,
		ApplicationCode: value.ApplicationCode,
		Code:            value.Code,
		Name:            value.Name,
		ResourceType:    value.ResourceType,
		Version:         value.Version,
	}
}

func permissionToResponse(value domain.Permission) permissionResponse {
	return permissionResponse{
		ID:       value.ID,
		Code:     value.Code,
		Name:     value.Name,
		Action:   value.Action,
		Resource: referenceToResponse(value.Resource),
		Version:  value.Version,
	}
}

func roleToResponse(value domain.Role) roleResponse {
	permissions := make([]referenceResponse, 0, len(value.Permissions))
	for _, permission := range value.Permissions {
		permissions = append(permissions, referenceToResponse(permission))
	}

	return roleResponse{
		ID:          value.ID,
		Code:        value.Code,
		Name:        value.Name,
		Description: value.Description,
		Status:      value.Status,
		BuiltIn:     value.BuiltIn,
		Permissions: permissions,
		Version:     value.Version,
	}
}

func roleBindingToResponse(value domain.RoleBinding) roleBindingResponse {
	return roleBindingResponse{
		ID:          value.ID,
		Role:        referenceToResponse(value.Role),
		SubjectType: value.SubjectType,
		Subject:     referenceToResponse(value.Subject),
		ScopeType:   value.ScopeType,
		ScopeID:     value.ScopeID,
		Status:      value.Status,
		ExpiresAt:   value.ExpiresAt,
		Version:     value.Version,
	}
}

func decisionToResponse(value domain.Decision) decisionResponse {
	return decisionResponse{
		Allowed:        value.Allowed,
		PermissionCode: value.PermissionCode,
		PolicyVersion:  value.PolicyVersion,
		ReasonCode:     value.ReasonCode,
	}
}

func accessSourceToResponse(value domain.AccessSource) accessSourceResponse {
	return accessSourceResponse{BindingID: value.BindingID, SubjectType: value.SubjectType, Subject: referenceToResponse(value.Subject), ScopeType: value.ScopeType, ScopeID: value.ScopeID}
}

func effectiveAccessPreviewToResponse(value domain.EffectiveAccessPreview) effectiveAccessPreviewResponse {
	roles := make([]effectiveRoleResponse, 0, len(value.Roles))
	for _, role := range value.Roles {
		sources := make([]accessSourceResponse, 0, len(role.Sources))
		for _, source := range role.Sources {
			sources = append(sources, accessSourceToResponse(source))
		}
		roles = append(roles, effectiveRoleResponse{Role: referenceToResponse(role.Role), Sources: sources})
	}
	permissions := make([]effectivePermissionResponse, 0, len(value.Permissions))
	for _, permission := range value.Permissions {
		sources := make([]accessSourceResponse, 0, len(permission.Sources))
		for _, source := range permission.Sources {
			sources = append(sources, accessSourceToResponse(source))
		}
		permissions = append(permissions, effectivePermissionResponse{Permission: permissionToResponse(permission.Permission), Sources: sources})
	}
	providers := make([]referenceResponse, 0, len(value.ExternalIdentityProviders))
	for _, provider := range value.ExternalIdentityProviders {
		providers = append(providers, referenceToResponse(provider))
	}
	return effectiveAccessPreviewResponse{User: referenceToResponse(value.User), Account: referenceToResponse(value.Account), LoginEligible: value.LoginEligible, PolicyVersion: value.PolicyVersion, GeneratedAt: value.GeneratedAt, Roles: roles, Permissions: permissions, ExternalIdentityProviders: providers, ExternalIdentityNote: "外部身份仅用于映射到本地用户，不直接授予角色或权限。"}
}

func roleBindingImpactToResponse(value domain.RoleBindingImpactPreview) roleBindingImpactResponse {
	permissions := make([]referenceResponse, 0, len(value.Permissions))
	for _, permission := range value.Permissions {
		permissions = append(permissions, referenceToResponse(permission))
	}
	users := make([]referenceResponse, 0, len(value.Users))
	for _, user := range value.Users {
		users = append(users, referenceToResponse(user))
	}
	return roleBindingImpactResponse{Role: referenceToResponse(value.Role), Permissions: permissions, SubjectType: value.SubjectType, Subject: referenceToResponse(value.Subject), ScopeType: value.ScopeType, ScopeID: value.ScopeID, ExpiresAt: value.ExpiresAt, TotalAffectedUsers: value.TotalAffectedUsers, Users: users, Truncated: value.Truncated, GeneratedAt: value.GeneratedAt}
}
