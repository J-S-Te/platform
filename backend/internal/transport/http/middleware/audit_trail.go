// Package middleware contains transport-level HTTP middleware shared by all modules.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const defaultPlatformAuditApplicationCode = "platform"
const defaultPlatformAuditEnvironmentCode = "dev"

// AuditSource identifies the configured application environment for server-generated events.
type AuditSource struct {
	ApplicationCode string
	EnvironmentCode string
}

// AuditRecorder is the narrow audit application contract used by HTTP middleware. Keeping this
// interface at the transport boundary prevents the router from depending on persistence details.
type AuditRecorder interface {
	Ingest(context.Context, string, auditapplication.EventInput) (auditdomain.Receipt, error)
}

// AuditTrail records successful, failed, and denied platform write operations. It also records
// audit-console queries and export job access, because those operations disclose security data.
// The external audit ingestion endpoints are intentionally excluded to avoid generating duplicate
// records for an event that is already being persisted by the audit application service.
func AuditTrail(recorder AuditRecorder, logger *slog.Logger, sources ...AuditSource) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	source := AuditSource{ApplicationCode: defaultPlatformAuditApplicationCode, EnvironmentCode: defaultPlatformAuditEnvironmentCode}
	if len(sources) > 0 {
		if value := strings.TrimSpace(sources[0].ApplicationCode); value != "" {
			source.ApplicationCode = value
		}
		if value := strings.TrimSpace(sources[0].EnvironmentCode); value != "" {
			source.EnvironmentCode = value
		}
	}

	return func(context *gin.Context) {
		context.Next()

		if recorder == nil || context.Request == nil {
			return
		}

		route := context.FullPath()
		if route == "" {
			route = context.Request.URL.Path
		}
		if !shouldRecordAuditTrail(context.Request.Method, route) {
			return
		}

		principal, ok := authctx.PrincipalFromContext(context.Request.Context())
		if !ok || strings.TrimSpace(principal.Tenant.ID) == "" {
			return
		}

		statusCode := context.Writer.Status()
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		now := time.Now().UTC()
		input := auditapplication.EventInput{
			EventID:         newPlatformAuditEventID(),
			ApplicationCode: source.ApplicationCode,
			EnvironmentCode: source.EnvironmentCode,
			EventCategory:   "PLATFORM",
			EventType:       context.Request.Method + " " + route,
			OccurredAt:      now,
			ActorType:       "USER",
			ActorID:         principal.User.ID,
			ActorName:       principal.User.Name,
			SessionID:       principal.SessionID,
			Action:          context.Request.Method + " " + route,
			ResourceType:    auditResourceType(route),
			ResourceID:      auditResourceID(context),
			RequestID:       requestctx.RequestID(context.Request.Context()),
			TraceID:         requestctx.TraceID(context.Request.Context()),
			Result:          auditResult(statusCode),
			RiskLevel:       auditRiskLevel(context.Request.Method, route, statusCode),
			Classification:  "INTERNAL",
			Summary:         auditSummary(context.Request.Method, route, statusCode),
			Metadata: map[string]any{
				"method":      context.Request.Method,
				"path":        route,
				"status_code": statusCode,
			},
			SourceIP:  RequestClientIP(context.Request),
			UserAgent: context.Request.UserAgent(),
		}
		if _, err := recorder.Ingest(context.Request.Context(), principal.Tenant.ID, input); err != nil {
			logger.Error("write platform audit event", "error", err, "request_id", input.RequestID, "path", route)
		}
	}
}

func shouldRecordAuditTrail(method, route string) bool {
	if method == http.MethodPost && (route == "/api/v1/audit/events" || route == "/api/v1/audit/events/batch") {
		return false
	}
	// Successful user access writes and subject-access revocations emit a richer business audit
	// event after persistence. Keep rejected subject PUTs in the generic trail so denied attempts
	// are still visible.
	if strings.HasPrefix(route, "/api/v1/users/") && strings.Contains(route, "/applications/:application_code/access") && (method == http.MethodPut || method == http.MethodDelete) {
		return false
	}
	if strings.HasPrefix(route, "/api/v1/authorization-subjects/") && strings.Contains(route, "/applications/:application_code/access") && method == http.MethodDelete {
		return false
	}
	if strings.Contains(route, "/authorization-catalog") && method == http.MethodPut {
		return false
	}
	if strings.HasPrefix(route, "/api/v1/audit/") {
		return true
	}
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func auditResourceType(route string) string {
	path := strings.TrimPrefix(route, "/api/v1/")
	segment, _, _ := strings.Cut(path, "/")
	switch segment {
	case "auth", "users", "accounts", "org-units", "positions", "memberships":
		return "IDENTITY"
	case "resources", "permissions", "roles", "role-bindings", "authorization", "authorization-subjects", "position-authorization-templates":
		return "AUTHORIZATION"
	case "config":
		return "CONFIGURATION"
	case "audit":
		return "AUDIT"
	default:
		return "PLATFORM_API"
	}
}

func auditResourceID(context *gin.Context) string {
	for index := len(context.Params) - 1; index >= 0; index-- {
		parameter := context.Params[index]
		if strings.HasSuffix(parameter.Key, "_id") {
			return parameter.Value
		}
	}
	return ""
}

func auditResult(statusCode int) string {
	switch {
	case statusCode >= http.StatusOK && statusCode < http.StatusBadRequest:
		return "SUCCESS"
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return "DENIED"
	default:
		return "FAILURE"
	}
}

func auditRiskLevel(method, route string, statusCode int) string {
	normalized := strings.ToLower(route)
	if statusCode >= http.StatusInternalServerError {
		return "HIGH"
	}
	if containsAny(normalized, "role-bindings", "/roles", "/permissions", "/authorization", "credentials/rotate", "credentials/:credential_id", "password", "reset", "/oauth-clients", "authorization-catalog", "position-authorization-templates") {
		return "HIGH"
	}
	if method == http.MethodDelete && containsAny(normalized, "/users/", "/applications/", "/environments/") {
		return "HIGH"
	}
	if strings.HasPrefix(normalized, "/api/v1/audit/") || containsAny(normalized, "/accounts/", "/security/", "/settings") || statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return "MEDIUM"
	}
	return "LOW"
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func auditSummary(method, route string, statusCode int) string {
	return method + " " + route + " completed with HTTP " + http.StatusText(statusCode)
}

func newPlatformAuditEventID() string {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err == nil {
		return "platform-" + hex.EncodeToString(entropy[:])
	}
	return "platform-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
}
