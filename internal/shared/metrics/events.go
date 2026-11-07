// Package metrics 提供不依赖 Prometheus 客户端库的进程内事件级计数器。
//
// 它只承载“在业务失败发生点直接记账”的少量受约束维度：
//   - platform_login_failures_total{reason}：登录失败原因；
//   - platform_audit_write_failures_total{stage}：审计写入失败阶段。
//
// 两条指标的标签取值域都是本包内的代码常量枚举，绝不接受账号、租户、路径或错误文本：
// 未知取值统一折叠进兜底桶（登录失败 reason→other，审计 stage→unavailable），
// 因此序列数上界固定，不会因为异常输入把 /metrics 变成高基数或隐私泄漏入口。
//
// 为什么不用 prometheus/client_golang：仓库既有 /metrics 是手写 exposition（见
// internal/transport/http/metrics.go 与 cmd/file-gateway/main.go），引入官方客户端会
// 形成两套注册表与两套抓取语义。本包只提供计数与文本渲染片段，由既有 exposition
// 直接嵌入，保持“单一抓取面、单一转义函数”的现状。
package metrics

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
)

// 登录失败原因枚举。取值域固定为下面六个常量：前五个是可由登录/回调失败分支明确判定的
// 原因，other 是唯一的兜底桶。禁止把账号、租户、角色、原始错误文本或路径拼进 reason。
const (
	// LoginFailureInvalidCredentials 表示本地账号/密码凭据不匹配，或未知账号（两者在
	// 公共响应中刻意不可区分，指标也共用同一桶，避免把指标变成账号枚举侧信道）。
	LoginFailureInvalidCredentials = "invalid_credentials"
	// LoginFailureAccountDisabled 表示凭据正确但身份不可用：租户/用户/账号被停用、
	// 锁定窗口内或凭据过期。
	LoginFailureAccountDisabled = "account_disabled"
	// LoginFailureIdPError 表示上游 OIDC 交换/验签/身份查询失败（IdP 侧或协议错误）。
	LoginFailureIdPError = "idp_error"
	// LoginFailureRateLimited 表示登录请求在限流层被拒绝（双桶固定窗口）。
	LoginFailureRateLimited = "rate_limited"
	// LoginFailureSessionExpired 表示登录事务或会话已失效（OIDC state/nonce/verifier
	// 过期、刷新时服务端会话不再有效）。
	LoginFailureSessionExpired = "session_expired"
	// LoginFailureOther 是唯一兜底桶：并发会话冲突、依赖故障、未知错误都折叠到这里。
	LoginFailureOther = "other"
)

// loginFailureReasons 是 reason 的固定输出顺序；other 必须在末尾，索引即兜底桶。
var loginFailureReasons = [...]string{
	LoginFailureInvalidCredentials,
	LoginFailureAccountDisabled,
	LoginFailureIdPError,
	LoginFailureRateLimited,
	LoginFailureSessionExpired,
	LoginFailureOther,
}

// auditStageUnavailableIndex 是 stage 兜底桶下标（unavailable，列表末位）。
const auditStageUnavailableIndex = 4

// 审计写入失败阶段枚举。它对应审计中间件的“意图→结果→死信→拒绝”链路：
//   - intent：业务链之前的意图事件写不进去（请求当场 503，handler 未执行）；
//   - result：业务链之后的结果事件写入（含同步重试）失败；
//   - dead_letter：结果事件只能落审计死信，且死信本身也写不进去；
//   - result_streamed：结果事件与死信都写不进去，但响应已经直接流给客户端
//     （审计导出下载），无法再补 503——这是“业务已流出、审计结论缺失”的失败，
//     只保留日志就无法被发现，因此单独成桶供告警使用；
//   - unavailable：审计管道处于失败窗口，请求在业务写入之前被拒绝——这是失败的
//     后果而不是一次写入尝试，但同样是“审计不可用”的可观测信号。
//
// 本指标只计数，不参与快速失败窗口判定：审计的失败关闭语义完全由 auditGate 决定。
const (
	AuditStageIntent         = "intent"
	AuditStageResult         = "result"
	AuditStageDeadLetter     = "dead_letter"
	AuditStageResultStreamed = "result_streamed"
	AuditStageUnavailable    = "unavailable"
)

// auditWriteFailureStages 是 stage 的固定输出顺序；unavailable 必须在末尾（兜底桶）。
var auditWriteFailureStages = [...]string{
	AuditStageIntent,
	AuditStageResult,
	AuditStageDeadLetter,
	AuditStageResultStreamed,
	AuditStageUnavailable,
}

const (
	metricLoginFailuresTotal      = "platform_login_failures_total"
	metricAuditWriteFailuresTotal = "platform_audit_write_failures_total"
)

// Registry 是进程内事件级计数器集合。所有字段是固定下标的 atomic 计数，
// 因此并发记账与抓取无锁竞争、无序列数增长。
type Registry struct {
	loginFailures      [len(loginFailureReasons)]atomic.Uint64
	auditWriteFailures [len(auditWriteFailureStages)]atomic.Uint64
}

// NewRegistry 创建一个独立的空注册表（测试与显式装配使用）。
func NewRegistry() *Registry {
	return &Registry{}
}

// sharedRegistry 是进程默认注册表。登录处理器、登录限流与审计中间件未显式注入时共用
// 这一实例，与平台 /metrics 的 exposition 保持同一份计数来源。
var sharedRegistry = NewRegistry()

// Shared 返回进程默认注册表。
func Shared() *Registry {
	return sharedRegistry
}

// RegistryOr 返回 registry；nil 时回退到进程默认注册表。装配遗漏只能丢计数，
// 不能在登录/审计路径上 panic。
func RegistryOr(registry *Registry) *Registry {
	if registry == nil {
		return sharedRegistry
	}
	return registry
}

// RecordLoginFailure 按 reason 计数；非枚举取值折叠进 other。
// nil 注册表是合法的空操作，便于未注入指标的可选装配路径。
func (registry *Registry) RecordLoginFailure(reason string) {
	if registry == nil {
		return
	}
	registry.loginFailures[loginFailureReasonIndex(reason)].Add(1)
}

// RecordAuditWriteFailure 按 stage 计数；非枚举取值折叠进 unavailable。
func (registry *Registry) RecordAuditWriteFailure(stage string) {
	if registry == nil {
		return
	}
	registry.auditWriteFailures[auditStageIndex(stage)].Add(1)
}

// LoginFailureCount 返回某个 reason 的当前累计值；非枚举取值恒为 0
// （它们被折叠进 other，不在这里冒充独立桶）。
func (registry *Registry) LoginFailureCount(reason string) uint64 {
	if registry == nil {
		return 0
	}
	index, known := lookupLoginFailureReason(reason)
	if !known {
		return 0
	}
	return registry.loginFailures[index].Load()
}

// AuditWriteFailureCount 返回某个 stage 的当前累计值；非枚举取值恒为 0。
func (registry *Registry) AuditWriteFailureCount(stage string) uint64 {
	if registry == nil {
		return 0
	}
	index, known := lookupAuditStage(stage)
	if !known {
		return 0
	}
	return registry.auditWriteFailures[index].Load()
}

// Write 渲染两条指标。全部枚举（含 0）无条件输出：标签维度固定且基数极小，
// 恒有样本才能让 rate()/absent() 在空闲进程上也有定义，而不是把“零失败”伪装成
// “抓取不到”。
func (registry *Registry) Write(writer io.Writer) {
	registry = RegistryOr(registry)
	fmt.Fprintf(writer, "# TYPE %s counter\n", metricLoginFailuresTotal)
	for index, reason := range loginFailureReasons {
		fmt.Fprintf(writer, "%s{reason=\"%s\"} %d\n", metricLoginFailuresTotal,
			EscapeLabelValue(reason), registry.loginFailures[index].Load())
	}
	fmt.Fprintf(writer, "# TYPE %s counter\n", metricAuditWriteFailuresTotal)
	for index, stage := range auditWriteFailureStages {
		fmt.Fprintf(writer, "%s{stage=\"%s\"} %d\n", metricAuditWriteFailuresTotal,
			EscapeLabelValue(stage), registry.auditWriteFailures[index].Load())
	}
}

// loginFailureReasonIndex 把 reason 映射到固定下标；未知取值一律落 other。
func loginFailureReasonIndex(reason string) int {
	index, known := lookupLoginFailureReason(reason)
	if !known {
		return len(loginFailureReasons) - 1
	}
	return index
}

// auditStageIndex 把 stage 映射到固定下标；未知取值一律落 unavailable。
func auditStageIndex(stage string) int {
	index, known := lookupAuditStage(stage)
	if !known {
		return auditStageUnavailableIndex
	}
	return index
}

func lookupLoginFailureReason(reason string) (int, bool) {
	for index, candidate := range loginFailureReasons {
		if candidate == reason {
			return index, true
		}
	}
	return 0, false
}

func lookupAuditStage(stage string) (int, bool) {
	for index, candidate := range auditWriteFailureStages {
		if candidate == stage {
			return index, true
		}
	}
	return 0, false
}

// EscapeLabelValue 按 Prometheus 文本协议转义标签值。所有标签取值都来自代码常量枚举，
// 但转义仍必须集中在一处：一个含引号或换行的值会破坏整个抓取结果。
// internal/transport/http 的既有实现直接复用本函数，避免两套转义规则漂移。
func EscapeLabelValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

// FormatBucketBound 用最短往返表示输出直方图桶上界，避免 %f 的尾零或科学计数法让 le
// 值与阈值比较时失真。平台 HTTP 面与 file-gateway 直方图共用，保证两处桶边界文本一致。
func FormatBucketBound(bound float64) string {
	return strconv.FormatFloat(bound, 'g', -1, 64)
}
