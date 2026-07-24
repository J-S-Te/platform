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

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/domain"
)

const defaultUploadMaxBytes int64 = 20 << 20

var (
	classificationValues = map[string]struct{}{"INTERNAL": {}, "CONFIDENTIAL": {}, "RESTRICTED": {}}
	jobTypePattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

// FileService owns the state machine that bridges durable metadata and local binary storage.
type FileService struct {
	repository FileRepository
	store      LocalStore
	ids        IDGenerator
	clock      Clock
	policy     UploadPolicy
}

// NewFileService creates a file service with an explicit policy. No default "accept everything"
// mode exists: a non-positive maximum and an empty MIME allowlist are rejected.
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

// DefaultUploadPolicy is deliberately conservative. Business modules that need additional file
// types must extend the server-side policy explicitly rather than relying on client MIME values.
func DefaultUploadPolicy() UploadPolicy {
	return UploadPolicy{
		MaxBytes: defaultUploadMaxBytes,
		AllowedMediaTypes: map[string]struct{}{
			"application/pdf": {},
			"image/jpeg":      {},
			"image/png":       {},
			"text/plain":      {},
			"text/csv":        {},
		},
	}
}

// Upload creates WRITING metadata, atomically persists the binary, then promotes both records to
// AVAILABLE. A file-system failure marks metadata FAILED; a database promotion failure removes the
// newly written binary before returning the error.
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
		Status: domain.FileStatusUploading, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	version := domain.FileVersion{
		ID: versionID, FileID: fileID, VersionNo: 1, StorageRelativePath: relativePath,
		MediaType: mediaType, OriginalName: name, UploaderUserID: strings.TrimSpace(input.OwnerUserID),
		UploadRequestID: strings.TrimSpace(input.RequestID), Status: domain.FileVersionStatusWriting, CreatedAt: now,
	}
	if err := service.repository.CreateWriting(ctx, file, version); err != nil {
		return domain.File{}, err
	}

	size, digest, err := service.store.WriteAtomically(ctx, relativePath, content, service.policy.MaxBytes)
	if err != nil {
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, fmt.Errorf("%w: write upload: %v", ErrStorage, err)
	}
	if err := service.repository.MarkAvailable(ctx, file.TenantID, file.ID, size, digest, service.clock.Now().UTC()); err != nil {
		_ = service.store.Remove(relativePath)
		_ = service.repository.MarkFailed(ctx, file.TenantID, file.ID, service.clock.Now().UTC())
		return domain.File{}, err
	}
	file.Status = domain.FileStatusAvailable
	file.UpdatedAt = service.clock.Now().UTC()
	return file, nil
}

// OpenDownload resolves an AVAILABLE file, enforces owner-or-permission access and opens a verified
// local handle. The caller must close the returned handle after streaming it.
func (service *FileService) OpenDownload(ctx context.Context, access DownloadAccess, fileID string) (domain.StoredFile, io.ReadSeekCloser, error) {
	if strings.TrimSpace(access.TenantID) == "" || strings.TrimSpace(access.UserID) == "" || strings.TrimSpace(fileID) == "" {
		return domain.StoredFile{}, nil, validation("download identity and file_id are required")
	}
	stored, err := service.repository.GetAvailable(ctx, strings.TrimSpace(access.TenantID), strings.TrimSpace(fileID))
	if err != nil {
		return domain.StoredFile{}, nil, err
	}
	if stored.File.OwnerUserID != access.UserID && !hasPermission(access.PermissionCodes, "platform:file:download") {
		return domain.StoredFile{}, nil, ErrForbidden
	}
	handle, err := service.store.OpenVerified(stored.Version.StorageRelativePath)
	if err != nil {
		return domain.StoredFile{}, nil, fmt.Errorf("%w: open local file: %v", ErrStorage, err)
	}
	return stored, handle, nil
}

// CleanupUnboundExpired deletes only unbound AVAILABLE files older than cutoff. The existing schema
// has no per-file expiration column, so callers supply a policy-derived cutoff; per-file expiration
// requires an additive migration documented in 文件与异步任务开发说明.md.
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

	// CSV is often detected as text/plain. Treat it as CSV only when the filename and declared type
	// both identify CSV; all other declared/detected differences are rejected conservatively.
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
	if tenantID == "" || applicationID == "" || strings.TrimSpace(input.OwnerUserID) == "" || input.Content == nil {
		return validation("tenant_id, application_id, owner_user_id and file are required")
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

// validStorageSegment prevents identity and application values from forming a path traversal
// segment before metadata is created. IDs are allowed to use ordinary punctuation because the
// platform stores both ULIDs and configured application codes.
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
