// Package middleware 提供所有模块共用的传输层 HTTP 中间件。
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const defaultPlatformAuditApplicationCode = "platform"
const defaultPlatformAuditEnvironmentCode = "dev"

// maxPlatformAuditBufferedResponseBytes 是必开模式下单请求响应的内存缓冲上限：超过即溢写到
// 临时文件继续扣留（见 platformAuditBuffer），不再放行直写，避免异常大的响应长期占用内存。
const maxPlatformAuditBufferedResponseBytes = 64 << 10

// AuditSource 标识服务端自行生成审计事件时使用的应用环境配置。
type AuditSource struct {
	ApplicationCode string
	EnvironmentCode string
}

// AuditRecorder 是 HTTP 中间件使用的审计应用层窄契约。把接口留在传输边界，
// 路由装配就不必依赖持久化细节。
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
		// 安全审查 SEC-D5：执行业务链前挂载可变目标 id 槽位，创建类处理器在成功后
		// 通过 MarkCreatedResource 登记对象 id，链路结束时统一采集进审计事件。
		if context.Request != nil {
			context.Request = context.Request.WithContext(AttachAuditResourceTarget(context.Request.Context()))
		}
		origWriter := context.Writer
		var buffered *platformAuditBuffer
		// 审计必开模式（AUD-2026-005）：对“将被记录审计事件”的请求先把响应缓冲在内存里，
		// Ingest 失败时丢弃业务响应并返回 503，保证“审计写不进去 ⇒ 客户端拿不到成功”。
		// 只改变满足记录条件的请求；/readyz 等只读路由与非必开模式行为完全不变。
		if platformAuditRequired() && recorder != nil && context.Request != nil {
			pendingRoute := context.FullPath()
			if pendingRoute == "" {
				pendingRoute = context.Request.URL.Path
			}
			if pendingPrincipal, ok := authctx.PrincipalFromContext(context.Request.Context()); ok && strings.TrimSpace(pendingPrincipal.Tenant.ID) != "" && shouldRecordAuditTrail(context.Request.Method, pendingRoute) {
				buffered = newPlatformAuditBuffer(origWriter, maxPlatformAuditBufferedResponseBytes, logger)
				context.Writer = buffered
			}
		}
		// handler panic 展平时先恢复真实 writer，外层 gin.Recovery 才能把 500 写出去；
		// 缓冲中的半成品响应被直接丢弃，不会被当成成功提交；溢写的临时文件一并清理。
		defer func() {
			context.Writer = origWriter
			if buffered != nil {
				buffered.dispose()
			}
		}()
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
		// 响应超过硬上限时无法在审计确认前完整扣留：按失败如实记录（保留业务 handler 的状态码
		// 供追溯），并在下方拒绝请求——绝不记录“成功”却把 503 交给客户端，也绝不放行直写。
		oversized := buffered != nil && buffered.oversized
		handlerStatusCode := statusCode
		if oversized {
			statusCode = http.StatusServiceUnavailable
		}
		now := time.Now().UTC()
		// 统一审计来源协议：登录地址取会话创建时固定的来源，操作地址每次请求重新解析。
		// 两者分别落库，不能再像旧实现那样用其中一个替代另一个——否则同一会话换网络后
		// 审计里看不出"登录在哪、这次操作在哪"。
		clientIP := RequestClientIP(context.Request)
		loginIPValue := loginIP(principal.LoginIP)
		// 来源由可信代理中间件的解析过程给出（DIRECT / TRUSTED_PROXY / UNKNOWN），
		// 不再按“上下文里有没有地址”推断——直连请求不应被标成可信代理。
		_, ipSource := requestctx.ClientIPWithSource(context.Request.Context())
		if ipSource == "" {
			// 仅设置过地址的适配器（历史调用方/测试）：无法得知解析路径，标记未知，
			// 不能凭“有地址”就推断为可信代理。
			ipSource = "UNKNOWN"
		}
		if clientIP == "" && loginIPValue == "" {
			ipSource = "UNKNOWN"
		}
		metadata := map[string]any{
			"method":      context.Request.Method,
			"path":        route,
			"status_code": statusCode,
		}
		if oversized {
			metadata["audit_rejection"] = "AUDIT_RESPONSE_TOO_LARGE"
			metadata["handler_status_code"] = handlerStatusCode
		}
		if loginIPValue != "" && clientIP != "" && clientIP != loginIPValue {
			// 历史字段：只在“登录后换了网络”时保留差异线索，供旧版审计页读取；
			// 登录地址缺失时操作地址已经写进 ClientIP，不再重复写入元数据。
			metadata["request_source_ip"] = clientIP
		}
		// 安全审查 SEC-D5：优先采用业务处理器登记的目标对象 id（体寻址创建没有路径
		// *_id），同时落入 metadata，满足审计“操作者+目标”的可检索要求。
		resourceID := auditResourceID(context)
		if handlerResourceID := AuditResourceIDFromRequest(context.Request); handlerResourceID != "" {
			resourceID = handlerResourceID
			metadata["resource_id"] = handlerResourceID
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
			ResourceID:      resourceID,
			RequestID:       requestctx.RequestID(context.Request.Context()),
			TraceID:         requestctx.TraceID(context.Request.Context()),
			Result:          auditResult(statusCode),
			RiskLevel:       auditRiskLevel(context.Request.Method, route, statusCode),
			Classification:  "INTERNAL",
			Summary:         auditSummary(context.Request.Method, route, statusCode),
			Metadata:        metadata,
			LoginIP:         loginIPValue,
			ClientIP:        clientIP,
			IPSource:        ipSource,
			// SourceIP 是旧协议兼容列：保持历史语义（登录地址优先，其次操作地址）。
			SourceIP:  legacySourceIP(loginIPValue, clientIP),
			UserAgent: context.Request.UserAgent(),
		}
		if _, err := recorder.Ingest(context.Request.Context(), principal.Tenant.ID, input); err != nil {
			logger.Error("write platform audit event", "error", err, "request_id", input.RequestID, "path", route)
			if buffered != nil {
				// 必开模式（AUD-2026-005/SEC-D4b）：审计事件写入失败必须拒绝请求，
				// 丢弃扣留的业务响应并返回 503，错误码与子系统保持一致。响应已溢写到
				// 临时文件时同样成立——扣留不再因体积放弃，因此这里没有直写例外。
				context.Writer = origWriter
				httpresponse.WriteError(context.Writer, context.Request, http.StatusServiceUnavailable, httperror.New("AUDIT_WRITE_REJECTED", "审计记录写入失败，操作未被确认", nil))
			}
			return
		}
		if oversized {
			// 审计已如实记录“响应超限、未被确认”，客户端同样不能拿到成功。
			context.Writer = origWriter
			httpresponse.WriteError(context.Writer, context.Request, http.StatusServiceUnavailable, httperror.New("AUDIT_RESPONSE_TOO_LARGE", "响应超过审计扣留上限，操作未被确认", nil))
			return
		}
		if buffered != nil {
			buffered.flushTo(origWriter)
		}
	}
}

// legacySourceIP 返回旧协议兼容列的值：登录地址优先，其次操作地址。
// 新代码应读取 LoginIP / ClientIP，该函数只服务于尚未升级的消费方。
func legacySourceIP(loginIPValue, clientIP string) string {
	if loginIPValue != "" {
		return loginIPValue
	}
	return clientIP
}

// platformAuditRequired 判定是否处于审计必开模式：PLATFORM_AUDIT_REQUIRED 显式设置优先，
// 否则 APP_ENV=production 默认必开。平台配置键为 APP_ENV（internal/shared/config 的
// Environment），与子系统 data_analysis/contract/PM 的 SEC-D4b 判定保持同一语义。
func platformAuditRequired() bool {
	raw := strings.TrimSpace(os.Getenv("PLATFORM_AUDIT_REQUIRED"))
	if raw != "" {
		required, err := strconv.ParseBool(raw)
		return err == nil && required
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
}

// loginIP 只返回服务端已验证的会话主体中的规范化 IP 字面量，绝不回退到请求头；
// 从请求解析来源始终是传输中间件的职责。
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
	// 安全审查 SEC-D4a：通用轨迹不再对任何写操作做成功抑制。此前用户应用授权、
	// 授权主体撤销、授权目录发布在成功时只依赖业务审计事件——业务审计摄取一旦失败，
	// 该操作在任何地方都没有记录。现在通用轨迹始终落库（业务审计失败时由它兜底，
	// 测试见 TestAuditTrailRecordsGenericEventForBusinessAuditRoute）；业务审计成功时
	// 额外产生一条更完整的事件，消费方按 EventCategory/EventType 区分，宁可重复不可丢失。
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
