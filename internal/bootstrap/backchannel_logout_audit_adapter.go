package bootstrap

import (
	"context"
	"errors"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// backchannelLogoutAuditAdapter 把接入阶段的 Back-Channel Logout 决定翻译成平台追加式
// 审计事件（BIZ-XEVT2-07）。它只做字段映射，不改变决定本身：决定是否成立、是否需要
// 失败关闭，始终由接入应用服务判定。
//
// 事件必须经审计模块既有 ingestion（auditapplication.Service.Ingest）落库，适配器绝不
// 直接写 audit_event；摄取失败原样返回错误，让接入失败关闭，而不是留下无法追责的决定。
type backchannelLogoutAuditAdapter struct {
	service *auditapplication.Service
	config  config.AuditConfig
	ids     ulid.Generator
}

// RecordBackchannelLogoutDecision 写入一条 Back-Channel Logout 决定事件。适配器未装配
// 时返回错误而不是静默丢弃：opt-out 与"端点校验失败被跳过"都意味着该 RP 收不到登出通知。
func (adapter *backchannelLogoutAuditAdapter) RecordBackchannelLogoutDecision(ctx context.Context, event applicationregistryapplication.BackchannelLogoutAuditEvent) error {
	if adapter == nil || adapter.service == nil {
		return errors.New("back-channel logout audit adapter is not configured")
	}
	now := time.Now().UTC()
	eventID, err := adapter.ids.New(now)
	if err != nil {
		return err
	}
	// 空 OperatorID 表示后台补偿或系统任务；有操作者时才记为 USER，不伪造占位用户。
	actorType := "SYSTEM"
	if strings.TrimSpace(event.OperatorID) != "" {
		actorType = "USER"
	}
	_, err = adapter.service.Ingest(ctx, event.TenantID, auditapplication.EventInput{
		EventID:         eventID,
		ApplicationCode: adapter.config.ApplicationCode,
		EnvironmentCode: adapter.config.EnvironmentCode,
		ActorType:       actorType,
		ActorID:         strings.TrimSpace(event.OperatorID),
		OccurredAt:      now,
		Action:          "UPDATE",
		ResourceType:    "OAUTH_BACKCHANNEL_LOGOUT",
		ResourceID:      strings.TrimSpace(event.OAuthClientID),
		Result:          "SUCCESS",
		ReasonCode:      strings.TrimSpace(event.Reason),
		RiskLevel:       "HIGH",
		Classification:  "INTERNAL",
		Summary:         backchannelLogoutAuditSummary(event),
		Metadata:        backchannelLogoutAuditMetadata(event),
		EventCategory:   "AUTHORIZATION_CHANGE",
		EventType:       "BACKCHANNEL_LOGOUT_DECISION",
	})
	return err
}

// backchannelLogoutAuditSummary 为两种"本应通知却无端点"的决定生成可读摘要；摘要只描述
// 决定与影响，不含凭据、tenant 内部 ID 之外的运行细节。
func backchannelLogoutAuditSummary(event applicationregistryapplication.BackchannelLogoutAuditEvent) string {
	application := strings.TrimSpace(event.ApplicationCode)
	environment := strings.TrimSpace(event.Environment)
	if application == "" || environment == "" {
		return "接入未登记 Back-Channel Logout 端点，该 RP 无法接收登出通知"
	}
	switch event.Decision {
	case applicationregistryapplication.BackchannelLogoutDecisionOptOut:
		return "接入未登记 Back-Channel Logout 端点（调用方显式 opt-out）：" + application + "/" + environment + " 无法接收登出通知"
	case applicationregistryapplication.BackchannelLogoutDecisionSkipped:
		return "接入跳过 Back-Channel Logout 端点登记（端点未通过协议校验）：" + application + "/" + environment + " 无法接收登出通知"
	}
	return "接入 Back-Channel Logout 决定：" + application + "/" + environment
}

// backchannelLogoutAuditMetadata 组装可机器检索的审计元数据。missing_logout_uri 是与
// 投递期 NO_ENDPOINT 原因码共用的固定键（见 backchannel.MissingLogoutURIMetadata），
// 即使调用方漏填也在这里补齐，保证告警规则不会因为字段缺失而漏聚合。
func backchannelLogoutAuditMetadata(event applicationregistryapplication.BackchannelLogoutAuditEvent) map[string]any {
	metadata := make(map[string]any, len(event.Metadata)+8)
	// 复制调用方 map：适配器补充审计字段时不能反向修改业务事件或引发并发 map 写入。
	for key, value := range event.Metadata {
		metadata[key] = value
	}
	metadata[backchannel.ReasonMissingLogoutURI] = true
	if reason := strings.TrimSpace(event.Reason); reason != "" {
		metadata["reason"] = reason
	}
	metadata["decision"] = strings.TrimSpace(event.Decision)
	metadata["application_code"] = strings.TrimSpace(event.ApplicationCode)
	metadata["environment"] = strings.TrimSpace(event.Environment)
	metadata["oauth_client_id"] = strings.TrimSpace(event.OAuthClientID)
	if clientID := strings.TrimSpace(event.ClientID); clientID != "" {
		metadata["client_id"] = clientID
	}
	// 端点地址是公开回调（不含凭据）；只有确实拿到地址时才写入，opt-out 不制造空字段。
	if logoutURI := strings.TrimSpace(event.LogoutURI); logoutURI != "" {
		metadata["logout_uri"] = logoutURI
	}
	return metadata
}
