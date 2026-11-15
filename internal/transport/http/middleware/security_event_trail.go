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

	"github.com/J-S-Te/Basic-Platform/internal/platform/securityevent"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

type SecurityEventRecorder interface {
	Record(context.Context, securityevent.Event) error
}

// FailureTrail 必须在平台 Authentication 之前挂载：认证中间件会直接 Abort 401，
// 之后的 AuditTrail 不再可见。这里只记录未认证请求，已认证的 403 仍由 AuditTrail 记录。
func SecurityEventTrail(recorder SecurityEventRecorder, appCode, envCode string, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if recorder == nil || c.Writer.Status() != http.StatusUnauthorized && c.Writer.Status() != http.StatusForbidden {
			return
		}
		if _, ok := authctx.PrincipalFromContext(c.Request.Context()); ok {
			return
		}
		status := c.Writer.Status()
		event := securityevent.Event{
			EventID: securityEventID(), ApplicationCode: strings.TrimSpace(appCode), EnvironmentCode: strings.TrimSpace(envCode),
			OccurredAt: time.Now().UTC(), ReceivedAt: time.Now().UTC(),
			ActorType: "ANONYMOUS", Action: "AUTHENTICATION_DENIED",
			Method: c.Request.Method, Path: c.Request.URL.Path,
			ClientIP: RequestClientIP(c.Request), UserAgent: trimSecurityEvent(c.Request.UserAgent(), 1000),
			RequestID: strings.TrimSpace(requestctx.RequestID(c.Request.Context())),
			Result:    "DENIED", ReasonCode: http.StatusText(status), RiskLevel: "HIGH",
			Summary: "unauthenticated API request",
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
		defer cancel()
		if err := recorder.Record(ctx, event); err != nil && logger != nil {
			logger.Error("record unauthenticated security event", "error", err, "application", appCode, "path", event.Path)
		}
	}
}

func securityEventID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buf)
}

func trimSecurityEvent(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
