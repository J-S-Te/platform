package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
)

// KeycloakAuditEvent 是 Keycloak 用户/管理员事件的刻意精简投影，不含任何凭据。
// Admin API 原始响应绝不写入平台审计存储——其中可能包含凭据相邻数据。
type KeycloakAuditEvent struct {
	EventID, Category, Type, SubjectID, SessionID, ClientID, SourceIP string
	ResourceType, ResourcePath, OperationType                         string
	OccurredAt                                                        time.Time
}

// KeycloakAuditEventSource 提供 Keycloak Admin API 的事件拉取入口；实现方负责把
// 原始响应裁剪为 KeycloakAuditEvent，凭据相邻字段不得进入平台审计存储。
type KeycloakAuditEventSource interface {
	ListKeycloakAuditEvents(context.Context, time.Time) ([]KeycloakAuditEvent, error)
}

// KeycloakAuditIdentityResolver 把 Keycloak 主体 ID 映射回租户与平台用户；
// 无法唯一映射时返回空租户，由采集方跳过该事件。
type KeycloakAuditIdentityResolver interface {
	ResolveKeycloakAuditIdentity(context.Context, string) (tenantID, userID string, err error)
}

// KeycloakEventAuditCollector 把登录/登出/管理员事件证据映射进既有的平台审计
// 服务（只追加写入）。它依赖审计 EventID 去重，因此每个轮询周期可以安全地
// 包含一段重叠时间窗。
type KeycloakEventAuditCollector struct {
	source      KeycloakAuditEventSource
	identities  KeycloakAuditIdentityResolver
	audit       *auditapplication.Service
	application string
	environment string
}

// NewKeycloakEventAuditCollector 构造事件采集器；任一依赖为空或应用/环境编码
// 缺失时返回错误。
func NewKeycloakEventAuditCollector(source KeycloakAuditEventSource, identities KeycloakAuditIdentityResolver, audit *auditapplication.Service, applicationCode, environmentCode string) (*KeycloakEventAuditCollector, error) {
	if source == nil || identities == nil || audit == nil || strings.TrimSpace(applicationCode) == "" || strings.TrimSpace(environmentCode) == "" {
		return nil, errors.New("Keycloak audit collector dependencies are invalid")
	}
	return &KeycloakEventAuditCollector{source: source, identities: identities, audit: audit, application: strings.TrimSpace(applicationCode), environment: strings.TrimSpace(environmentCode)}, nil
}

// Collect 执行一次幂等的轮询采集。未知/已删除的 Keycloak 主体被刻意跳过：
// 把这类事件强行归入任意租户，比给出可见告警并做受控映射修复更糟。
func (collector *KeycloakEventAuditCollector) Collect(ctx context.Context, since time.Time) (int, error) {
	events, err := collector.source.ListKeycloakAuditEvents(ctx, since.UTC())
	if err != nil {
		return 0, err
	}
	accepted := 0
	for _, event := range events {
		if !supportedKeycloakAuditEvent(event) {
			continue
		}
		tenantID, userID, resolveErr := collector.identities.ResolveKeycloakAuditIdentity(ctx, event.SubjectID)
		if resolveErr != nil || strings.TrimSpace(tenantID) == "" {
			continue
		}
		if event.OccurredAt.IsZero() || strings.TrimSpace(event.EventID) == "" {
			continue
		}
		input := auditapplication.EventInput{
			EventID: "keycloak:" + strings.TrimSpace(event.EventID), ApplicationCode: collector.application, EnvironmentCode: collector.environment,
			ActorType: keycloakAuditActorType(event.Category), ActorID: userID, SessionID: strings.TrimSpace(event.SessionID), ClientID: strings.TrimSpace(event.ClientID),
			OccurredAt: event.OccurredAt.UTC(), Action: keycloakAuditAction(event), ResourceType: keycloakAuditResourceType(event), ResourceID: strings.TrimSpace(event.ResourcePath),
			Result: keycloakAuditResult(event), RiskLevel: keycloakAuditRisk(event), Classification: "INTERNAL", EventCategory: "KEYCLOAK", EventType: strings.TrimSpace(event.Type),
			Summary: keycloakAuditSummary(event), Metadata: keycloakAuditMetadata(event), SourceIP: strings.TrimSpace(event.SourceIP),
		}
		if _, err := collector.audit.Ingest(ctx, tenantID, input); err != nil {
			return accepted, fmt.Errorf("ingest Keycloak audit event %s: %w", event.EventID, err)
		}
		accepted++
	}
	return accepted, nil
}

// supportedKeycloakAuditEvent 接受登录成功、登录失败、退出与管理员事件。
//
// 批次 3：此前只采 LOGIN/LOGOUT，登录失败（LOGIN_ERROR）完全不入审计——
// 恰恰是安全排查最需要的一类事件。
func supportedKeycloakAuditEvent(event KeycloakAuditEvent) bool {
	category, kind := strings.ToUpper(strings.TrimSpace(event.Category)), strings.ToUpper(strings.TrimSpace(event.Type))
	if category == "ADMIN" {
		return strings.TrimSpace(event.SubjectID) != ""
	}
	if category != "LOGIN" {
		return false
	}
	return kind == "LOGIN" || kind == "LOGOUT" || kind == "LOGIN_ERROR"
}

// keycloakAuditResult 把 Keycloak 事件类型映射为审计结果。
func keycloakAuditResult(event KeycloakAuditEvent) string {
	if strings.EqualFold(strings.TrimSpace(event.Type), "LOGIN_ERROR") {
		return "FAILURE"
	}
	return "SUCCESS"
}

func keycloakAuditActorType(category string) string {
	if strings.EqualFold(strings.TrimSpace(category), "ADMIN") {
		return "USER"
	}
	return "USER"
}

func keycloakAuditAction(event KeycloakAuditEvent) string {
	if strings.EqualFold(strings.TrimSpace(event.Category), "ADMIN") {
		return "keycloak.admin." + strings.ToLower(strings.TrimSpace(event.OperationType))
	}
	return "keycloak.session." + strings.ToLower(strings.TrimSpace(event.Type))
}

func keycloakAuditResourceType(event KeycloakAuditEvent) string {
	if strings.TrimSpace(event.ResourceType) != "" {
		return "keycloak." + strings.ToLower(strings.TrimSpace(event.ResourceType))
	}
	if strings.EqualFold(strings.TrimSpace(event.Category), "ADMIN") {
		return "keycloak.admin_resource"
	}
	return "keycloak.session"
}

func keycloakAuditRisk(event KeycloakAuditEvent) string {
	if strings.EqualFold(strings.TrimSpace(event.Category), "ADMIN") {
		return "HIGH"
	}
	// 登录失败按高风险记录：它是撞库/密码喷洒排查的主要线索。
	if strings.EqualFold(strings.TrimSpace(event.Type), "LOGIN_ERROR") {
		return "HIGH"
	}
	return "MEDIUM"
}

func keycloakAuditSummary(event KeycloakAuditEvent) string {
	if strings.EqualFold(strings.TrimSpace(event.Category), "ADMIN") {
		return "Keycloak 管理员事件：" + strings.ToUpper(strings.TrimSpace(event.OperationType))
	}
	if strings.EqualFold(strings.TrimSpace(event.Type), "LOGIN_ERROR") {
		return "Keycloak 登录失败：" + strings.ToUpper(strings.TrimSpace(event.Type))
	}
	return "Keycloak 用户会话事件：" + strings.ToUpper(strings.TrimSpace(event.Type))
}

func keycloakAuditMetadata(event KeycloakAuditEvent) map[string]any {
	return map[string]any{"provider": "keycloak", "category": strings.ToUpper(strings.TrimSpace(event.Category)), "event_type": strings.ToUpper(strings.TrimSpace(event.Type)), "resource_path": strings.TrimSpace(event.ResourcePath)}
}
