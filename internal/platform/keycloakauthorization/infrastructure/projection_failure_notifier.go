package infrastructure

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	notificationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

// ProjectionFailureNotifier 在 Keycloak 授权投影反复失败时发送站内通知。
// 它实现 keycloakauthorization/application 包的 FailureNotifier 接口。
type ProjectionFailureNotifier struct {
	notificationService *notificationapplication.Service
	logger              *slog.Logger
}

// NewProjectionFailureNotifier 构造经既有通知服务发送告警的通知器。
// logger 仅用于通知发送自身失败时的诊断输出。
func NewProjectionFailureNotifier(notificationService *notificationapplication.Service, logger *slog.Logger) (*ProjectionFailureNotifier, error) {
	if notificationService == nil {
		return nil, fmt.Errorf("projection failure notifier requires a notification service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ProjectionFailureNotifier{notificationService: notificationService, logger: logger}, nil
}

// NotifyProjectionFailure 就重复的 Keycloak 投影失败向租户管理员发送租户内站内
// 通知。使用 keycloak.projection.failed 模板编码；租户内不存在该模板时静默跳过。
func (notifier *ProjectionFailureNotifier) NotifyProjectionFailure(ctx context.Context, tenantID, applicationID, identityID string, err error) {
	if notifier == nil || notifier.notificationService == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return
	}
	idempotency := fmt.Sprintf("keycloak.projection.failure.%s.%s.%d", applicationID, identityID, time.Now().UTC().UnixNano()/int64(time.Minute))
	templateCode := "keycloak.projection.failed"
	// 有身份用户才发送，否则跳过：通知服务要求至少一个接收人。
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return
	}
	_, createErr := notifier.notificationService.Create(ctx, notificationapplication.CreateInput{
		TenantID:     tenantID,
		OperatorID:   "system-keycloak",
		TemplateCode: templateCode,
		Category:     "keycloak_sync",
		Variables: map[string]string{
			"application_id": applicationID,
			"identity_id":    identityID,
			"error":          trimError(err, 500),
		},
		Recipients:     []notificationdomain.RecipientTarget{{Type: notificationdomain.RecipientTypeUser, ID: identityID}},
		IdempotencyKey: idempotency,
	})
	if createErr != nil {
		notifier.logger.Warn("failed to send projection failure notification",
			"tenant_id", tenantID, "application_id", applicationID,
			"identity_id", identityID, "error", createErr)
	}
}

func trimError(err error, maxLen int) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxLen {
		return msg[:maxLen] + "…"
	}
	return msg
}
