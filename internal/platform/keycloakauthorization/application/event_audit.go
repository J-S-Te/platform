package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
)

// KeycloakAuditEvent is the deliberately small, credential-free projection of
// a Keycloak user or admin event.  The raw Admin API response is never written
// to the platform audit store because it can contain credential-adjacent data.
type KeycloakAuditEvent struct {
	EventID, Category, Type, SubjectID, SessionID, ClientID, SourceIP string
	ResourceType, ResourcePath, OperationType                         string
	OccurredAt                                                        time.Time
}

type KeycloakAuditEventSource interface {
	ListKeycloakAuditEvents(context.Context, time.Time) ([]KeycloakAuditEvent, error)
}

type KeycloakAuditIdentityResolver interface {
	ResolveKeycloakAuditIdentity(context.Context, string) (tenantID, userID string, err error)
}

// KeycloakEventAuditCollector maps login/logout/admin evidence into the
// existing append-only platform audit service. It relies on audit EventID
// deduplication, so each polling cycle can safely include an overlap window.
type KeycloakEventAuditCollector struct {
	source      KeycloakAuditEventSource
	identities  KeycloakAuditIdentityResolver
	audit       *auditapplication.Service
	application string
	environment string
}

func NewKeycloakEventAuditCollector(source KeycloakAuditEventSource, identities KeycloakAuditIdentityResolver, audit *auditapplication.Service, applicationCode, environmentCode string) (*KeycloakEventAuditCollector, error) {
	if source == nil || identities == nil || audit == nil || strings.TrimSpace(applicationCode) == "" || strings.TrimSpace(environmentCode) == "" {
		return nil, errors.New("Keycloak audit collector dependencies are invalid")
	}
	return &KeycloakEventAuditCollector{source: source, identities: identities, audit: audit, application: strings.TrimSpace(applicationCode), environment: strings.TrimSpace(environmentCode)}, nil
}

// Collect performs one idempotent polling pass. Unknown/deleted Keycloak
// subjects are deliberately skipped: assigning such events to an arbitrary
// tenant would be worse than a visible warning and a controlled mapping repair.
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
			Result: "SUCCESS", RiskLevel: keycloakAuditRisk(event), Classification: "INTERNAL", EventCategory: "KEYCLOAK", EventType: strings.TrimSpace(event.Type),
			Summary: keycloakAuditSummary(event), Metadata: keycloakAuditMetadata(event), SourceIP: strings.TrimSpace(event.SourceIP),
		}
		if _, err := collector.audit.Ingest(ctx, tenantID, input); err != nil {
			return accepted, fmt.Errorf("ingest Keycloak audit event %s: %w", event.EventID, err)
		}
		accepted++
	}
	return accepted, nil
}

func supportedKeycloakAuditEvent(event KeycloakAuditEvent) bool {
	category, kind := strings.ToUpper(strings.TrimSpace(event.Category)), strings.ToUpper(strings.TrimSpace(event.Type))
	if category == "ADMIN" {
		return strings.TrimSpace(event.SubjectID) != ""
	}
	return category == "LOGIN" && (kind == "LOGIN" || kind == "LOGOUT") && strings.TrimSpace(event.SubjectID) != ""
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
	return "MEDIUM"
}

func keycloakAuditSummary(event KeycloakAuditEvent) string {
	if strings.EqualFold(strings.TrimSpace(event.Category), "ADMIN") {
		return "Keycloak 管理员事件：" + strings.ToUpper(strings.TrimSpace(event.OperationType))
	}
	return "Keycloak 用户会话事件：" + strings.ToUpper(strings.TrimSpace(event.Type))
}

func keycloakAuditMetadata(event KeycloakAuditEvent) map[string]any {
	return map[string]any{"provider": "keycloak", "category": strings.ToUpper(strings.TrimSpace(event.Category)), "event_type": strings.ToUpper(strings.TrimSpace(event.Type)), "resource_path": strings.TrimSpace(event.ResourcePath)}
}
