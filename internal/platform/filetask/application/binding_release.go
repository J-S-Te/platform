package application

import (
	"context"
	"fmt"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

// BindingReleaseRepository 持久化终态引用墓碑。此操作与删除分离，
// 不可变或长期证据仍受其保留策略约束。
type BindingReleaseRepository interface {
	ReleaseBinding(context.Context, domain.FileBinding) error
}

func (service *FileService) ReleaseResourceBinding(ctx context.Context, input BindingInput) error {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.FileID = strings.TrimSpace(input.FileID)
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	input.ResourceID = strings.TrimSpace(input.ResourceID)
	input.BindingType = strings.TrimSpace(input.BindingType)
	input.OperatorUserID = strings.TrimSpace(input.OperatorUserID)
	if input.TenantID == "" || input.ApplicationID == "" || input.FileID == "" || input.ResourceType == "" || input.ResourceID == "" || input.BindingType == "" || input.OperatorUserID == "" {
		return validation("binding retirement tenant, application, file, resource, type and operator are required")
	}
	if len(input.ResourceType) > 64 || len(input.ResourceID) > 128 || len(input.BindingType) > 64 {
		return validation("binding retirement field exceeds the configured limit")
	}
	repository, ok := service.repository.(BindingReleaseRepository)
	if !ok {
		return fmt.Errorf("%w: durable binding retirement is unavailable", ErrStorage)
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return fmt.Errorf("generate binding retirement ID: %w", err)
	}
	return repository.ReleaseBinding(ctx, domain.FileBinding{ID: id, TenantID: input.TenantID, ApplicationID: input.ApplicationID, FileID: input.FileID, ResourceType: input.ResourceType, ResourceID: input.ResourceID, BindingType: input.BindingType, Status: "DISABLED", CreatedBy: input.OperatorUserID, CreatedAt: now})
}
