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
	db    *gorm.DB
	files uploadV2FileService
	now   func() time.Time
	newID func(time.Time) (string, error)
}

func NewUploadV2Handler(db *gorm.DB, files uploadV2FileService, newID func(time.Time) (string, error)) (*UploadV2Handler, error) {
	if db == nil || files == nil || newID == nil {
		return nil, errors.New("v2 upload dependencies are required")
	}
	return &UploadV2Handler{db: db, files: files, now: func() time.Time { return time.Now().UTC() }, newID: newID}, nil
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
	"contract.external.source":          policyWithMax("contract", "external-source", false, 10<<20, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
	"contract.template":                 policyWithMax("contract", "template", false, 10<<20, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
	"contract.stamped-pdf":              policy("contract", "stamped-pdf", false, "application/pdf"),
	"project.field.evidence":            policy("project", "field-evidence", false, "application/pdf", "image/png", "image/jpeg"),
	"project.deviation.evidence":        policy("project", "deviation-evidence", false, "application/pdf", "image/png", "image/jpeg"),
	"project.report":                    policy("project", "report", false, "application/pdf"),
	"project.capability.import":         policy("project", "capability-import", true, "text/csv"),
	"project.detection-category.import": policy("project", "detection-category-import", true, "text/csv"),
	"settlement.invoice.document":       policy("settlement", "invoice-document", false, "application/pdf", "image/png", "image/jpeg", "application/xml", "text/xml", "application/ofd"),
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
	if existing, found := h.findIdempotent(r.Context(), p, input.IdempotencyKey); found {
		if existing.Purpose != input.Purpose || existing.ExpectedSize != input.SizeBytes || subtle.ConstantTimeCompare(existing.ExpectedSHA256, digest) != 1 || existing.ResourceType != input.ResourceType || existing.ResourceID != input.ResourceID {
			v2Error(w, http.StatusConflict, "FILE_IDEMPOTENCY_CONFLICT", "幂等键已被不同文件请求使用")
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
	session := uploadV2Session{ID: sessionID, TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, ApplicationCode: p.Account.Code, AuthenticatedClientID: p.SessionID, ActorUserID: optionalString(input.ActorUserID), FileID: fileID, VersionID: versionID, Namespace: selected.Namespace, Purpose: selected.Purpose, PolicyVersion: v2PolicyVersion, OriginalName: filepath.Base(input.OriginalName), DeclaredMediaType: input.MediaType, Classification: classification, ExpectedSize: input.SizeBytes, ExpectedSHA256: digest, ResourceType: input.ResourceType, ResourceID: input.ResourceID, BindingType: input.BindingType, DisplayName: strings.TrimSpace(input.DisplayName), IdempotencyKey: input.IdempotencyKey, UploadMode: mode, RetentionUntil: retentionUntil, Status: "CREATED", CreatedAt: now, UpdatedAt: now}
	if err := h.db.WithContext(r.Context()).Create(&session).Error; err != nil {
		v2Internal(w)
		return
	}
	h.audit(r.Context(), session, "SESSION_CREATED", "SUCCESS", r.Header.Get("X-Request-ID"))
	v2JSON(w, http.StatusCreated, sessionResponse(session))
}

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

func (h *UploadV2Handler) Complete(w http.ResponseWriter, r *http.Request) { h.GetSession(w, r) }
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

func (h *UploadV2Handler) IssueDownloadTicket(w http.ResponseWriter, r *http.Request) {
	p, ok := v2Principal(w, r)
	if !ok {
		return
	}
	var input downloadTicketRequest
	if !v2Decode(w, r, &input) {
		return
	}
	access := application.DownloadAccess{TenantID: p.Tenant.ID, ApplicationID: p.Account.ID, ResourceType: strings.TrimSpace(input.ResourceType), ResourceID: strings.TrimSpace(input.ResourceID), PermissionCodes: p.PermissionCodes, ResourceAccessVerified: true}
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

func (h *UploadV2Handler) DownloadContent(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "DownloadTicket "))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("ticket"))
	}
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
		v2Error(w, http.StatusForbidden, "FILE_DOWNLOAD_TICKET_INVALID", "下载票据无效、过期或已使用")
		return
	}
	access := application.DownloadAccess{TenantID: ticket.TenantID, ApplicationID: ticket.ApplicationID, ResourceType: ticket.ResourceType, ResourceID: ticket.ResourceID, PermissionCodes: []string{"platform:file:download"}, ResourceAccessVerified: true}
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
func (h *UploadV2Handler) failSession(ctx context.Context, session uploadV2Session, code string) {
	now := h.now()
	h.db.WithContext(ctx).Model(&uploadV2Session{}).Where("id = ?", session.ID).Updates(map[string]any{"status": "FAILED", "failure_code": code, "updated_at": now})
	h.audit(ctx, session, "UPLOAD_COMPLETED", "FAILED", "")
}
func (h *UploadV2Handler) rejectSession(ctx context.Context, session uploadV2Session, code string) {
	now := h.now()
	h.db.WithContext(ctx).Model(&uploadV2Session{}).Where("id = ?", session.ID).Updates(map[string]any{"status": "REJECTED", "failure_code": code, "updated_at": now})
	h.audit(ctx, session, "UPLOAD_REJECTED", "FAILED", "")
}
func (h *UploadV2Handler) validation(ctx context.Context, session uploadV2Session, result, code string) {
	_ = h.db.WithContext(ctx).Create(&validationEvent{TenantID: session.TenantID, FileID: session.FileID, Validator: "static-content", ValidatorVersion: v2PolicyVersion, Result: result, ErrorCode: optionalString(code), CreatedAt: h.now()}).Error
}
func (h *UploadV2Handler) audit(ctx context.Context, s uploadV2Session, action, result, requestID string) {
	h.auditDirect(ctx, s.TenantID, s.ApplicationID, s.FileID, deref(s.ActorUserID), s.AuthenticatedClientID, action, result, requestID)
}
func (h *UploadV2Handler) auditDirect(ctx context.Context, tenantID, applicationID, fileID, actorUserID, authenticatedClientID, action, result, requestID string) {
	_ = h.db.WithContext(ctx).Create(&accessAudit{TenantID: tenantID, ApplicationID: applicationID, FileID: fileID, ActorUserID: optionalString(actorUserID), AuthenticatedClientID: authenticatedClientID, Action: action, Result: result, RequestID: optionalString(requestID), CreatedAt: h.now()}).Error
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
