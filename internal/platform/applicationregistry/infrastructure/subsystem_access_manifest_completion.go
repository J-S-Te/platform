package infrastructure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// recordAppliedAccessManifest 用途：在同步 Agent 成功后持久化实际清单，沿用仓库的期望摘要并发校验。
// 参数：ctx 为执行上下文，operation 为已授权目标，checksum 为 Agent 接受并完成的摘要。
// 返回值：未配置清单存储或空摘要返回 nil，持久化失败返回错误且不得完成阶段。
func (adapter *SubsystemAccessDeploymentAdapter) recordAppliedAccessManifest(ctx context.Context, operation application.SubsystemAccessOperation, checksum string) error {
	if adapter.manifestState == nil || strings.TrimSpace(checksum) == "" {
		return nil
	}
	if err := adapter.manifestState.MarkSubsystemManifestApplied(ctx, operation.TenantID,
		operation.ApplicationCode, operation.EnvironmentCode, strings.TrimSpace(checksum), time.Now().UTC()); err != nil {
		return fmt.Errorf("record applied manifest after Agent completion: %w", err)
	}
	return nil
}
