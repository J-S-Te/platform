package filetaskhttp

import (
	"context"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type uploadRequestResolver interface {
	ResolveUploadRequest(context.Context, string, string, string) (string, string, error)
}

// ResolveUploadRequest 仅从已验证的机器主体获取租户和应用。
// 响应只暴露稳定标识和状态，绝不返回存储路径。
func (handler *Handler) ResolveUploadRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := handler.principal(w, r)
	if !ok {
		return
	}
	service, ok := handler.files.(uploadRequestResolver)
	if !ok {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.Internal)
		return
	}
	requestID := r.PathValue("request_id")
	fileID, status, err := service.ResolveUploadRequest(r.Context(), p.Tenant.ID, p.Account.ID, requestID)
	result := "SUCCESS"
	if err != nil {
		result = "FAILED"
	}
	handler.auditFileAccess(r.Context(), p.Tenant.ID, p.Account.ID, fileID, p.User.ID, p.SessionID, "UPLOAD_REQUEST_RESOLVED", result, r.Header.Get("X-Request-ID"))
	if err != nil {
		handler.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "上传请求状态已解析", map[string]string{"request_id": requestID, "file_id": fileID, "status": status})
}

func (handler *Handler) RetireUploadRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := handler.principal(w, r)
	if !ok {
		return
	}
	service, ok := handler.files.(interface {
		RetireUploadRequest(context.Context, string, string, string) (string, string, error)
	})
	if !ok {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.Internal)
		return
	}
	requestID := r.PathValue("request_id")
	fileID, status, err := service.RetireUploadRequest(r.Context(), p.Tenant.ID, p.Account.ID, requestID)
	result := "SUCCESS"
	if err != nil {
		result = "FAILED"
	}
	handler.auditFileAccess(r.Context(), p.Tenant.ID, p.Account.ID, fileID, p.User.ID, p.SessionID, "UPLOAD_REQUEST_RETIRED", result, r.Header.Get("X-Request-ID"))
	if err != nil {
		handler.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "上传请求已退休", map[string]any{"request_id": requestID, "file_id": fileID, "status": status, "retired": true})
}
