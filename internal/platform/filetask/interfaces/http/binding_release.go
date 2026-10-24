package filetaskhttp

import (
	"context"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type bindingReleaser interface {
	ReleaseResourceBinding(context.Context, application.BindingInput) error
}
type releaseBindingPayload struct {
	ApplicationID string `json:"application_id"`
	ResourceType  string `json:"resource_type"`
	ResourceID    string `json:"resource_id"`
	BindingType   string `json:"binding_type"`
}

// ReleaseFileBinding 退役应用拥有的指定引用，不删除文件
// 内容或其他消费者的引用。持久化标记也会拒绝迟到的 Bind。
func (handler *Handler) ReleaseFileBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var payload releaseBindingPayload
	if !decodeJSON(w, r, &payload) {
		return
	}
	if strings.TrimSpace(payload.ApplicationID) == "" || strings.TrimSpace(payload.ApplicationID) != p.Account.ID {
		httpresponse.WriteError(w, r, http.StatusForbidden, httperror.Forbidden)
		return
	}
	service, ok := handler.files.(bindingReleaser)
	if !ok {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.Internal)
		return
	}
	input := application.BindingInput{TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, FileID: r.PathValue("file_id"), ResourceType: payload.ResourceType, ResourceID: payload.ResourceID, BindingType: payload.BindingType, OperatorUserID: bindingOperator(p)}
	err := service.ReleaseResourceBinding(r.Context(), input)
	result := "SUCCESS"
	if err != nil {
		result = "FAILED"
	}
	handler.auditFileAccess(r.Context(), p.Tenant.ID, p.Account.ID, input.FileID, p.User.ID, p.SessionID, "BINDING_RELEASED", result, r.Header.Get("X-Request-ID"))
	if err != nil {
		handler.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "文件业务引用已释放", map[string]any{"released": true})
}
