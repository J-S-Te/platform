package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

const defaultUploadMaxBytes int64 = 20 << 20

var (
	classificationValues = map[string]struct{}{"INTERNAL": {}, "CONFIDENTIAL": {}, "RESTRICTED": {}}
	jobTypePattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

// FileService 管理数据库元数据与本地二进制之间的状态机；两种存储无法共享事务，
// 因此每个跨边界步骤都必须有明确失败状态或补偿动作。
type FileService struct {
	repository FileRepository
	store      LocalStore
	ids        IDGenerator
	clock      Clock
	policy     UploadPolicy
}

// NewFileService 要求显式上传策略。大小未配置时使用保守默认值，但 MIME 白名单不能为空，
// 不存在“接受任意文件类型”的降级模式。
func NewFileService(repository FileRepository, store LocalStore, ids IDGenerator, clock Clock, policy UploadPolicy) (*FileService, error) {
	if repository == nil || store == nil || ids == nil || clock == nil {
		return nil, errors.New("file service dependencies must not be nil")
	}
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = defaultUploadMaxBytes
	}
	if len(policy.AllowedMediaTypes) == 0 {
		return nil, fmt.Errorf("%w: upload MIME allowlist is required", ErrValidation)
	}
	allowed := make(map[string]struct{}, len(policy.AllowedMediaTypes))
	for mediaType := range policy.AllowedMediaTypes {
		canonical, err := canonicalMediaType(mediaType)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid allowed media type", ErrValidation)
		}
		allowed[canonical] = struct{}{}
	}
	policy.AllowedMediaTypes = allowed
	return &FileService{repository: repository, store: store, ids: ids, clock: clock, policy: policy}, nil
}

// DefaultUploadPolicy 只开放平台明确处理的少量格式。业务模块需要其他类型时必须修改服务端
// 白名单，浏览器声明的 Content-Type 不能扩大信任边界。
func DefaultUploadPolicy() UploadPolicy {
	return UploadPolicy{
		MaxBytes: defaultUploadMaxBytes,
		AllowedMediaTypes: map[string]struct{}{
			"application/pdf": {},
			"application/zip": {},
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {},
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {},
			"application/vnd.openxmlformats-officedocument.presentationml.presentation": {},
			"image/jpeg": {},
			"image/png":  {},
			"text/plain": {},
			"text/csv":   {},
		},
	}
}

// Upload 先创建 PENDING_UPLOAD 元数据，再原子发布文件并进入 VALIDATING，校验通过后推进到 READY。
// 文件系统失败会把元数据标为 FAILED；数据库推进失败则删除新文件并标记失败，尽量补偿
// MySQL 与文件系统之间无法实现分布式事务造成的不一致窗口。
func (service *FileService) Upload(ctx context.Context, input UploadInput) (domain.File, error) {
	if err := validateUploadInput(input); err != nil {
		return domain.File{}, err
	}

	name, extension := safeOriginalName(input.OriginalName)
	if name == "" {
		return domain.File{}, validation("file name is invalid")
	}
	peeked, content, err := peekContent(input.Content)
	if err != nil {
		return domain.File{}, fmt.Errorf("read upload header: %w", err)
	}
	mediaType, err := service.resolveMediaType(input.DeclaredMediaType, extension, peeked)
	if err != nil {
		return domain.File{}, err
	}
	// 在创建元数据前计算完整请求摘要，确保重试时能区分同一请求与同一 request_id 的不同内容。
	contentBytes, err := io.ReadAll(io.LimitReader(content, service.policy.MaxBytes+1))
	if err != nil || int64(len(contentBytes)) > service.policy.MaxBytes {
		return domain.File{}, validation("upload exceeds configured maximum size")
	}
	content = bytes.NewReader(contentBytes)
	requestHash := uploadRequestHash(input, mediaType, contentBytes)
	now := service.clock.Now().UTC()
	fileID, err := service.ids.New(now)
	if err != nil {
		return domain.File{}, fmt.Errorf("generate file ID: %w", err)
	}
	versionID, err := service.ids.New(now.Add(time.Millisecond))
	if err != nil {
		return domain.File{}, fmt.Errorf("generate file version ID: %w", err)
	}

	classification := strings.ToUpper(strings.TrimSpace(input.Classification))
	if classification == "" {
		classification = "INTERNAL"
	}
	relativePath := storageRelativePath(input.TenantID, input.ApplicationID, now, fileID, versionID)
	file := domain.File{
		ID: fileID, TenantID: strings.TrimSpace(input.TenantID), ApplicationID: strings.TrimSpace(input.ApplicationID),
		OriginalName: name, FileExtension: extension, MediaType: mediaType, Classification: classification,
		OwnerUserID: strings.TrimSpace(input.OwnerUserID), CurrentVersionNo: 1, CurrentVersionID: versionID,
		Status: domain.FileStatusPendingUpload, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	version := domain.FileVersion{
		ID: versionID, FileID: fileID, VersionNo: 1, StorageRelativePath: relativePath,
		MediaType: mediaType, OriginalName: name, UploaderUserID: strings.TrimSpace(input.OwnerUserID),
		UploadRequestID: strings.TrimSpace(input.RequestID), Status: domain.FileVersionStatusPendingUpload, CreatedAt: now,
		UploadRequestHash: requestHash[:],
	}
	if strings.TrimSpace(input.RequestID) != "" {
		existing, created, reserveErr := service.repository.ReserveUpload(ctx, file, version)
		if reserveErr != nil {
			return domain.File{}, reserveErr
		}
		if !created {
			return existing.File, nil
		}
	} else if err := service.repository.CreateWriting(ctx, file, version); err != nil {
		return domain.File{}, err
	}

	size, digest, err := service.store.WriteAtomically(ctx, relativePath, content, service.policy.MaxBytes)
	if err != nil {
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, fmt.Errorf("%w: write upload: %v", ErrStorage, err)
	}
	if err := service.repository.MarkValidating(ctx, file.TenantID, file.ID, size, digest, service.clock.Now().UTC()); err != nil {
		_ = service.store.Remove(relativePath)
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, err
	}
	stored, openErr := service.store.OpenVerified(relativePath)
	if openErr != nil {
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, fmt.Errorf("%w: open uploaded file for validation: %v", ErrStorage, openErr)
	}
	validationErr := validateStoredContent(mediaType, stored)
	closeErr := stored.Close()
	if validationErr != nil {
		_ = service.store.Remove(relativePath)
		_ = service.repository.MarkRejected(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, validation(validationErr.Error())
	}
	if closeErr != nil {
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, fmt.Errorf("%w: close uploaded file after validation: %v", ErrStorage, closeErr)
	}
	if err := service.repository.MarkReady(ctx, file.TenantID, file.ID, service.clock.Now().UTC()); err != nil {
		_ = service.store.Remove(relativePath)
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, err
	}
	file.Status = domain.FileStatusReady
	file.UpdatedAt = service.clock.Now().UTC()
	return file, nil
}

func uploadRequestHash(input UploadInput, mediaType string, content []byte) [32]byte {
	hash := sha256.New()
	// 字段长度前缀避免不同字段拼接产生同一摘要；二进制内容始终参与摘要。
	classification := strings.ToUpper(strings.TrimSpace(input.Classification))
	if classification == "" {
		classification = "INTERNAL"
	}
	for _, value := range []string{strings.TrimSpace(input.TenantID), strings.TrimSpace(input.ApplicationID), strings.TrimSpace(input.OwnerUserID), strings.TrimSpace(input.RequestID), mediaType, strings.TrimSpace(input.OriginalName), classification} {
		_, _ = fmt.Fprintf(hash, "%d:", len(value))
		_, _ = hash.Write([]byte(value))
	}
	_, _ = fmt.Fprintf(hash, "%d:", len(content))
	_, _ = hash.Write(content)
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

// OpenDownload 只解析 READY 文件，并在打开本地句柄前再次执行“所有者或下载权限”判断。
// 即使路由权限配置错误，应用层也不会让任意已登录用户绕过文件归属；调用方负责流式发送后关闭句柄。
func (service *FileService) OpenDownload(ctx context.Context, access DownloadAccess, fileID string) (domain.StoredFile, io.ReadSeekCloser, error) {
	// UserID 仅在浏览器用户下载时必填；File Gateway 机器令牌无平台用户主体，机器上传的文件
	// OwnerUserID 为空，owner 匹配直接放行，非 owner 则走权限/绑定校验。
	if strings.TrimSpace(access.TenantID) == "" || strings.TrimSpace(fileID) == "" {
		return domain.StoredFile{}, nil, validation("download tenant and file_id are required")
	}
	stored, err := service.repository.GetAvailable(ctx, strings.TrimSpace(access.TenantID), strings.TrimSpace(fileID))
	if err != nil {
		return domain.StoredFile{}, nil, err
	}
	if stored.File.OwnerUserID != access.UserID {
		if !hasPermission(access.PermissionCodes, "platform:file:download") {
			return domain.StoredFile{}, nil, ErrForbidden
		}
		applicationID := strings.TrimSpace(access.ApplicationID)
		resourceType := strings.TrimSpace(access.ResourceType)
		resourceID := strings.TrimSpace(access.ResourceID)
		// ResourceAccessVerified 只能由进程内的受信子系统适配器在完成业务 ACL 校验后设置；
		// 浏览器查询参数不能生成该证明，否则知道资源标识即可伪造授权。
		if !access.ResourceAccessVerified || applicationID == "" || resourceType == "" || resourceID == "" || applicationID != stored.File.ApplicationID {
			return domain.StoredFile{}, nil, ErrForbidden
		}
		bound, bindErr := service.repository.HasActiveBinding(ctx, stored.File.TenantID, stored.File.ID, applicationID, resourceType, resourceID)
		if bindErr != nil {
			return domain.StoredFile{}, nil, bindErr
		}
		if !bound {
			return domain.StoredFile{}, nil, ErrForbidden
		}
	}
	handle, err := service.store.OpenVerified(stored.Version.StorageRelativePath)
	if err != nil {
		return domain.StoredFile{}, nil, fmt.Errorf("%w: open local file: %v", ErrStorage, err)
	}
	if err := verifyStoredIntegrity(handle, stored.Version.SizeBytes, stored.Version.SHA256); err != nil {
		_ = handle.Close()
		return domain.StoredFile{}, nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	return stored, handle, nil
}

// BindResource 创建文件到业务资源的幂等绑定。参数错误、文件不属于相同租户/应用、
// 文件尚未 READY 时返回对应错误；相同 ACTIVE 绑定重复提交时仓储返回原绑定。
func (service *FileService) BindResource(ctx context.Context, input BindingInput) (domain.FileBinding, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.FileID = strings.TrimSpace(input.FileID)
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	input.ResourceID = strings.TrimSpace(input.ResourceID)
	input.BindingType = strings.TrimSpace(input.BindingType)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.OperatorUserID = strings.TrimSpace(input.OperatorUserID)
	if input.TenantID == "" || input.ApplicationID == "" || input.FileID == "" || input.ResourceType == "" || input.ResourceID == "" || input.BindingType == "" || input.OperatorUserID == "" {
		return domain.FileBinding{}, validation("binding tenant, application, file, resource, type and operator are required")
	}
	if len(input.ResourceType) > 64 || len(input.ResourceID) > 128 || len(input.BindingType) > 64 || len([]byte(input.DisplayName)) > 512 {
		return domain.FileBinding{}, validation("binding field exceeds the configured limit")
	}
	now := service.clock.Now().UTC()
	bindingID, err := service.ids.New(now)
	if err != nil {
		return domain.FileBinding{}, fmt.Errorf("generate file binding ID: %w", err)
	}
	return service.repository.CreateBinding(ctx, domain.FileBinding{
		ID: bindingID, TenantID: input.TenantID, ApplicationID: input.ApplicationID,
		FileID: input.FileID, ResourceType: input.ResourceType, ResourceID: input.ResourceID,
		BindingType: input.BindingType, DisplayName: input.DisplayName, SortOrder: input.SortOrder,
		Status: "ACTIVE", CreatedBy: input.OperatorUserID, CreatedAt: now,
	})
}

// UnbindResource 仅停用当前租户下指定文件的绑定，不物理删除审计证据。
// 绑定不存在、已停用或绑定不属于该文件时返回冲突或未找到错误。
func (service *FileService) UnbindResource(ctx context.Context, tenantID, applicationID, fileID, bindingID string) error {
	tenantID, applicationID, fileID, bindingID = strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(fileID), strings.TrimSpace(bindingID)
	if tenantID == "" || applicationID == "" || fileID == "" || bindingID == "" {
		return validation("tenant_id, application_id, file_id and binding_id are required")
	}
	return service.repository.DeactivateBinding(ctx, tenantID, applicationID, fileID, bindingID, service.clock.Now().UTC())
}

// CleanupUnboundExpired 只清理早于截止时间、无业务绑定且状态为 READY 的文件。
// 现有表没有逐文件过期时间，所以截止点来自外部策略；若要支持每个文件独立过期，必须新增迁移。
func (service *FileService) CleanupUnboundExpired(ctx context.Context, tenantID string, cutoff time.Time, maxFiles int) (domain.CleanupResult, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || cutoff.IsZero() || maxFiles <= 0 {
		return domain.CleanupResult{}, validation("tenant_id, cleanup cutoff and max_files are required")
	}
	result := domain.CleanupResult{}
	for range maxFiles {
		stored, claimed, err := service.repository.ClaimExpiredUnbound(ctx, tenantID, cutoff.UTC())
		if err != nil {
			return result, err
		}
		if !claimed {
			break
		}
		result.ClaimedFiles++
		if err := service.store.Remove(stored.Version.StorageRelativePath); err != nil {
			result.FailedFiles++
			_ = service.repository.ReleaseCleanupClaim(ctx, stored.File.TenantID, stored.File.ID, stored.File.Status, service.clock.Now().UTC())
			continue
		}
		if err := service.repository.MarkDeleted(ctx, stored.File.TenantID, stored.File.ID, service.clock.Now().UTC()); err != nil {
			// 二进制已删除但数据库推进失败时不能恢复文件；记录失败计数，DELETING 元数据需由后续对账修复。
			result.FailedFiles++
			continue
		}
		result.DeletedFiles++
	}
	removed, err := service.store.CleanupTemporary(cutoff.UTC())
	if err != nil {
		result.TempCleanupFailure = 1
		return result, err
	}
	result.RemovedTempFiles = removed
	return result, nil
}

// ReconcileStaleUploads 恢复早于截止时间且停留在 PENDING_UPLOAD/VALIDATING 的文件。
// PENDING_UPLOAD 已有完整落盘文件时重算摘要并继续校验；文件缺失或基础设施读取失败时标记
// FAILED；内容不合规时标记 REJECTED。limit 保证单次操作不会无界占用磁盘 I/O。
func (service *FileService) ReconcileStaleUploads(ctx context.Context, tenantID string, cutoff time.Time, limit int) (domain.ReconcileResult, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || cutoff.IsZero() || limit <= 0 || limit > 500 {
		return domain.ReconcileResult{}, validation("tenant_id, reconciliation cutoff and limit (1..500) are required")
	}
	candidates, err := service.repository.ListRecoveryCandidates(ctx, tenantID, cutoff.UTC(), limit)
	if err != nil {
		return domain.ReconcileResult{}, err
	}
	result := domain.ReconcileResult{Inspected: len(candidates)}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		stream, openErr := service.store.OpenVerified(candidate.Version.StorageRelativePath)
		if openErr != nil {
			if markErr := service.repository.MarkFailed(ctx, tenantID, candidate.File.ID, service.clock.Now().UTC()); markErr != nil && !errors.Is(markErr, ErrConflict) {
				return result, markErr
			}
			result.Failed++
			continue
		}

		if candidate.File.Status == domain.FileStatusPendingUpload {
			size, digest, integrityErr := calculateStoredIntegrity(stream)
			if integrityErr != nil {
				_ = stream.Close()
				_ = service.repository.MarkFailed(ctx, tenantID, candidate.File.ID, service.clock.Now().UTC())
				result.Failed++
				continue
			}
			if transitionErr := service.repository.MarkValidating(ctx, tenantID, candidate.File.ID, size, digest, service.clock.Now().UTC()); transitionErr != nil {
				_ = stream.Close()
				if errors.Is(transitionErr, ErrConflict) || errors.Is(transitionErr, ErrNotFound) {
					result.Conflicts++
					continue
				}
				return result, transitionErr
			}
		} else if integrityErr := verifyStoredIntegrity(stream, candidate.Version.SizeBytes, candidate.Version.SHA256); integrityErr != nil {
			_ = stream.Close()
			_ = service.repository.MarkFailed(ctx, tenantID, candidate.File.ID, service.clock.Now().UTC())
			result.Failed++
			continue
		}

		validationErr := validateStoredContent(candidate.Version.MediaType, stream)
		closeErr := stream.Close()
		if validationErr != nil {
			_ = service.store.Remove(candidate.Version.StorageRelativePath)
			if markErr := service.repository.MarkRejected(ctx, tenantID, candidate.File.ID, service.clock.Now().UTC()); markErr != nil && !errors.Is(markErr, ErrConflict) && !errors.Is(markErr, ErrNotFound) {
				return result, markErr
			}
			result.Rejected++
			continue
		}
		if closeErr != nil {
			_ = service.repository.MarkFailed(ctx, tenantID, candidate.File.ID, service.clock.Now().UTC())
			result.Failed++
			continue
		}
		if transitionErr := service.repository.MarkReady(ctx, tenantID, candidate.File.ID, service.clock.Now().UTC()); transitionErr != nil {
			if errors.Is(transitionErr, ErrConflict) || errors.Is(transitionErr, ErrNotFound) {
				result.Conflicts++
				continue
			}
			return result, transitionErr
		}
		result.Recovered++
	}
	return result, nil
}

func (service *FileService) resolveMediaType(declared, extension string, header []byte) (string, error) {
	detected := canonicalDetectedMediaType(http.DetectContentType(header))
	declaredCanonical := ""
	if strings.TrimSpace(declared) != "" {
		var err error
		declaredCanonical, err = canonicalMediaType(declared)
		if err != nil {
			return "", validation("declared media type is invalid")
		}
	}
	extensionMediaType := ""
	if extension != "" {
		if value := mime.TypeByExtension("." + extension); value != "" {
			extensionMediaType, _ = canonicalMediaType(value)
		}
	}

	// CSV 常被内容探测识别为 text/plain；只有扩展名和声明类型同时确认 CSV 时才修正。
	// 其他“声明/扩展名/内容”不一致一律拒绝，防止仅靠客户端 MIME 绕过白名单。
	if extension == "csv" && detected == "text/plain" && declaredCanonical == "text/csv" {
		detected = "text/csv"
	}
	if declaredCanonical != "" && detected != "application/octet-stream" && declaredCanonical != detected {
		return "", validation("declared media type does not match file content")
	}
	if extensionMediaType != "" && detected != "application/octet-stream" && extensionMediaType != detected && !(extension == "csv" && detected == "text/plain") {
		return "", validation("file extension does not match file content")
	}
	if _, allowed := service.policy.AllowedMediaTypes[detected]; !allowed {
		return "", validation("file media type is not allowed")
	}
	return detected, nil
}

func validateUploadInput(input UploadInput) error {
	tenantID := strings.TrimSpace(input.TenantID)
	applicationID := strings.TrimSpace(input.ApplicationID)
	// owner_user_id 仅在浏览器用户上传时必填；File Gateway 的机器令牌没有平台用户主体，
	// 机器上传的文件归属为空（owner_user_id NULL），由 application_id 与绑定关系承载归属。
	if tenantID == "" || applicationID == "" || input.Content == nil {
		return validation("tenant_id, application_id and file are required")
	}
	if !validStorageSegment(tenantID) || !validStorageSegment(applicationID) {
		return validation("tenant_id or application_id contains an unsafe storage path segment")
	}
	classification := strings.ToUpper(strings.TrimSpace(input.Classification))
	if classification != "" {
		if _, ok := classificationValues[classification]; !ok {
			return validation("classification is invalid")
		}
	}
	return nil
}

// validStorageSegment 在创建元数据前阻止租户或应用标识形成路径穿越片段。平台同时使用 ULID
// 与配置型应用编码，因此允许普通标点，但路径分隔符、控制字符及 . / .. 明确禁止。
func validStorageSegment(value string) bool {
	if len(value) > 128 || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, runeValue := range value {
		if unicode.IsControl(runeValue) || runeValue == 0 {
			return false
		}
	}
	return true
}

func storageRelativePath(tenantID, applicationID string, now time.Time, fileID, versionID string) string {
	return filepath.ToSlash(filepath.Join(strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", int(now.Month())), fileID, versionID+".bin"))
}

func safeOriginalName(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	var builder strings.Builder
	for _, runeValue := range value {
		if unicode.IsControl(runeValue) || runeValue == '/' || runeValue == '\\' || runeValue == 0 {
			builder.WriteRune('_')
			continue
		}
		builder.WriteRune(runeValue)
	}
	name := strings.Trim(strings.TrimSpace(builder.String()), ".")
	if name == "" || name == "." || name == ".." {
		return "", ""
	}
	for len([]byte(name)) > 512 {
		_, width := utf8DecodeLastRune(name)
		name = name[:len(name)-width]
	}
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if len(extension) > 32 || !validExtension(extension) {
		extension = ""
	}
	return name, extension
}

func utf8DecodeLastRune(value string) (rune, int) {
	for index := len(value) - 1; index >= 0; index-- {
		if value[index]&0xc0 != 0x80 {
			return rune(value[index]), len(value) - index
		}
	}
	return 0, 1
}

func validExtension(value string) bool {
	for _, runeValue := range value {
		if !((runeValue >= 'a' && runeValue <= 'z') || (runeValue >= '0' && runeValue <= '9')) {
			return false
		}
	}
	return value != ""
}

func peekContent(source io.Reader) ([]byte, io.Reader, error) {
	buffer := make([]byte, 512)
	count, err := io.ReadFull(source, buffer)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	return buffer[:count], io.MultiReader(bytes.NewReader(buffer[:count]), source), nil
}

func canonicalMediaType(value string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil || mediaType == "" {
		return "", err
	}
	return strings.ToLower(mediaType), nil
}

func canonicalDetectedMediaType(value string) string {
	mediaType, err := canonicalMediaType(value)
	if err != nil {
		return "application/octet-stream"
	}
	return mediaType
}

func hasPermission(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }

// StableAllowedMediaTypes returns the policy list in deterministic order for management UIs.
func (service *FileService) StableAllowedMediaTypes() []string {
	result := make([]string, 0, len(service.policy.AllowedMediaTypes))
	for mediaType := range service.policy.AllowedMediaTypes {
		result = append(result, mediaType)
	}
	sort.Strings(result)
	return result
}

// VerifyDigest is small but intentionally exported for worker-side integrity checks after restore.
func VerifyDigest(reader io.Reader, expected []byte) (bool, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return false, err
	}
	return bytes.Equal(hash.Sum(nil), expected), nil
}
