package application

import (
	"context"
	"fmt"
	"strings"
)

// UploadRequestResolver 只暴露元数据。解析不会重新开放
// 失败的不可变上传，也不会写入任何文件字节。
type UploadRequestResolver interface {
	LookupUploadRequest(context.Context, string, string, string) (string, string, error)
}

func (service *FileService) ResolveUploadRequest(ctx context.Context, tenantID, applicationID, requestID string) (string, string, error) {
	tenantID, applicationID, requestID = strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(requestID)
	if tenantID == "" || applicationID == "" {
		return "", "", ErrForbidden
	}
	if requestID == "" || len(requestID) > 128 {
		return "", "", validation("upload request identity is required and must not exceed 128 bytes")
	}
	repository, ok := service.repository.(UploadRequestResolver)
	if !ok {
		return "", "", fmt.Errorf("%w: upload request resolution is unavailable", ErrStorage)
	}
	return repository.LookupUploadRequest(ctx, tenantID, applicationID, requestID)
}

func (service *FileService) RetireUploadRequest(ctx context.Context, tenantID, appID, requestID string) (string, string, error) {
	tenantID, appID, requestID = strings.TrimSpace(tenantID), strings.TrimSpace(appID), strings.TrimSpace(requestID)
	if tenantID == "" || appID == "" {
		return "", "", ErrForbidden
	}
	if requestID == "" || len(requestID) > 128 {
		return "", "", validation("upload request identity is required and must not exceed 128 bytes")
	}
	repository, ok := service.repository.(interface {
		RetireUploadRequest(context.Context, string, string, string) (string, string, error)
	})
	if !ok {
		return "", "", fmt.Errorf("%w: upload request retirement is unavailable", ErrStorage)
	}
	return repository.RetireUploadRequest(ctx, tenantID, appID, requestID)
}
