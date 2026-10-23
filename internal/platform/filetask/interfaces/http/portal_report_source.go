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
	RequestID      uint64 `json:"request_id"`
	ExpectedSHA256 string `json:"expected_sha256"`
	ExpectedSize   int64  `json:"expected_size"`
	ExpectedMIME   string `json:"expected_mime"`
	ExpectedName   string `json:"expected_name"`
}

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

func validPortalReportSource(input portalReportSourceRequest) ([]byte, bool) {
	digest, err := hex.DecodeString(input.ExpectedSHA256)
	valid := err == nil && len(digest) == 32 && len(input.ExpectedSHA256) == 64 && input.RequestID > 0 && input.ExpectedSize > 0 && input.ExpectedSize <= int64(v2MaxUploadBytes) && input.ExpectedMIME == "application/pdf" && strings.TrimSpace(input.ExpectedName) != "" && len(input.ExpectedName) <= 255 && !strings.ContainsAny(input.ExpectedName, "\r\n\x00/\\")
	return digest, valid
}

// PortalReportContent 是两个经过评审的应用之间固定的只读委托。它绝不把来源应用的
// 凭据交给 Portal，也绝不把调用方自报的应用/资源当作授权证明。
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
	if err := h.db.WithContext(r.Context()).Where("tenant_id = ? AND file_id = ? AND application_code = ? AND namespace = ? AND purpose = ? AND status = ? AND resource_type = ? AND resource_id = ?", p.Tenant.ID, r.PathValue("file_id"), "project_management", "project", "report", "READY", "PORTAL_REPORT_REQUEST", strconv.FormatUint(input.RequestID, 10)).Take(&session).Error; err != nil {
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

func portalReportSourceMatches(stored domain.StoredFile, session uploadV2Session, input portalReportSourceRequest, digest []byte) bool {
	return stored.File.ID == session.FileID && stored.File.TenantID == session.TenantID && stored.File.ApplicationID == session.ApplicationID && stored.File.Namespace == "project" && stored.File.Purpose == "report" && stored.File.Status == domain.FileStatusReady && stored.Version.Status == domain.FileVersionStatusReady && stored.Version.ID != "" && stored.Version.ID == session.VersionID && stored.Version.FileID == stored.File.ID && stored.Version.SizeBytes == uint64(input.ExpectedSize) && stored.Version.MediaType == "application/pdf" && stored.Version.OriginalName == input.ExpectedName && subtle.ConstantTimeCompare(stored.Version.SHA256, digest) == 1 && subtle.ConstantTimeCompare(session.ExpectedSHA256, digest) == 1 && session.ExpectedSize == stored.Version.SizeBytes
}
