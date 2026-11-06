package filetaskhttp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	v2PolicyVersion     = "2026-09-v1"
	v2ProxyThreshold    = uint64(8 << 20)
	v2MaxUploadBytes    = uint64(20 << 20)
	v2TicketTTL         = 10 * time.Minute
	v2DownloadTicketTTL = 2 * time.Minute
)

type uploadV2FileService interface {
	Upload(context.Context, application.UploadInput) (domain.File, error)
	BindResource(context.Context, application.BindingInput) (domain.FileBinding, error)
	UnbindResource(context.Context, string, string, string, string) error
	OpenDownload(context.Context, application.DownloadAccess, string) (domain.StoredFile, io.ReadSeekCloser, error)
}

// UploadV2Handler 承载新上传会话。业务应用仍先校验资源 ACL，文件网关只接受
// 已验签的应用机器身份，并把租户和应用绑定到会话。
type UploadV2Handler struct {
	db     *gorm.DB
	files  uploadV2FileService
	logger *slog.Logger
	now    func() time.Time
	newID  func(time.Time) (string, error)
}

// NewUploadV2Handler 构造 v2 上传处理器；任一依赖为 nil 时失败关闭。
func NewUploadV2Handler(db *gorm.DB, files uploadV2FileService, newID func(time.Time) (string, error), logger *slog.Logger) (*UploadV2Handler, error) {
	if db == nil || files == nil || newID == nil || logger == nil {
		return nil, errors.New("v2 upload dependencies are required")
	}
	return &UploadV2Handler{db: db, files: files, logger: logger, now: func() time.Time { return time.Now().UTC() }, newID: newID}, nil
}

type uploadPolicy struct {
	Namespace string
	Purpose   string
	Media     map[string]struct{}
	MaxBytes  uint64
	Temporary bool
}

var uploadPolicies = map[string]uploadPolicy{
	"platform.iam.user-import":          policy("platform", "iam-user-import", true, "text/csv"),
	"crm.customer.import":               policy("crm", "customer-import", true, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
	"crm.opportunity.attachment":        policy("crm", "opportunity-attachment", false, "application/pdf", "image/png", "image/jpeg", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
	"portal.filing.material":            policy("portal", "filing-material", false, "application/pdf", "image/png", "image/jpeg"),
	"portal.local_report":               policyWithMax("portal", "local-report", false, 20<<20, "application/pdf"),
	"contract.external.source":          policyWithMax("contract", "external-source", false, 10<<20, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
	"contract.external.pdf-source":      policyWithMax("contract", "external-pdf-source", false, 20<<20, "application/pdf"),
	"contract.import.source":            policyWithMax("contract", "import-source", true, 20<<20, "application/pdf", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
	"contract.template":                 policyWithMax("contract", "template", false, 10<<20, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
	"contract.stamped-pdf":              policy("contract", "stamped-pdf", false, "application/pdf"),
	"project.field.evidence":            policy("project", "field-evidence", false, "application/pdf", "image/png", "image/jpeg"),
	"project.deviation.evidence":        policy("project", "deviation-evidence", false, "application/pdf", "image/png", "image/jpeg"),
	"project.report":                    policy("project", "report", false, "application/pdf"),
	"project.capability.import":         policy("project", "capability-import", true, "text/csv"),
	"project.detection-category.import": policy("project", "detection-category-import", true, "text/csv"),
	"settlement.receipt.document":       policy("settlement", "receipt-document", false, "application/pdf", "image/png", "image/jpeg"),
	"settlement.invoice.document":       policy("settlement", "invoice-document", false, "application/pdf", "image/png", "image/jpeg", "application/xml", "text/xml", "application/ofd"),
	// 结算报表导出（v1 通道沿用）必须显式声明用途：文本 CSV 才允许，上限 20MB、非临时文件。
	// 该用途把原先"空 purpose 裸上传"的报表导出收敛到策略表内，v1 与 v2 共用同一事实源。
	"settlement.report.export": policyWithMax("settlement", "report-export", false, 20<<20, "text/csv"),
}

func policy(namespace, purpose string, temporary bool, media ...string) uploadPolicy {
	return policyWithMax(namespace, purpose, temporary, v2MaxUploadBytes, media...)
}
func policyWithMax(namespace, purpose string, temporary bool, max uint64, media ...string) uploadPolicy {
	allowed := make(map[string]struct{}, len(media))
	for _, item := range media {
		allowed[item] = struct{}{}
	}
	return uploadPolicy{Namespace: namespace, Purpose: purpose, Media: allowed, MaxBytes: max, Temporary: temporary}
}

type uploadV2Session struct {
	ID, TenantID, ApplicationID, ApplicationCode, AuthenticatedClientID            string
	ActorUserID                                                                    *string
	FileID, VersionID, Namespace, Purpose, PolicyVersion                           string
	OriginalName, DeclaredMediaType, Classification                                string
	ExpectedSize                                                                   uint64
	ExpectedSHA256                                                                 []byte
	ResourceType, ResourceID, BindingType, DisplayName, IdempotencyKey, UploadMode string
	TicketHash                                                                     []byte
	TicketExpiresAt, TicketUsedAt                                                  *time.Time
	RetentionUntil                                                                 *time.Time
	Status                                                                         string
	FailureCode                                                                    *string
	CreatedAt, UpdatedAt                                                           time.Time
	CompletedAt                                                                    *time.Time
}

func (uploadV2Session) TableName() string { return "file_upload_v2_session" }

type downloadTicket struct {
	ID, TenantID, ApplicationID, FileID, ResourceType, ResourceID string
	TokenHash                                                     []byte
	ExpiresAt                                                     time.Time
	UsedAt                                                        *time.Time
	CreatedAt                                                     time.Time
}

func (downloadTicket) TableName() string { return "file_download_ticket" }

type accessAudit struct {
	ID                                    uint64
	TenantID, ApplicationID, FileID       string
	ActorUserID                           *string
	AuthenticatedClientID, Action, Result string
	RequestID                             *string
	CreatedAt                             time.Time
}

func (accessAudit) TableName() string { return "file_access_audit" }

type validationEvent struct {
	ID                          uint64
	TenantID, FileID            string
	Validator, ValidatorVersion string
	Result                      string
	ErrorCode                   *string
	CreatedAt                   time.Time
}

func (validationEvent) TableName() string { return "file_validation_event" }

type createUploadRequest struct {
	Purpose        string `json:"purpose"`
	OriginalName   string `json:"original_name"`
	MediaType      string `json:"media_type"`
	SHA256         string `json:"sha256"`
	Classification string `json:"classification"`
	SizeBytes      uint64 `json:"size_bytes"`
	ActorUserID    string `json:"actor_user_id"`
	ResourceType   string `json:"resource_type"`
	ResourceID     string `json:"resource_id"`
	BindingType    string `json:"binding_type"`
	DisplayName    string `json:"display_name"`
	IdempotencyKey string `json:"idempotency_key"`
}

// CreateSession 按白名单用途校验大小、摘要、身份、业务绑定与幂等键后创建会话。
// 参数：w为响应，r包含已认证应用和会话声明，此时尚未接收文件字节。
// 返回值：无；不合法声明失败关闭，不创建部分会话。
func (h *UploadV2Handler) CreateSession(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	var input createUploadRequest
	if !v2Decode(w, r, &input) {
		return
	}
	input.Purpose, input.OriginalName, input.MediaType = strings.TrimSpace(input.Purpose), strings.TrimSpace(input.OriginalName), canonicalMedia(input.MediaType)
	input.ResourceType, input.ResourceID, input.BindingType = strings.TrimSpace(input.ResourceType), strings.TrimSpace(input.ResourceID), strings.TrimSpace(input.BindingType)
	input.IdempotencyKey = strings.TrimSpace(firstNonEmpty(input.IdempotencyKey, r.Header.Get("Idempotency-Key")))
	selected, exists := uploadPolicies[input.Purpose]
	digest, digestErr := hex.DecodeString(strings.TrimSpace(input.SHA256))
	if !exists || input.OriginalName == "" || input.SizeBytes == 0 || input.SizeBytes > selected.MaxBytes || len(digest) != sha256.Size || digestErr != nil || input.ResourceType == "" || input.ResourceID == "" || input.BindingType == "" || input.IdempotencyKey == "" {
		v2Error(w, http.StatusUnprocessableEntity, "FILE_UPLOAD_INVALID", "上传会话参数不完整或不符合文件策略")
		return
	}
	if _, allowed := selected.Media[input.MediaType]; !allowed {
		v2Error(w, http.StatusUnprocessableEntity, "FILE_MEDIA_NOT_ALLOWED", "文件类型不在当前用途的允许范围内")
		return
	}
	if !validPurposeApplication(p.Account.Code, selected.Namespace) {
		v2Error(w, http.StatusForbidden, "FILE_PURPOSE_FORBIDDEN", "当前应用不能使用该文件用途")
		return
	}
	if input.Purpose == portalLocalReportPurpose {
		if !portalLocalReportUploadCaller(p) {
			v2Error(w, http.StatusForbidden, "FILE_PURPOSE_FORBIDDEN", "本地报告用途仅允许门户应用机器身份")
			return
		}
		if !portalLocalReportUploadBinding(input) {
			v2Error(w, http.StatusUnprocessableEntity, "FILE_UPLOAD_INVALID", "本地报告必须绑定规范的报告申请及REPORT类型")
			return
		}
	}
	// 操作人只能来自已认证主体。请求体的 actor_user_id 为兼容留存字段，服务端不再信任：
	// 声明值与主体不一致立即失败关闭，否则调用方可以替任意用户声明文件归属；机器主体
	// 没有用户身份时同样拒绝替用户声明。
	actor := strings.TrimSpace(p.User.ID)
	if declared := strings.TrimSpace(input.ActorUserID); declared != "" && declared != actor {
		v2Error(w, http.StatusUnprocessableEntity, "FILE_ACTOR_MISMATCH", "请求体声明的操作人与已认证身份不一致")
		return
	}
	if existing, found := h.findIdempotent(r.Context(), p, input.IdempotencyKey); found {
		// 会话落库的是策略内用途（selected.Purpose，如 "template"），请求体用的是用途键
		// （如 "contract.template"）。指纹必须与落库形态比较，否则同键同请求也会被误判成
		// 不同请求，幂等回放永远 409。
		if existing.Purpose != selected.Purpose || existing.ExpectedSize != input.SizeBytes || subtle.ConstantTimeCompare(existing.ExpectedSHA256, digest) != 1 || existing.ResourceType != input.ResourceType || existing.ResourceID != input.ResourceID {
			v2Error(w, http.StatusConflict, "FILE_IDEMPOTENCY_CONFLICT", "幂等键已被不同文件请求使用")
			return
		}
		// 终态失败会话（FAILED/REJECTED）已无法继续或成功。此前按 200 返回会让客户端
		// 误以为会话仍可用，因此这里失败关闭：不返回 sessionResponse、不改旧会话，
		// 要求客户端换用新的幂等键重新发起。
		if terminalUploadSessionFailure(existing.Status) {
			v2Error(w, http.StatusConflict, "FILE_UPLOAD_SESSION_FAILED", "该幂等键对应的上传已失败，请使用新的 Idempotency-Key 重新发起")
			return
		}
		v2JSON(w, http.StatusOK, sessionResponse(existing))
		return
	}
	now := h.now()
	sessionID, err := h.newID(now)
	if err != nil {
		v2Internal(w)
		return
	}
	fileID, err := h.newID(now.Add(time.Millisecond))
	if err != nil {
		v2Internal(w)
		return
	}
	versionID, err := h.newID(now.Add(2 * time.Millisecond))
	if err != nil {
		v2Internal(w)
		return
	}
	classification := strings.ToUpper(strings.TrimSpace(input.Classification))
	if classification == "" {
		classification = "INTERNAL"
	}
	mode := "PROXY"
	if input.SizeBytes > v2ProxyThreshold {
		mode = "DIRECT_GATEWAY"
	}
	var retentionUntil *time.Time
	if selected.Temporary {
		value := now.Add(24 * time.Hour)
		retentionUntil = &value
	}
	session := uploadV2Session{ID: sessionID, TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, ApplicationCode: p.Account.Code, AuthenticatedClientID: p.SessionID, ActorUserID: optionalString(actor), FileID: fileID, VersionID: versionID, Namespace: selected.Namespace, Purpose: selected.Purpose, PolicyVersion: v2PolicyVersion, OriginalName: filepath.Base(input.OriginalName), DeclaredMediaType: input.MediaType, Classification: classification, ExpectedSize: input.SizeBytes, ExpectedSHA256: digest, ResourceType: input.ResourceType, ResourceID: input.ResourceID, BindingType: input.BindingType, DisplayName: strings.TrimSpace(input.DisplayName), IdempotencyKey: input.IdempotencyKey, UploadMode: mode, RetentionUntil: retentionUntil, Status: "CREATED", CreatedAt: now, UpdatedAt: now}
	if err := h.db.WithContext(r.Context()).Create(&session).Error; err != nil {
		v2Internal(w)
		return
	}
	h.audit(r.Context(), session, "SESSION_CREATED", "SUCCESS", r.Header.Get("X-Request-ID"))
	v2JSON(w, http.StatusCreated, sessionResponse(session))
}

// IssueUploadTicket 为 CREATED 会话签发一次性上传票据；数据库只存票据哈希，
// 原文仅在响应中出现一次。
func (h *UploadV2Handler) IssueUploadTicket(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	var session uploadV2Session
	if err := h.db.WithContext(r.Context()).Where("id = ? AND tenant_id = ? AND application_id = ?", r.PathValue("upload_id"), p.Tenant.ID, p.Account.ID).Take(&session).Error; err != nil {
		v2Error(w, http.StatusNotFound, "FILE_SESSION_NOT_FOUND", "上传会话不存在")
		return
	}
	if session.Status != "CREATED" {
		v2Error(w, http.StatusConflict, "FILE_SESSION_STATE", "上传会话当前状态不允许签发票据")
		return
	}
	raw, hash, err := randomTicket()
	if err != nil {
		v2Internal(w)
		return
	}
	expires := h.now().Add(v2TicketTTL)
	result := h.db.WithContext(r.Context()).Model(&uploadV2Session{}).Where("id = ? AND status = ?", session.ID, "CREATED").Updates(map[string]any{"ticket_hash": hash, "ticket_expires_at": expires, "updated_at": h.now()})
	if result.Error != nil || result.RowsAffected != 1 {
		v2Error(w, http.StatusConflict, "FILE_SESSION_STATE", "上传会话状态已变更")
		return
	}
	v2JSON(w, http.StatusCreated, map[string]any{"upload_id": session.ID, "file_id": session.FileID, "mode": session.UploadMode, "upload_url": "/file-gateway/api/v2/upload-sessions/" + session.ID + "/content", "ticket": raw, "expires_at": expires.Format(time.RFC3339Nano)})
}

// UploadContent 以一次性票据兑换上传：核验哈希、占用与状态后流式落盘并完成
// 内容校验、会话推进与绑定创建，全程带审计。
func (h *UploadV2Handler) UploadContent(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "UploadTicket "))
	if raw == "" {
		v2Error(w, http.StatusUnauthorized, "FILE_TICKET_REQUIRED", "缺少上传票据")
		return
	}
	digest := sha256.Sum256([]byte(raw))
	var session uploadV2Session
	now := h.now()
	err := h.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ? AND ticket_used_at IS NULL", r.PathValue("upload_id"), "CREATED").Take(&session).Error; err != nil {
			return err
		}
		if session.TicketExpiresAt == nil || !session.TicketExpiresAt.After(now) || len(session.TicketHash) != sha256.Size || subtle.ConstantTimeCompare(session.TicketHash, digest[:]) != 1 {
			return errors.New("invalid upload ticket")
		}
		result := tx.Model(&uploadV2Session{}).Where("id = ? AND status = ?", session.ID, "CREATED").Updates(map[string]any{"status": "UPLOADING", "ticket_used_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if err != nil {
		// 无效上传票据尝试（AUD-2026-021）：票据是 PUT content 路径唯一凭据（无 Bearer
		// 主体），必须落 FAILED 访问审计后再返回既有错误码。会话已命中时记录其真实租户/
		// 应用/文件身份；会话未命中（伪造 upload_id）时仅记尝试本身，占位客户端标识
		// "upload-ticket" 与成功路径的票据行为一致。
		if session.ID != "" {
			h.auditDirect(r.Context(), session.TenantID, session.ApplicationID, session.FileID, deref(session.ActorUserID), session.AuthenticatedClientID, "UPLOAD_TICKET_REJECTED", "FAILED", r.Header.Get("X-Request-ID"))
		} else {
			h.auditDirect(r.Context(), "", "", "", "", "upload-ticket", "UPLOAD_TICKET_REJECTED", "FAILED", r.Header.Get("X-Request-ID"))
		}
		v2Error(w, http.StatusForbidden, "FILE_TICKET_INVALID", "上传票据无效、过期或已使用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(session.ExpectedSize)+1)
	retentionClass := "LONG_TERM"
	if session.RetentionUntil != nil {
		retentionClass = "TEMPORARY"
	}
	file, uploadErr := h.files.Upload(r.Context(), application.UploadInput{TenantID: session.TenantID, ApplicationID: session.ApplicationID, OwnerUserID: deref(session.ActorUserID), Namespace: session.Namespace, Purpose: session.Purpose, PolicyVersion: session.PolicyVersion, AuthenticatedClientID: session.AuthenticatedClientID, RetentionClass: retentionClass, RetentionUntil: session.RetentionUntil, OriginalName: session.OriginalName, DeclaredMediaType: session.DeclaredMediaType, Classification: session.Classification, RequestID: session.ID, PreallocatedFileID: session.FileID, PreallocatedVersionID: session.VersionID, ExpectedSize: session.ExpectedSize, ExpectedSHA256: session.ExpectedSHA256, Content: r.Body})
	if uploadErr != nil {
		if errors.Is(uploadErr, application.ErrValidation) {
			h.rejectSession(r.Context(), session, "STATIC_VALIDATION_FAILED")
			h.validation(r.Context(), session, "FAILED", "STATIC_VALIDATION_FAILED")
			v2Error(w, http.StatusUnprocessableEntity, "FILE_VALIDATION_FAILED", "文件内容与申报信息不一致或未通过安全校验")
		} else {
			h.failSession(r.Context(), session, "UPLOAD_FAILED")
			h.validation(r.Context(), session, "FAILED", "UPLOAD_FAILED")
			v2Error(w, http.StatusServiceUnavailable, "FILE_STORAGE_UNAVAILABLE", "文件存储暂时不可用，请稍后重试")
		}
		return
	}
	h.validation(r.Context(), session, "PASSED", "")
	operator := deref(session.ActorUserID)
	if operator == "" {
		operator = session.ApplicationID
	}
	binding, bindErr := h.files.BindResource(r.Context(), application.BindingInput{TenantID: session.TenantID, ApplicationID: session.ApplicationID, FileID: file.ID, ResourceType: session.ResourceType, ResourceID: session.ResourceID, BindingType: session.BindingType, DisplayName: firstNonEmpty(session.DisplayName, session.OriginalName), OperatorUserID: operator})
	if bindErr != nil {
		h.failSession(r.Context(), session, "BINDING_FAILED")
		v2Internal(w)
		return
	}
	completed := h.now()
	result := h.db.WithContext(r.Context()).Model(&uploadV2Session{}).Where("id = ? AND status = ?", session.ID, "UPLOADING").Updates(map[string]any{"status": "READY", "completed_at": completed, "updated_at": completed})
	if result.Error != nil || result.RowsAffected != 1 {
		h.failSession(r.Context(), session, "SESSION_FINALIZE_FAILED")
		v2Internal(w)
		return
	}
	h.audit(r.Context(), session, "UPLOAD_COMPLETED", "SUCCESS", r.Header.Get("X-Request-ID"))
	v2JSON(w, http.StatusCreated, map[string]any{"upload_id": session.ID, "file_id": file.ID, "binding_id": binding.ID, "status": "READY"})
}

// Complete 与 GetSession 同义：按会话 ID 或文件 ID 查询上传进度与结果。
func (h *UploadV2Handler) Complete(w http.ResponseWriter, r *http.Request) { h.GetSession(w, r) }

// GetSession 按 upload_id 或 file_id 返回上传会话状态；只对会话所属应用可见。
func (h *UploadV2Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	var session uploadV2Session
	query := h.db.WithContext(r.Context()).Where("tenant_id = ? AND application_id = ?", p.Tenant.ID, p.Account.ID)
	if id := r.PathValue("upload_id"); id != "" {
		query = query.Where("id = ?", id)
	} else {
		query = query.Where("file_id = ?", r.PathValue("file_id"))
	}
	if err := query.Take(&session).Error; err != nil {
		v2Error(w, http.StatusNotFound, "FILE_NOT_FOUND", "文件或上传会话不存在")
		return
	}
	v2JSON(w, http.StatusOK, sessionResponse(session))
}

type downloadTicketRequest struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

// IssueDownloadTicket 为已绑定资源签发一次性下载票据；资源标识必须由调用方
// 事先完成 ACL 校验，票据本身不携带授权。
func (h *UploadV2Handler) IssueDownloadTicket(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	var input downloadTicketRequest
	if !v2Decode(w, r, &input) {
		return
	}
	// 安全（SEC-D9）：HTTP 边界不再自证 ResourceAccessVerified——该标志只属于进程内
	// 受信子适配器；票据必须携带完整业务资源标识，服务端只依据数据库中的具体绑定证明放行。
	resourceType, resourceID := strings.TrimSpace(input.ResourceType), strings.TrimSpace(input.ResourceID)
	if resourceType == "" || resourceID == "" {
		v2Error(w, http.StatusBadRequest, "FILE_REQUEST_INVALID", "请求必须携带完整 resource_type 与 resource_id")
		return
	}
	access := application.DownloadAccess{TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, ResourceType: resourceType, ResourceID: resourceID, PermissionCodes: p.PermissionCodes}
	stored, stream, err := h.files.OpenDownload(r.Context(), access, r.PathValue("file_id"))
	if err != nil {
		v2Error(w, http.StatusForbidden, "FILE_DOWNLOAD_FORBIDDEN", "文件不存在或当前资源无权访问")
		return
	}
	_ = stream.Close()
	raw, hash, err := randomTicket()
	if err != nil {
		v2Internal(w)
		return
	}
	now := h.now()
	id, err := h.newID(now)
	if err != nil {
		v2Internal(w)
		return
	}
	ticket := downloadTicket{ID: id, TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, FileID: stored.File.ID, ResourceType: access.ResourceType, ResourceID: access.ResourceID, TokenHash: hash, ExpiresAt: now.Add(v2DownloadTicketTTL), CreatedAt: now}
	if err := h.db.WithContext(r.Context()).Create(&ticket).Error; err != nil {
		v2Internal(w)
		return
	}
	h.auditDirect(r.Context(), ticket.TenantID, ticket.ApplicationID, ticket.FileID, p.User.ID, p.SessionID, "DOWNLOAD_TICKET_ISSUED", "SUCCESS", r.Header.Get("X-Request-ID"))
	v2JSON(w, http.StatusCreated, map[string]any{"download_url": "/file-gateway/api/v2/files/" + stored.File.ID + "/content", "ticket": raw, "expires_at": ticket.ExpiresAt.Format(time.RFC3339Nano)})
}

// DownloadContent 以一次性票据兑换下载：事务内核销票据后按票据中的服务端资源
// 标识重新校验访问权，再流式输出文件并写审计。
func (h *UploadV2Handler) DownloadContent(w http.ResponseWriter, r *http.Request) {
	// 票据只接受 Authorization: DownloadTicket 头。历史实现允许 ?ticket= 查询参数回退，
	// 但 URL 会被浏览器历史、访问日志与代理记录留存，等于把 bearer 能力写进日志链；
	// 这里不再回退，缺失头即按空票据处理（下方统一返回既有无效票据错误）。
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "DownloadTicket "))
	hash := sha256.Sum256([]byte(raw))
	now := h.now()
	var ticket downloadTicket
	err := h.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("file_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?", r.PathValue("file_id"), hash[:], now).Take(&ticket).Error; err != nil {
			return err
		}
		return tx.Model(&downloadTicket{}).Where("id = ? AND used_at IS NULL", ticket.ID).Update("used_at", now).Error
	})
	if err != nil {
		// 无效下载票据/兑换尝试（AUD-2026-021）：票据查询未命中时无从得知租户与应用，
		// 以路径中的 file_id 与占位客户端标识记录 FAILED 审计后再返回既有错误码。
		h.auditDirect(r.Context(), "", "", r.PathValue("file_id"), "", "download-ticket", "DOWNLOAD_TICKET_REJECTED", "FAILED", r.Header.Get("X-Request-ID"))
		v2Error(w, http.StatusForbidden, "FILE_DOWNLOAD_TICKET_INVALID", "下载票据无效、过期或已使用")
		return
	}
	// 安全（SEC-D9）：兑换路径同样不自证 ResourceAccessVerified；资源标识取自服务端
	// 签发的票据，校验仍由服务端的具体绑定证明完成。
	access := application.DownloadAccess{TenantID: ticket.TenantID, ApplicationID: ticket.ApplicationID, ResourceType: ticket.ResourceType, ResourceID: ticket.ResourceID, PermissionCodes: []string{"platform:file:download"}}
	stored, stream, err := h.files.OpenDownload(r.Context(), access, ticket.FileID)
	if err != nil {
		h.auditDirect(r.Context(), ticket.TenantID, ticket.ApplicationID, ticket.FileID, "", "download-ticket", "DOWNLOAD_COMPLETED", "FAILED", r.Header.Get("X-Request-ID"))
		v2Error(w, http.StatusForbidden, "FILE_DOWNLOAD_FORBIDDEN", "文件已不可访问")
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", stored.Version.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": stored.Version.OriginalName}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	if _, err := io.Copy(w, stream); err != nil {
		h.auditDirect(r.Context(), ticket.TenantID, ticket.ApplicationID, ticket.FileID, "", "download-ticket", "DOWNLOAD_COMPLETED", "FAILED", r.Header.Get("X-Request-ID"))
		return
	}
	h.auditDirect(r.Context(), ticket.TenantID, ticket.ApplicationID, ticket.FileID, "", "download-ticket", "DOWNLOAD_COMPLETED", "SUCCESS", r.Header.Get("X-Request-ID"))
}

// UnbindFile 停用当前应用对指定文件的资源绑定；仅限绑定所属应用调用。
func (h *UploadV2Handler) UnbindFile(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	fileID, bindingID := strings.TrimSpace(r.PathValue("file_id")), strings.TrimSpace(r.PathValue("binding_id"))
	if fileID == "" || bindingID == "" {
		v2Error(w, http.StatusUnprocessableEntity, "FILE_BINDING_INVALID", "文件和绑定标识不能为空")
		return
	}
	if err := h.files.UnbindResource(r.Context(), p.Tenant.ID, p.Account.ID, fileID, bindingID); err != nil {
		v2Error(w, http.StatusForbidden, "FILE_BINDING_FORBIDDEN", "绑定不存在或当前应用无权停用")
		return
	}
	h.auditDirect(r.Context(), p.Tenant.ID, p.Account.ID, fileID, p.User.ID, p.SessionID, "BINDING_DEACTIVATED", "SUCCESS", r.Header.Get("X-Request-ID"))
	w.WriteHeader(http.StatusNoContent)
}

func (h *UploadV2Handler) findIdempotent(ctx context.Context, p authctx.Principal, key string) (uploadV2Session, bool) {
	var item uploadV2Session
	err := h.db.WithContext(ctx).Where("tenant_id = ? AND application_id = ? AND idempotency_key = ?", p.Tenant.ID, p.Account.ID, key).Take(&item).Error
	return item, err == nil
}

// terminalUploadSessionFailure 判断会话是否处于终态失败。file_upload_v2_session 的
// 状态只有 CREATED/UPLOADING/READY/FAILED/REJECTED，其中 FAILED/REJECTED 已收敛：
// 会话不会再推进，命中幂等键时必须让客户端换新键重生，而不是回 200 假装可用。
func terminalUploadSessionFailure(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "FAILED", "REJECTED":
		return true
	default:
		return false
	}
}

// failSession / rejectSession 只收敛仍在进行中的会话。
//
// 慢上传期间文件对账 Worker 可能已经把同一会话收敛到终态（恢复成功置 READY 并补做绑定）；
// 无条件的失败回写会把已成功的会话改写为 FAILED，客户端随后看到失败但文件其实可用。
func (h *UploadV2Handler) failSession(ctx context.Context, session uploadV2Session, code string) {
	now := h.now()
	h.db.WithContext(ctx).Model(&uploadV2Session{}).Where("id = ? AND status IN ?", session.ID, []string{"CREATED", "UPLOADING"}).Updates(map[string]any{"status": "FAILED", "failure_code": code, "updated_at": now})
	h.audit(ctx, session, "UPLOAD_COMPLETED", "FAILED", "")
}
func (h *UploadV2Handler) rejectSession(ctx context.Context, session uploadV2Session, code string) {
	now := h.now()
	h.db.WithContext(ctx).Model(&uploadV2Session{}).Where("id = ? AND status IN ?", session.ID, []string{"CREATED", "UPLOADING"}).Updates(map[string]any{"status": "REJECTED", "failure_code": code, "updated_at": now})
	h.audit(ctx, session, "UPLOAD_REJECTED", "FAILED", "")
}
func (h *UploadV2Handler) validation(ctx context.Context, session uploadV2Session, result, code string) {
	_ = h.db.WithContext(ctx).Create(&validationEvent{TenantID: session.TenantID, FileID: session.FileID, Validator: "static-content", ValidatorVersion: v2PolicyVersion, Result: result, ErrorCode: optionalString(code), CreatedAt: h.now()}).Error
}
func (h *UploadV2Handler) audit(ctx context.Context, s uploadV2Session, action, result, requestID string) {
	h.auditDirect(ctx, s.TenantID, s.ApplicationID, s.FileID, deref(s.ActorUserID), s.AuthenticatedClientID, action, result, requestID)
}

// auditDirect 落网关访问审计。写入失败不阻断主流程，但不能被空白标识符静默丢弃
// （AUD-2026-021，SEC-D4a 口径）：writeFileAccessAudit 输出带可告警字段的结构化
// error 日志，供运维检测并回填。
func (h *UploadV2Handler) auditDirect(ctx context.Context, tenantID, applicationID, fileID, actorUserID, authenticatedClientID, action, result, requestID string) {
	writeFileAccessAudit(ctx, h.db, h.logger, accessAudit{TenantID: tenantID, ApplicationID: applicationID, FileID: fileID, ActorUserID: optionalString(actorUserID), AuthenticatedClientID: authenticatedClientID, Action: action, Result: result, RequestID: optionalString(requestID), CreatedAt: h.now()})
}
func sessionResponse(s uploadV2Session) map[string]any {
	return map[string]any{
		"upload_id": s.ID, "file_id": s.FileID, "mode": s.UploadMode,
		"upload_url": "/file-gateway/api/v2/upload-sessions/" + s.ID + "/content",
		"purpose":    s.Purpose, "status": s.Status, "original_name": s.OriginalName,
		"media_type": s.DeclaredMediaType, "size_bytes": s.ExpectedSize,
		"sha256": hex.EncodeToString(s.ExpectedSHA256), "failure_code": s.FailureCode,
		"created_at": s.CreatedAt.Format(time.RFC3339Nano), "completed_at": s.CompletedAt,
	}
}
func randomTicket() (string, []byte, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", nil, err
	}
	raw := base64.RawURLEncoding.EncodeToString(data)
	digest := sha256.Sum256([]byte(raw))
	return raw, digest[:], nil
}
func canonicalMedia(value string) string {
	parsed, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed)
}
func validPurposeApplication(code, namespace string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	aliases := map[string][]string{"platform": {"platform", "basic-platform", "basic_platform"}, "crm": {"customer_and_opportunity", "customer-opportunity", "crm"}, "portal": {"customer_portal", "customer-portal", "portal"}, "contract": {"contract_management", "contract-management", "contract"}, "project": {"project_management", "project-management", "project"}, "settlement": {"settlement"}}
	for _, alias := range aliases[namespace] {
		if code == alias {
			return true
		}
	}
	return false
}
func v2Principal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok || p.Tenant.ID == "" || p.Account.ID == "" || p.Account.Code == "" {
		v2Error(w, http.StatusUnauthorized, "FILE_AUTH_REQUIRED", "需要已验证的应用身份")
		return authctx.Principal{}, false
	}
	return p, true
}
func v2Decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		v2Error(w, http.StatusUnprocessableEntity, "FILE_REQUEST_INVALID", "请求数据不合法")
		return false
	}
	return true
}
func v2JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func v2Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message})
}
func v2Internal(w http.ResponseWriter) {
	v2Error(w, http.StatusInternalServerError, "FILE_INTERNAL_ERROR", "文件网关处理失败")
}
func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
