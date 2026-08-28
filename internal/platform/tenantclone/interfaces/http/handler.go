// Package tenantclonehttp 将新租户授权目录克隆用例适配为受认证的管理 API。
package tenantclonehttp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type cloneService interface {
	Clone(context.Context, tenantclone.Input) (tenantclone.Result, error)
}

// Handler 仅允许从当前登录租户向已存在的目标租户克隆静态授权目录。
// 源租户不从请求体接收，避免调用方越权读取任意租户的权限结构。
type Handler struct {
	service cloneService
}

// NewHandler 创建新租户授权目录克隆 HTTP 适配器。
func NewHandler(service cloneService) (*Handler, error) {
	if service == nil {
		return nil, errors.New("tenant clone service must not be nil")
	}
	return &Handler{service: service}, nil
}

// CloneAuthorizationCatalog 处理授权目录克隆请求；目标租户来自受保护路由参数，完整操作以
// Idempotency-Key 幂等。返回值只包含目录条目计数，不暴露用户、绑定或 OAuth 凭据。
func (handler *Handler) CloneAuthorizationCatalog(writer http.ResponseWriter, request *http.Request) {
	principal, authenticated := authctx.PrincipalFromContext(request.Context())
	targetTenantID := strings.TrimSpace(request.PathValue("tenant_id"))
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if !authenticated || strings.TrimSpace(principal.Tenant.ID) == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Error{Code: "AUTH_REQUIRED", Message: "需要登录后执行此操作"})
		return
	}
	if targetTenantID == "" || idempotencyKey == "" {
		httpresponse.WriteError(writer, request, http.StatusBadRequest, httperror.Error{Code: "TENANT_CLONE_INVALID_REQUEST", Message: "目标租户和 Idempotency-Key 不能为空"})
		return
	}

	result, err := handler.service.Clone(request.Context(), tenantclone.Input{
		SourceTenantID: principal.Tenant.ID,
		TargetTenantID: targetTenantID,
		IdempotencyKey: idempotencyKey,
		OperatorID:     principal.User.ID,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "新租户授权目录克隆完成", result)
}

func (handler *Handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, tenantclone.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusBadRequest, httperror.Error{Code: "TENANT_CLONE_VALIDATION_FAILED", Message: "授权目录克隆参数无效"})
	case errors.Is(err, tenantclone.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.Error{Code: "TENANT_CLONE_TENANT_NOT_FOUND", Message: "源租户或目标租户不存在"})
	default:
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Error{Code: "TENANT_CLONE_FAILED", Message: "授权目录克隆未完成，请使用同一 Idempotency-Key 重试"})
	}
}
