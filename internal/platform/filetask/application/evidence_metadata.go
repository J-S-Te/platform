package application

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// 证据核验的资源标识长度上限：与业务子系统的资源标识口径一致，同时避免超长输入
// 进入 SQL 条件或日志。binding_type 可选，缺失时不参与过滤。
const (
	evidenceResourceTypeMaxLen = 64
	evidenceResourceIDMaxLen   = 128
	evidenceBindingTypeMaxLen  = 64
)

// EvidenceAccess 是只读证据元数据核验请求的服务端身份上下文。
// 权限码由 HTTP 适配器从已验签主体复制，客户端不能自行声明。
// ResourceType/ResourceID 是调用方声明的业务对象（例如 PROJECT_SERVICE_ITEM + 服务项 ID），
// 用于把绑定校验从"应用级"收窄到"对象级"；BindingType 可选，用于进一步收窄绑定类型。
type EvidenceAccess struct {
	TenantID, ApplicationID  string
	ResourceType, ResourceID string
	BindingType              string
	PermissionCodes          []string
}

// EvidenceMetadata 是文件网关提供给业务子系统的只读证据元数据。
// 它只包含核验"文件真实存在且确实属于本次业务提交"所需的字段：
// 不含文件内容、存储路径、其它应用的绑定明细，也不含任何凭据材料。
type EvidenceMetadata struct {
	FileID        string    `json:"file_id"`
	TenantID      string    `json:"tenant_id"`
	ApplicationID string    `json:"application_id"`
	Status        string    `json:"status"`
	MediaType     string    `json:"media_type"`
	OriginalName  string    `json:"original_name"`
	SHA256        string    `json:"sha256"`
	SizeBytes     uint64    `json:"size_bytes"`
	BindingActive bool      `json:"binding_active"`
	CreatedAt     time.Time `json:"created_at"`
}

// StatEvidence 只读核验一份证据文件是否真实存在、可用且绑定到本次提交的业务对象。
//
// 安全边界（BIZ-PROJECT-03）：业务子系统提交现场记录/电子签名/报告证据时，
// 不能再只校验客户端回执的形状（file_id + sha256 字符串），必须回源到文件网关。
// 因此这里坚持四条约束：
//  1. 租户隔离：查询恒带租户条件，跨租户命中不到行，统一按"不存在"回答；
//  2. 应用隔离：文件归属应用必须是调用方应用，否则同样回答"不存在"，
//     不泄漏其它应用是否存在该文件；
//  3. 对象级绑定：必须存在 (租户, 文件, 应用, resource_type, resource_id) 的 ACTIVE 绑定。
//     只看"该文件存在任意绑定"会让同一应用内服务项 A 的证据被复用到服务项 B，
//     所以这里不接受 HasAnyActiveBinding 级别的证明；
//  4. 绑定类型：调用方给出 binding_type 时一并精确匹配，未给出时只在资源维度过滤。
//
// 返回的 Status 恒为可用状态（仓储只解析 READY/AVAILABLE/ACTIVE），
// SHA256 与 SizeBytes 是服务端在落盘时计算并校验过的真实值。
func (service *FileService) StatEvidence(ctx context.Context, access EvidenceAccess, fileID string) (EvidenceMetadata, error) {
	tenantID := strings.TrimSpace(access.TenantID)
	applicationID := strings.TrimSpace(access.ApplicationID)
	resourceType := strings.TrimSpace(access.ResourceType)
	resourceID := strings.TrimSpace(access.ResourceID)
	bindingType := strings.TrimSpace(access.BindingType)
	fileID = strings.TrimSpace(fileID)
	if tenantID == "" || applicationID == "" || fileID == "" || resourceType == "" || resourceID == "" {
		return EvidenceMetadata{}, validation("evidence tenant, application, file_id, resource_type and resource_id are required")
	}
	if len(resourceType) > evidenceResourceTypeMaxLen || len(resourceID) > evidenceResourceIDMaxLen || len(bindingType) > evidenceBindingTypeMaxLen {
		return EvidenceMetadata{}, validation("evidence resource identifiers exceed the maximum length")
	}
	// 路由层已按 platform:file:download 授权；服务层再校验一次，避免路由配置错误放宽边界
	// （与 OpenDownload 的属主/权限双闸保持同一口径）。
	if !hasPermission(access.PermissionCodes, "platform:file:download") {
		return EvidenceMetadata{}, ErrForbidden
	}
	stored, err := service.repository.GetAvailable(ctx, tenantID, fileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return EvidenceMetadata{}, ErrNotFound
		}
		return EvidenceMetadata{}, err
	}
	if strings.TrimSpace(stored.File.TenantID) != tenantID || strings.TrimSpace(stored.File.ApplicationID) != applicationID {
		return EvidenceMetadata{}, ErrNotFound
	}
	bound, err := service.repository.HasExactActiveBinding(ctx, tenantID, fileID, applicationID, resourceType, resourceID, bindingType)
	if err != nil {
		return EvidenceMetadata{}, err
	}
	if !bound {
		// 文件可下载但没有绑定到本次提交的业务对象：它不能被当作该对象的业务证据。
		// 按冲突回答以便运维区分"文件不存在"与"文件存在但没绑定到该业务对象"。
		return EvidenceMetadata{}, ErrConflict
	}
	return EvidenceMetadata{
		FileID: stored.File.ID, TenantID: stored.File.TenantID, ApplicationID: stored.File.ApplicationID,
		Status:    stored.File.Status,
		MediaType: stored.Version.MediaType, OriginalName: stored.Version.OriginalName,
		SHA256: hex.EncodeToString(stored.Version.SHA256), SizeBytes: stored.Version.SizeBytes,
		BindingActive: true, CreatedAt: stored.File.CreatedAt.UTC(),
	}, nil
}
