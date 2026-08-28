package http

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// BackchannelLogoutURIHandler 提供 OAuth Client 回调地址的受控登记 API。
type BackchannelLogoutURIHandler struct {
	Repository     application.BackchannelLogoutURIRepository
	AllowLocalHTTP bool
}

// Handle 处理 GET/PUT/DELETE /oauth-clients/{id}/backchannel-logout-uri。
func (h BackchannelLogoutURIHandler) Handle(w http.ResponseWriter, r *http.Request) {
	principal, authenticated := authctx.PrincipalFromContext(r.Context())
	clientID := strings.TrimSpace(r.PathValue("oauth_client_id"))
	tenantID := strings.TrimSpace(principal.Tenant.ID)
	if !authenticated || clientID == "" || tenantID == "" || h.Repository == nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		value, err := h.Repository.Get(r.Context(), tenantID, clientID)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"uri": value})
	case http.MethodPut:
		var body struct {
			URI string `json:"uri"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || application.ValidateBackchannelLogoutURI(body.URI, h.AllowLocalHTTP) != nil {
			http.Error(w, "invalid URI", http.StatusBadRequest)
			return
		}
		if err := h.Repository.Set(r.Context(), application.BackchannelLogoutURIUpdate{TenantID: tenantID, OAuthClientID: clientID, OperatorID: principal.User.ID, URI: strings.TrimSpace(body.URI)}, time.Now().UTC()); err != nil {
			http.Error(w, "unable to save URI", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := h.Repository.Delete(r.Context(), tenantID, clientID); err != nil {
			http.Error(w, "unable to delete URI", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
