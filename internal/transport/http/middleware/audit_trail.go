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
	"sync"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const defaultPlatformAuditApplicationCode = "platform"
const defaultPlatformAuditEnvironmentCode = "dev"

// maxPlatformAuditBufferedResponseBytes 是必开模式下单请求响应的内存缓冲上限：超过即溢写到
// 临时文件继续扣留（见 platformAuditBuffer），不再放行直写，避免异常大的响应长期占用内存。
const maxPlatformAuditBufferedResponseBytes = 64 << 10

// auditAvailabilityCooldown 是审计管道故障后的快速失败窗口（BIZ-RUNTIME-11）。必开模式
// 下审计写入失败发生在业务提交之后，客户端重试会把非幂等写入重复执行；因此一旦观测到
// 写入失败，就在窗口内把新的“将被审计”请求在业务链之前拒绝，重试不再产生业务事实。
// 窗口到期后允许单个请求再次探测审计管道：恢复即放行，仍失败则继续拒绝。
// 该机制只做快速失败，不改变审计事件本身的事务/异步 outbox 语义。
const auditAvailabilityCooldown = 30 * time.Second

// auditGate 保存审计管道最近一次写入失败后开启的快速失败窗口。实例随中间件挂载
// 进程内共享；零值表示可用，无需初始化。
//
// events 是审计写入失败指标的注册表（platform_audit_write_failures_total{stage}）。
// 它只承担记账：失败窗口的开关仍完全由 reopenAt 决定，指标绝不改变失败关闭语义。
type auditGate struct {
	mu       sync.Mutex
	reopenAt time.Time
	events   *eventmetrics.Registry
}

// eventMetrics 返回该 gate 的失败计数注册表；未注入时回退进程默认注册表。
func (gate *auditGate) eventMetrics() *eventmetrics.Registry {
	if gate == nil {
		return eventmetrics.Shared()
	}
	return eventmetrics.RegistryOr(gate.events)
}

// recordWriteFailure 在失败发生点按 stage 计数，不触碰快速失败窗口。
// 它必须与 recordFailure 分开：结果事件已落死信时属于“可继续放行”的降级路径，
// 硬把窗口打开会把一次可恢复的审计降级升级成拒绝全部写请求。
func (gate *auditGate) recordWriteFailure(stage string) {
	if gate == nil {
		return
	}
	gate.eventMetrics().RecordAuditWriteFailure(stage)
}

// blocked 报告当前是否处于快速失败窗口内。
func (gate *auditGate) blocked(now time.Time) bool {
	if gate == nil {
		return false
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return now.Before(gate.reopenAt)
}

// recordFailure 在审计写入失败后开启（或延长）快速失败窗口。
func (gate *auditGate) recordFailure(now time.Time) {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.reopenAt = now.Add(auditAvailabilityCooldown)
}

// recordSuccess 在审计写入成功后立即关闭快速失败窗口。
func (gate *auditGate) recordSuccess() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.reopenAt = time.Time{}
}

// AuditSource 标识服务端自行生成审计事件时使用的应用环境配置。
type AuditSource struct {
	ApplicationCode string
	EnvironmentCode string
}

// AuditRecorder 是 HTTP 中间件使用的审计应用层窄契约。把接口留在传输边界，
// 路由装配就不必依赖持久化细节。
//
// BIZ-RUNTIME-11：必开模式下中间件按“意图 + 结果”两段写入，结果事件在同步重试后仍
// 写不进去时不能再让已经生效的业务事实失去审计结论，因此契约除 Ingest 外再暴露 Spool。
type AuditRecorder interface {
	Ingest(context.Context, string, auditapplication.EventInput) (auditdomain.Receipt, error)
	// Spool 把同步写入/重试都失败的审计事件按审计死信语义（PENDING + 脱敏载荷）持久化，
	// 供运营重放或忽略。tenantID 与 Ingest 一致，是租户边界，实现必须强制。
	Spool(context.Context, string, auditapplication.EventInput) error
}

// auditResultRetryAttempts 是必开模式下“结果事件”写入失败后的同步重试次数（不含首次）。
// 意图事件已先落库时，结果事件丢失等于业务事实没有审计结论；先同步重试一次，仍失败才落
// 死信。重试次数有意保持为 1：再多的同步重试只会把客户端拖在已经生效的请求上。
const auditResultRetryAttempts = 1

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

	// 每次挂载共享一个快速失败窗口：同一进程内被审计的请求感知同一次审计管道故障。
	return auditTrailHandler(recorder, logger, source, &auditGate{})
}

// auditTrailHandler 是 AuditTrail 的内部构造，额外接收快速失败窗口，使必开模式的
// “业务写入前拒绝审计不可用请求”语义可独立测试。
//
// BIZ-RUNTIME-11 事务顺序（方案 A 步骤 1+2）：必开模式下先在业务链之前写入“意图事件”
// （metadata.phase=intent，Result=REQUESTED），写不进去就当场 503 且 handler 不执行；
// 业务链结束后补写“结果事件”（metadata.phase=result，metadata.correlates_to=意图
// EventID，含真实状态码语义）。结果事件先同步重试一次，仍失败则落审计死信并放行业务
// 响应——审计事实已在库，不再对已经生效的操作谎报“未被确认”。只有意图、结果、死信
// 三者都不可写才拒绝，且必须明确告知 operation_applied。
func auditTrailHandler(recorder AuditRecorder, logger *slog.Logger, source AuditSource, gate *auditGate) gin.HandlerFunc {
	return func(context *gin.Context) {
		// 安全审查 SEC-D5：执行业务链前挂载可变目标 id 槽位，创建类处理器在成功后
		// 通过 MarkCreatedResource 登记对象 id，链路结束时统一采集进审计事件。
		if context.Request != nil {
			context.Request = context.Request.WithContext(AttachAuditResourceTarget(context.Request.Context()))
		}
		origWriter := context.Writer
		var buffered *platformAuditBuffer
		// 审计必开模式（AUD-2026-005 / BIZ-RUNTIME-11）：判定条件与“将被记录审计事件”的
		// 记录条件保持一致；/readyz 等只读路由与非必开模式行为完全不变。
		auditedRequest := false
		auditRoute := ""
		var auditedPrincipal authctx.Principal
		if platformAuditRequired() && recorder != nil && context.Request != nil {
			auditRoute = auditRequestRoute(context)
			if pendingPrincipal, ok := authctx.PrincipalFromContext(context.Request.Context()); ok && strings.TrimSpace(pendingPrincipal.Tenant.ID) != "" && shouldRecordAuditTrail(context.Request.Method, auditRoute) {
				auditedRequest = true
				auditedPrincipal = pendingPrincipal
			}
		}
		// BIZ-RUNTIME-11：审计不可用的检查必须早于业务写入。审计写入失败只可能发生在业务
		// 提交之后，此时才拒绝响应会让客户端重试并重复执行非幂等写入；因此把上一次审计
		// 失败转成快速失败窗口，窗口内的新请求在执行业务链之前即被拒绝，不产生业务事实。
		if auditedRequest && gate.blocked(time.Now().UTC()) {
			logger.Warn("reject write before business commit because the audit pipeline is unavailable",
				"path", context.Request.URL.Path, "request_id", requestctx.RequestID(context.Request.Context()))
			// 被失败窗口拒绝的请求没有执行任何写入尝试：stage=unavailable 记录的是
			// “审计不可用”这一后果（与 write 失败分开计），不参与窗口判定。
			gate.recordWriteFailure(eventmetrics.AuditStageUnavailable)
			// 必须 Abort：gin 的中间件返回本身不会终止链路，否则业务处理器仍会执行。
			context.Abort()
			httpresponse.WriteError(origWriter, context.Request, http.StatusServiceUnavailable, httperror.New(
				"AUDIT_UNAVAILABLE",
				"审计记录暂不可用，操作未被受理",
				map[string]string{"operation_applied": "false", "retryable": "true", "next_action": "retry_allowed"},
			))
			return
		}
		// BIZ-RUNTIME-11 步骤 1：意图事件必须落在业务链之前。审计写不进去就当场 503 且
		// handler 不执行——这才是真正的 fail-closed：既没有副作用，也没有“已生效但未确认”
		// 的重试陷阱。意图写入成功后才扣留响应（oversized 仍需能如实记录并拒绝）。
		intentEventID := ""
		// 该请求是否属于“留痕但响应体不采集”的路由（审计导出下载）。只在必开模式下有意义：
		// 非必开模式本来就不缓冲任何响应，标记留空以免夸大语义。
		bodyNotCaptured := auditedRequest && !shouldCaptureAuditResponseBody(auditRoute)
		if auditedRequest {
			intent := newPlatformAuditInput(context, source, auditedPrincipal, platformAuditEventFields{route: auditRoute, phase: "intent", bodyNotCaptured: bodyNotCaptured})
			if _, err := recorder.Ingest(context.Request.Context(), auditedPrincipal.Tenant.ID, intent); err != nil {
				logger.Error("write platform audit intent event", "error", err, "request_id", intent.RequestID, "path", auditRoute)
				gate.recordWriteFailure(eventmetrics.AuditStageIntent)
				gate.recordFailure(time.Now().UTC())
				// 必须 Abort：gin 的中间件返回本身不会终止链路，否则业务处理器仍会执行。
				context.Abort()
				httpresponse.WriteError(origWriter, context.Request, http.StatusServiceUnavailable, httperror.New(
					"AUDIT_WRITE_REJECTED",
					"审计记录写入失败，操作未被受理",
					map[string]string{"operation_applied": "false", "retryable": "true", "next_action": "retry_allowed"},
				))
				return
			}
			intentEventID = intent.EventID
			gate.recordSuccess()
			// 审计导出下载是流式二进制响应（可能是任意大小的 zip），扣留它既没有审计价值，
			// 又会把内容写进 /tmp 或直接触发 AUDIT_RESPONSE_TOO_LARGE 503；因此只对需要
			// 扣留正文的路由装缓冲，下载路由直接写真实连接、仍然留痕。
			if shouldCaptureAuditResponseBody(auditRoute) {
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

		route := auditRequestRoute(context)
		if intentEventID == "" && !shouldRecordAuditTrail(context.Request.Method, route) {
			return
		}

		principal, ok := authctx.PrincipalFromContext(context.Request.Context())
		if intentEventID != "" {
			// 意图事件已经落库，结果事件必须成对写入：路由解析在业务链后被改写也不能让
			// 一条意图没有结论。
			principal, ok = auditedPrincipal, true
		}
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
		fields := platformAuditEventFields{route: route, statusCode: statusCode, handlerStatusCode: handlerStatusCode, oversized: oversized, bodyNotCaptured: bodyNotCaptured}
		retries := 0
		if intentEventID != "" {
			// 必开模式：结果事件与意图事件通过 correlates_to 关联，需要同步重试 + 死信兜底。
			fields.phase = "result"
			fields.correlatesTo = intentEventID
			retries = auditResultRetryAttempts
		}
		input := newPlatformAuditInput(context, source, principal, fields)
		if err := ingestPlatformAuditEvent(context.Request.Context(), recorder, principal.Tenant.ID, input, retries); err != nil {
			logger.Error("write platform audit event", "error", err, "request_id", input.RequestID, "path", route)
			// 结果事件（含同步重试）写入失败先按 stage=result 计数；是否同时按死信
			// 失败计数取决于下面的 Spool 结果，是否打开失败窗口仍只看既有 recordFailure 调用。
			gate.recordWriteFailure(eventmetrics.AuditStageResult)
			if intentEventID == "" {
				// 非必开模式/无意图事件：保持既有“尽力而为 + 结构化错误日志”语义，响应照常。
				gate.recordFailure(time.Now().UTC())
				return
			}
			// BIZ-RUNTIME-11 步骤 2：结果事件同步重试后仍不可写，落审计死信（既有 PENDING
			// 死信语义，可被运营重放或忽略）。意图事件已在库，合规成立，因此放行业务响应。
			if spoolErr := recorder.Spool(context.Request.Context(), principal.Tenant.ID, input); spoolErr != nil {
				logger.Error("spool platform audit result event to dead letter", "error", spoolErr,
					"request_id", input.RequestID, "path", route, "event_id", input.EventID, "intent_event_id", intentEventID)
				gate.recordWriteFailure(eventmetrics.AuditStageDeadLetter)
				gate.recordFailure(time.Now().UTC())
				// 意图 + 结果 + 死信全不可写：只能拒绝响应，但必须如实说明业务事实是否生效，
				// 绝不谎报“未被确认”，也不对已生效的写操作引导无脑重试。
				applied := handlerStatusCode >= http.StatusOK && handlerStatusCode < http.StatusMultipleChoices
				detail := map[string]string{
					"handler_status_code": strconv.Itoa(handlerStatusCode),
					"intent_event_id":     intentEventID,
					"operation_applied":   strconv.FormatBool(applied),
					"next_action":         "retry_allowed",
				}
				message := "审计结果写入失败且无法落死信，业务操作未生效"
				if applied {
					detail["next_action"] = "do_not_retry"
					message = "审计结果写入失败且无法落死信，业务操作已生效，请勿重复提交"
				}
				if buffered == nil {
					// 该路由按策略不扣留响应体（审计导出下载），响应已经直接流给客户端，
					// 状态码与响应体都无法再撤回；此时追加 503 只会污染一个已完成的文件下载，
					// 因此只保留错误日志与已经写入的意图/死信事实。
					logger.Error("platform audit result event unavailable after the response was already streamed",
						"request_id", input.RequestID, "path", route, "event_id", input.EventID, "intent_event_id", intentEventID,
						"operation_applied", applied)
					return
				}
				context.Writer = origWriter
				httpresponse.WriteError(context.Writer, context.Request, http.StatusServiceUnavailable, httperror.New("AUDIT_RESULT_WRITE_REJECTED", message, detail))
				return
			}
			logger.Warn("platform audit result event stored as dead letter after retry exhaustion",
				"request_id", input.RequestID, "path", route, "event_id", input.EventID, "intent_event_id", intentEventID)
		} else {
			gate.recordSuccess()
		}
		if oversized {
			// 业务链已执行完，超大响应只说明无法完整扣留审计载荷，不改变业务事实是否生效。
			// 因此与步骤 2 同一口径：如实告知 operation_applied，绝不再写“未被确认”。
			applied := handlerStatusCode >= http.StatusOK && handlerStatusCode < http.StatusMultipleChoices
			detail := map[string]string{
				"handler_status_code": strconv.Itoa(handlerStatusCode),
				"operation_applied":   strconv.FormatBool(applied),
				"next_action":         "retry_allowed",
			}
			message := "响应超过审计扣留上限，业务操作未生效"
			if applied {
				detail["next_action"] = "do_not_retry"
				message = "响应超过审计扣留上限，业务操作已生效，请勿重复提交"
			}
			context.Writer = origWriter
			httpresponse.WriteError(context.Writer, context.Request, http.StatusServiceUnavailable, httperror.New("AUDIT_RESPONSE_TOO_LARGE", message, detail))
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

// auditRequestRoute 返回审计事件使用的路由标识：优先路由模板（可按路由聚合），
// 没有匹配模板时退回原始请求路径。业务链前后都要用同一口径，避免意图与结果事件
// 落在不同 path 上。
func auditRequestRoute(context *gin.Context) string {
	if context == nil {
		return ""
	}
	route := context.FullPath()
	if route == "" && context.Request != nil {
		route = context.Request.URL.Path
	}
	return route
}

// platformAuditEventFields 是“意图事件”和“结果事件”之间的差异部分。
type platformAuditEventFields struct {
	route string
	// phase 为空表示非必开模式的单条终态事件（保持既有通用轨迹语义）；
	// 必开模式下取 intent / result。
	phase string
	// correlatesTo 是结果事件回指意图事件 EventID 的关联标识。
	correlatesTo string
	// statusCode 为 0 表示业务尚未执行（意图事件没有结论可记）。
	statusCode int
	// handlerStatusCode 只在响应超限时用于保留业务真实状态码供追溯。
	handlerStatusCode int
	oversized         bool
	// bodyNotCaptured 表示该路由按策略不采集响应体（审计导出下载）：事件仍然完整记录
	// 方法/路径/主体/状态码/结果，只在 metadata 里如实标注正文未采集，避免取证方误判
	// “有审计记录 ⇒ 响应体已被审计覆盖”。
	bodyNotCaptured bool
}

// newPlatformAuditInput 组装中间件审计事件。必开模式下同一请求产生两条事件：业务链之前
// 的意图事件（Result=REQUESTED、无状态码）和业务链之后的结果事件（真实状态码、
// correlates_to 指向意图 EventID）；非必开模式仍是既有的一条终态事件，字段语义不变。
func newPlatformAuditInput(context *gin.Context, source AuditSource, principal authctx.Principal, fields platformAuditEventFields) auditapplication.EventInput {
	method := context.Request.Method
	// 统一审计来源协议：登录地址取会话创建时固定的来源，操作地址每次请求重新解析。
	// 两者分别落库，不能再像旧实现那样用其中一个替代另一个——否则同一会话换网络后
	// 审计里看不出"登录在哪、这次操作在哪"。
	clientIP, loginIPValue, ipSource := platformAuditOrigin(context, principal)
	metadata := map[string]any{"method": method, "path": fields.route}
	if fields.phase != "" {
		metadata["phase"] = fields.phase
	}
	if fields.correlatesTo != "" {
		metadata["correlates_to"] = fields.correlatesTo
	}
	if fields.statusCode > 0 {
		metadata["status_code"] = fields.statusCode
	}
	if fields.oversized {
		metadata["audit_rejection"] = "AUDIT_RESPONSE_TOO_LARGE"
		metadata["handler_status_code"] = fields.handlerStatusCode
	}
	if fields.bodyNotCaptured {
		// 取证口径：这条路由仍留痕，但响应体按策略未采集（不缓冲、不落盘）。
		metadata["response_body"] = "not_captured"
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
	// 意图事件的结论未定：Result 用 REQUESTED 与终态结果区分，Summary 也不谎报状态。
	result := "REQUESTED"
	summary := method + " " + fields.route + " requested"
	riskStatus := http.StatusOK
	if fields.statusCode > 0 {
		result = auditResult(fields.statusCode)
		summary = auditSummary(method, fields.route, fields.statusCode)
		riskStatus = fields.statusCode
	}
	return auditapplication.EventInput{
		EventID:         newPlatformAuditEventID(),
		ApplicationCode: source.ApplicationCode,
		EnvironmentCode: source.EnvironmentCode,
		EventCategory:   "PLATFORM",
		EventType:       method + " " + fields.route,
		OccurredAt:      time.Now().UTC(),
		ActorType:       "USER",
		ActorID:         principal.User.ID,
		ActorName:       principal.User.Name,
		SessionID:       principal.SessionID,
		Action:          method + " " + fields.route,
		ResourceType:    auditResourceType(fields.route),
		ResourceID:      resourceID,
		RequestID:       requestctx.RequestID(context.Request.Context()),
		TraceID:         requestctx.TraceID(context.Request.Context()),
		Result:          result,
		RiskLevel:       auditRiskLevel(method, fields.route, riskStatus),
		Classification:  "INTERNAL",
		Summary:         summary,
		Metadata:        metadata,
		LoginIP:         loginIPValue,
		ClientIP:        clientIP,
		IPSource:        ipSource,
		// SourceIP 是旧协议兼容列：保持历史语义（登录地址优先，其次操作地址）。
		SourceIP:  legacySourceIP(loginIPValue, clientIP),
		UserAgent: context.Request.UserAgent(),
	}
}

// platformAuditOrigin 解析统一审计来源协议的两个地址与来源标记。
func platformAuditOrigin(context *gin.Context, principal authctx.Principal) (clientIP, loginIPValue, ipSource string) {
	clientIP = RequestClientIP(context.Request)
	loginIPValue = loginIP(principal.LoginIP)
	// 来源由可信代理中间件的解析过程给出（DIRECT / TRUSTED_PROXY / UNKNOWN），
	// 不再按“上下文里有没有地址”推断——直连请求不应被标成可信代理。
	_, ipSource = requestctx.ClientIPWithSource(context.Request.Context())
	if ipSource == "" {
		// 仅设置过地址的适配器（历史调用方/测试）：无法得知解析路径，标记未知，
		// 不能凭“有地址”就推断为可信代理。
		ipSource = "UNKNOWN"
	}
	if clientIP == "" && loginIPValue == "" {
		ipSource = "UNKNOWN"
	}
	return clientIP, loginIPValue, ipSource
}

// ingestPlatformAuditEvent 写入一条中间件审计事件；retries > 0 时失败后同步重试，
// 复用同一 EventID，避免重试生成两条语义相同的事件。
func ingestPlatformAuditEvent(ctx context.Context, recorder AuditRecorder, tenantID string, input auditapplication.EventInput, retries int) error {
	_, err := recorder.Ingest(ctx, tenantID, input)
	for attempt := 0; err != nil && attempt < retries; attempt++ {
		_, err = recorder.Ingest(ctx, tenantID, input)
	}
	return err
}

// platformAuditRequired 判定是否处于审计必开模式：PLATFORM_AUDIT_REQUIRED 显式设置优先，
// 否则 APP_ENV=production 默认必开。平台配置键为 APP_ENV（internal/shared/config 的
// Environment），与子系统 data_analysis/contract/PM 的 SEC-D4b 判定保持同一语义。
// 显式值非法（strconv.ParseBool 解析失败）按失败关闭返回 true：把强制审计配错不该静默
// 降级成“非必开”，与 project_management / Settlement 的同类实现同语义。
func platformAuditRequired() bool {
	raw := strings.TrimSpace(os.Getenv("PLATFORM_AUDIT_REQUIRED"))
	if raw != "" {
		required, err := strconv.ParseBool(raw)
		if err != nil {
			return true
		}
		return required
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

// auditExportDownloadRoutePrefix 是审计导出下载的路由前缀：GET
// <prefix><job_id>/download 直接回流导出文件。它与 download-tickets（签发一次性票据）
// 不同，是唯一会把任意大小二进制内容写进响应体的审计路由。
const auditExportDownloadRoutePrefix = "/api/v1/audit/export-jobs/"

// isAuditExportDownloadRoute 判定审计导出下载路由，同时接受路由模板与原始路径两种口径
// （中间件在业务链前后都用 auditRequestRoute 解析，未命中模板时会退回原始路径）。
func isAuditExportDownloadRoute(route string) bool {
	return strings.HasPrefix(route, auditExportDownloadRoutePrefix) && strings.HasSuffix(route, "/download")
}

// shouldCaptureAuditResponseBody 报告该路由的响应体是否需要被审计缓冲扣留。
//
// 审计导出下载必须留痕（谁在什么时候下载了哪个导出），但它的响应体是流式二进制文件：
// 扣留正文既不增加审计价值，又会把内容写进临时文件，并在超过 512MiB 硬上限时把一次
// 合法下载变成 AUDIT_RESPONSE_TOO_LARGE 503。因此下载路由仍然产生审计事件（方法/路径/
// 主体/状态码/结果），只是响应体按策略不采集（metadata.response_body=not_captured）。
// 其余路由（含审计查询、票据签发）保持原扣留语义不变。
func shouldCaptureAuditResponseBody(route string) bool {
	return !isAuditExportDownloadRoute(route)
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
