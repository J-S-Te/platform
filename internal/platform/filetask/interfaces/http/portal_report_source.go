package filetaskhttp

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/gorm"
)

const portalReportFileScope = "platform:file:portal_report"

type portalReportSourceRequest struct {
	SourceType     string `json:"source_type"`
	RequestID      uint64 `json:"request_id"`
	ExpectedSHA256 string `json:"expected_sha256"`
	ExpectedSize   int64  `json:"expected_size"`
	ExpectedMIME   string `json:"expected_mime"`
	ExpectedName   string `json:"expected_name"`
}

// portalReportSourceCaller 保留门户报告专用机器身份与精确scope门禁。
// 参数：p为已认证的当前机器主体。
// 返回值：专用主体匹配时返回true，普通writer、浏览器或通配scope均拒绝。
func portalReportSourceCaller(p authctx.Principal) bool {
	if p.Account.Code != "customer_portal" || p.User.ID != "" || p.Tenant.ID == "" || p.Account.ID == "" || !strings.HasSuffix(p.SessionID, "-portal-report-source") {
		return false
	}
	for _, scope := range p.PermissionCodes {
		if scope == portalReportFileScope {
			return true
		}
	}
	return false
}

// validPortalReportSource 校验明确来源类型及不可变PDF完整性声明。
// 参数：input为调用者固定的报告申请、文件大小、摘要、类型和名称。
// 返回值：解码摘要与合法标记；未知来源不回退项目分支。
func validPortalReportSource(input portalReportSourceRequest) ([]byte, bool) {
	digest, err := hex.DecodeString(input.ExpectedSHA256)
	_, _, _, knownSource := portalReportSourceKind(input.SourceType)
	valid := knownSource && err == nil && len(digest) == 32 &&
		len(input.ExpectedSHA256) == 64 && input.RequestID > 0 &&
		input.ExpectedSize > 0 && input.ExpectedSize <= int64(v2MaxUploadBytes) &&
		input.ExpectedMIME == "application/pdf" && strings.TrimSpace(input.ExpectedName) != "" &&
		len(input.ExpectedName) <= 255 && !strings.ContainsAny(input.ExpectedName, "\r\n\x00/\\")
	return digest, valid
}

// PortalReportContent 读取经过评审的固定项目来源或当前门户应用本地来源。
// 参数：w为响应，r含专用机器主体、文件ID及完整性声明，不接受自报应用身份。
// 返回值：无；来源、当前绑定、版本或审计失败时不释放文件字节。
func (h *UploadV2Handler) PortalReportContent(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	if !portalReportSourceCaller(p) {
		v2Error(w, http.StatusForbidden, "PORTAL_REPORT_SOURCE_FORBIDDEN", "报告来源凭据或应用不符合要求")
		return
	}
	var input portalReportSourceRequest
	if !v2Decode(w, r, &input) {
		return
	}
	digest, valid := validPortalReportSource(input)
	if !valid {
		v2Error(w, http.StatusBadRequest, "PORTAL_REPORT_SOURCE_INVALID", "报告来源完整性参数不合法")
		return
	}
	var session uploadV2Session
	query := portalReportSourceSessionQuery(h.db.WithContext(r.Context()), p, input, r.PathValue("file_id"))
	if err := query.Take(&session).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			h.logger.ErrorContext(r.Context(), "Portal report source lookup unavailable")
			v2Error(w, http.StatusServiceUnavailable, "PORTAL_REPORT_SOURCE_UNAVAILABLE", "报告来源校验暂不可用")
			return
		}
		v2Error(w, http.StatusForbidden, "PORTAL_REPORT_SOURCE_FORBIDDEN", "文件没有匹配的报告来源或请求绑定")
		return
	}
	// 归属应用 ID 只取自已认证的上传会话。OpenDownload 在返回任何字节前独立证明
	// 绑定仍然精确有效且内容未变。
	access := application.DownloadAccess{TenantID: p.Tenant.ID, ApplicationID: session.ApplicationID, ResourceType: "PORTAL_REPORT_REQUEST", ResourceID: strconv.FormatUint(input.RequestID, 10), PermissionCodes: []string{"platform:file:download"}}
	stored, content, err := h.files.OpenDownload(r.Context(), access, session.FileID)
	if err != nil {
		v2Error(w, http.StatusForbidden, "PORTAL_REPORT_SOURCE_FORBIDDEN", "报告文件不存在、绑定失效或无法校验")
		return
	}
	defer content.Close()
	if input.SourceType == "PORTAL_LOCAL" {
		bound, err := h.portalLocalReportBinding(r, session, input.RequestID)
		if err != nil {
			v2Error(w, http.StatusServiceUnavailable, "PORTAL_REPORT_SOURCE_UNAVAILABLE", "报告来源绑定校验暂不可用")
			return
		}
		if !bound {
			v2Error(w, http.StatusForbidden, "PORTAL_REPORT_SOURCE_FORBIDDEN", "本地报告没有当前有效的REPORT绑定")
			return
		}
	}
	if !portalReportSourceMatches(stored, session, input, digest) {
		v2Error(w, http.StatusConflict, "PORTAL_REPORT_SOURCE_CHANGED", "报告来源与固定版本或摘要不一致")
		return
	}
	// 在释放跨应用内容之前先持久化授权证据。
	audit := accessAudit{TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, FileID: stored.File.ID, AuthenticatedClientID: p.SessionID, Action: "PORTAL_REPORT_SOURCE_AUTHORIZED", Result: "SUCCESS", RequestID: optionalString(r.Header.Get("X-Request-ID")), CreatedAt: h.now()}
	if err := h.db.WithContext(r.Context()).Create(&audit).Error; err != nil {
		v2Error(w, http.StatusServiceUnavailable, "PORTAL_REPORT_SOURCE_AUDIT_UNAVAILABLE", "报告来源读取审计暂不可用")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.FormatInt(input.ExpectedSize, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": stored.Version.OriginalName}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-File-Version-ID", stored.Version.ID)
	w.Header().Set("X-File-SHA256", hex.EncodeToString(stored.Version.SHA256))
	w.WriteHeader(http.StatusOK)
	n, copyErr := io.CopyN(w, content, input.ExpectedSize)
	result := "SUCCESS"
	if copyErr != nil || n != input.ExpectedSize {
		result = "FAILED"
		h.logger.ErrorContext(r.Context(), "Portal report source stream incomplete", "file_id", stored.File.ID)
	}
	h.auditDirect(r.Context(), p.Tenant.ID, p.Account.ID, stored.File.ID, "", p.SessionID, "PORTAL_REPORT_SOURCE_READ", result, r.Header.Get("X-Request-ID"))
}

// portalReportSourceMatches 核对真实文件版本与声明，明确区分门户本地和旧项目来源。
// 参数：stored为OpenDownload真实结果，session为权威上传会话，input为声明，digest为摘要。
// 返回值：命名空间、内容及固定版本全部一致时返回true。
func portalReportSourceMatches(stored domain.StoredFile, session uploadV2Session, input portalReportSourceRequest, digest []byte) bool {
	code, namespace, purpose, known := portalReportSourceKind(input.SourceType)
	localSessionMatches := session.ApplicationCode == code && session.Namespace == namespace &&
		session.Purpose == purpose && session.ApplicationID != ""
	if !known || (input.SourceType == "PORTAL_LOCAL" && !localSessionMatches) {
		return false
	}
	fileMatches := stored.File.ID == session.FileID && stored.File.TenantID == session.TenantID &&
		stored.File.ApplicationID == session.ApplicationID && stored.File.Namespace == namespace &&
		stored.File.Purpose == purpose && stored.File.Status == domain.FileStatusReady
	versionMatches := stored.Version.Status == domain.FileVersionStatusReady &&
		stored.Version.ID != "" && stored.Version.ID == session.VersionID &&
		stored.Version.FileID == stored.File.ID && stored.Version.SizeBytes == uint64(input.ExpectedSize) &&
		stored.Version.MediaType == "application/pdf" && stored.Version.OriginalName == input.ExpectedName
	integrityMatches := subtle.ConstantTimeCompare(stored.Version.SHA256, digest) == 1 &&
		subtle.ConstantTimeCompare(session.ExpectedSHA256, digest) == 1 &&
		session.ExpectedSize == stored.Version.SizeBytes
	return fileMatches && versionMatches && integrityMatches
}

// portalReportSourceKind 将受控来源枚举映射到固定应用与存储用途。
// 参数：source为请求显式来源，空值保留旧项目读取兼容。
// 返回值：固定应用编码、命名空间、用途与已知标记，不支持通配或自报路径。
func portalReportSourceKind(source string) (string, string, string, bool) {
	switch source {
	case "", "PROJECT":
		return "project_management", "project", "report", true
	case "PORTAL_LOCAL":
		return "customer_portal", "portal", "local-report", true
	default:
		return "", "", "", false
	}
}

// portalReportSourceSessionQuery 按真实租户、来源及申请读取READY上传会话。
// 参数：query为请求数据库，p为专用主体，input为已校验声明，fileID来自路径。
// 返回值：受限查询；LOCAL额外固定本次认证应用ID及REPORT绑定类型。
func portalReportSourceSessionQuery(query *gorm.DB, p authctx.Principal, input portalReportSourceRequest, fileID string) *gorm.DB {
	code, namespace, purpose, known := portalReportSourceKind(input.SourceType)
	if !known {
		return query.Where("1=0")
	}
	query = query.Where("tenant_id = ? AND file_id = ? AND application_code = ? AND namespace = ?"+
		" AND purpose = ? AND status = ? AND resource_type = ? AND resource_id = ?",
		p.Tenant.ID, fileID, code, namespace, purpose, "READY", "PORTAL_REPORT_REQUEST",
		strconv.FormatUint(input.RequestID, 10))
	if input.SourceType == "PORTAL_LOCAL" {
		query = query.Where("application_id = ? AND binding_type = ?", p.Account.ID, "REPORT")
	}
	return query
}

// portalLocalReportBinding 进一步确认当前有效绑定仍然是门户报告专有REPORT类型。
// 参数：r为请求上下文，session为受限上传会话，requestID为报告申请ID。
// 返回值：绑定存在标记及查询错误，未知或失效绑定不可降级放行。
func (h *UploadV2Handler) portalLocalReportBinding(r *http.Request, session uploadV2Session, requestID uint64) (bool, error) {
	var count int64
	err := h.db.WithContext(r.Context()).Table("file_binding").Where(
		"tenant_id = ? AND application_id = ? AND file_id = ? AND resource_type = ?"+
			" AND resource_id = ? AND binding_type = ? AND status = ?",
		session.TenantID, session.ApplicationID, session.FileID, "PORTAL_REPORT_REQUEST",
		strconv.FormatUint(requestID, 10), "REPORT", "ACTIVE").Count(&count).Error
	return count > 0, err
}
