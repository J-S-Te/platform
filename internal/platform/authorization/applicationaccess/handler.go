package applicationaccess

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxAccessRequestBytes = 32 << 10

type Handler struct{ service *Service }

func NewHandler(service *Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("application authorization HTTP handler service must not be nil")
	}
	return &Handler{service: service}, nil
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
}

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
	}, strings.TrimSpace(r.PathValue("application_code")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "应用权限已更新", access)
}

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

func (h *Handler) DeleteAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	err := h.service.DeleteAccess(r.Context(), DeleteAccessInput{
		TenantID: principal.Tenant.ID, UserID: strings.TrimSpace(r.PathValue("user_id")), OperatorID: principal.User.ID, OperatorName: principal.User.Name,
	}, strings.TrimSpace(r.PathValue("application_code")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "应用访问已撤销", map[string]any{
		"application_code": strings.TrimSpace(r.PathValue("application_code")), "user_id": strings.TrimSpace(r.PathValue("user_id")),
	})
}

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
		// The service rejects direct permission updates. Preserve roles here so
		// the request is rejected for its unsupported field rather than being
		// misreported as an empty role replacement.
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
		// Validation messages are deliberately limited to static, client-safe contract
		// explanations (for example, an unavailable role or invalid scope). Returning the
		// detail lets the IAM UI distinguish a stale catalog from a malformed selection while
		// keeping infrastructure errors and credentials out of the response.
		detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrValidation.Error()+": "))
		apiError := httperror.Validation
		if detail != "" {
			apiError = httperror.New(apiError.Code, apiError.Message, map[string]string{"reason": detail})
		}
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, apiError)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotConfigured):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, ErrAccessDenied):
		httpresponse.WriteError(w, r, http.StatusForbidden, httperror.Forbidden)
	default:
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}

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
	// The catalog owner is established from the verified application bearer token.
	// Never trust caller-provided provenance fields: they are accepted only for
	// backwards-compatible request decoding and are overwritten before persistence.
	payload.SourceType, payload.SourceIdentifier = catalogSourceFromApplicationPrincipal(principal)
	// Persist the operator as the OAuth client's platform-side primary key (a 26-char ULID) so it
	// fits the CHAR(26) created_by/updated_by columns without truncation. The human-readable
	// client_id is preserved separately in the catalog's source_identifier column.
	catalog, err := h.service.SyncCatalog(r.Context(), principal.TenantID, applicationID, principal.OAuthClientID, payload)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "应用授权目录已同步", catalog)
}

// catalogSyncApplicationPrincipal authorizes writes to an authorization catalog. Catalogs are
// application-owned declarations, so a platform console principal (including an administrator or
// application owner) is deliberately never sufficient. The application bearer must be bound to
// the target application and explicitly carry authorization.catalog.sync.
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

// catalogPrincipal authorizes read access for either a platform console user or the OAuth
// application that owns the catalog. Writes use catalogSyncApplicationPrincipal instead.
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
