package identityhttp

import (
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"gorm.io/gorm"
	"net/http"
)

// AuthorizationOverviewHandler returns a compact, read-only IAM overview for one person.
type AuthorizationOverviewHandler struct{ db *gorm.DB }

func NewAuthorizationOverviewHandler(db *gorm.DB) *AuthorizationOverviewHandler {
	return &AuthorizationOverviewHandler{db: db}
}
func (h *AuthorizationOverviewHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	uid := r.PathValue("user_id")
	if uid == "" {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	var user map[string]any
	if err := h.db.WithContext(r.Context()).Table("iam_user").Where("tenant_id=? AND id=?", p.Tenant.ID, uid).Take(&user).Error; err != nil {
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
		return
	}
	out := map[string]any{"user": user}
	out["accounts"] = h.rows(r, "iam_account", "tenant_id=? AND user_id=?", p.Tenant.ID, uid)
	out["memberships"] = h.rows(r, "iam_membership", "tenant_id=? AND user_id=?", p.Tenant.ID, uid)
	out["role_bindings"] = h.rows(r, "authz_role_binding", "tenant_id=? AND subject_type='USER' AND subject_id=?", p.Tenant.ID, uid)
	out["pending_changes"] = h.rows(r, "iam_personnel_change_request", "tenant_id=? AND user_id=? AND status IN ('DRAFT','PENDING_APPROVAL','PENDING_HANDOVER','SCHEDULED')", p.Tenant.ID, uid)
	out["handover"] = h.rows(r, "iam_personnel_handover_item", "tenant_id=? AND current_owner_id=? AND status NOT IN ('COMPLETED','CANCELLED')", p.Tenant.ID, uid)
	var sync []map[string]any
	// Keycloak sync state is emitted when the outbox table is present; a missing optional table is safe.
	h.db.WithContext(r.Context()).Table("keycloak_authorization_outbox").Where("tenant_id=? AND identity_id=?", p.Tenant.ID, uid).Order("created_at DESC").Limit(20).Find(&sync)
	out["keycloak_sync"] = sync
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", out)
}
func (h *AuthorizationOverviewHandler) rows(r *http.Request, table, where string, args ...any) []map[string]any {
	var rows []map[string]any
	h.db.WithContext(r.Context()).Table(table).Where(where, args...).Find(&rows)
	if rows == nil {
		return []map[string]any{}
	}
	return rows
}
