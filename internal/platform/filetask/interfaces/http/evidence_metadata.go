package filetaskhttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// evidenceMetadataService 是只读证据核验所需的窄接口：业务子系统只需要"回源核验
// 一份证据文件是否真实存在、可用、归属同一租户与应用"这一件事，不能借该端点读取
// 内容、列出绑定或写入任何状态（BIZ-PROJECT-03）。
type evidenceMetadataService interface {
	StatEvidence(context.Context, application.EvidenceAccess, string) (application.EvidenceMetadata, error)
}

// EvidenceHandler 提供文件网关的只读证据元数据端点。它有意与 v1 上传/下载处理器分开：
// 该端点没有任何写路径，也不需要并发上传名额。
type EvidenceHandler struct {
	files  evidenceMetadataService
	logger *slog.Logger
}

// NewEvidenceHandler 校验依赖后构造只读证据元数据处理器；缺失依赖时失败关闭。
func NewEvidenceHandler(files evidenceMetadataService, logger *slog.Logger) (*EvidenceHandler, error) {
	if files == nil || logger == nil {
		return nil, errors.New("file gateway evidence metadata dependencies must not be nil")
	}
	return &EvidenceHandler{files: files, logger: logger}, nil
}

// StatFile 返回指定证据文件的只读元数据（租户、应用、状态、真实 SHA-256、大小与
// ACTIVE 绑定标记）。跨租户、跨应用、非可用状态、未绑定一律不返回文件元数据：
// 不存在/跨应用统一 404，未绑定 409，避免泄漏其它应用的文件是否存在。
//
// 绑定校验是对象级的：resource_type 与 resource_id 必填（缺失 422），服务层要求存在
// (租户, 文件, 应用, 资源) 的 ACTIVE 绑定；binding_type 可选，用于进一步收窄。
func (handler *EvidenceHandler) StatFile(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.Account.ID) == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	query := request.URL.Query()
	fileID := strings.TrimSpace(request.PathValue("file_id"))
	resourceType := strings.TrimSpace(query.Get("resource_type"))
	resourceID := strings.TrimSpace(query.Get("resource_id"))
	// 对象级绑定是证据有效性的前提：缺参数时直接 422，不进入服务层，也不退化成
	// "只校验应用级绑定"。
	if resourceType == "" || resourceID == "" {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	metadata, err := handler.files.StatEvidence(request.Context(), application.EvidenceAccess{
		TenantID: principal.Tenant.ID, ApplicationID: principal.Account.ID,
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		BindingType:     strings.TrimSpace(query.Get("binding_type")),
		PermissionCodes: principal.PermissionCodes,
	}, fileID)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrValidation):
			httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		case errors.Is(err, application.ErrNotFound):
			// 跨租户、跨应用与真正的不存在共用同一回答，不暴露文件归属信息。
			httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
		case errors.Is(err, application.ErrForbidden):
			httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		case errors.Is(err, application.ErrConflict):
			httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
		default:
			handler.logger.Error("file gateway evidence metadata request failed", "path", request.URL.Path, "error", err)
			httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		}
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "文件证据元数据查询成功", metadata)
}
