// Package middleware contains transport-level HTTP middleware shared by all modules.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
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

// AuditTrail 在处理器完成后记录成功、失败和拒绝的写操作，也记录会泄露安全信息的审计查询/导出。
// 外部审计摄取接口有意排除，因为其载荷本身就是待保存事件，再生成一条平台审计会造成重复。
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
		// 必须先执行后续链，才能采集最终路由模板、HTTP 状态和授权拒绝结果。
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
		requestSourceIP := RequestClientIP(context.Request)
		sourceIP := loginIP(principal.LoginIP)
		if sourceIP == "" {
			sourceIP = requestSourceIP
		}
		metadata := map[string]any{
			"method":      context.Request.Method,
			"path":        route,
			"status_code": statusCode,
		}
		if requestSourceIP != "" && requestSourceIP != sourceIP {
			metadata["request_source_ip"] = requestSourceIP
		}
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
			Metadata:        metadata,
			SourceIP:        sourceIP,
			UserAgent:       context.Request.UserAgent(),
		}
		if _, err := recorder.Ingest(context.Request.Context(), principal.Tenant.ID, input); err != nil {
			logger.Error("write platform audit event", "error", err, "request_id", input.RequestID, "path", route)
		}
	}
}

// loginIP returns only a normalized IP literal from the server-verified session
// principal. It never falls back to a request header; that remains the transport
// middleware's responsibility.
func loginIP(value net.IP) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func shouldRecordAuditTrail(method, route string) bool {
	if method == http.MethodPost && (route == "/api/v1/audit/events" || route == "/api/v1/audit/events/batch") {
		return false
	}
	// 用户应用授权等操作在持久化成功后会写更完整的业务审计，这里避免成功事件重复；
	// 被拒绝的尝试仍由通用轨迹保留，确保攻击和误操作可见。
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
