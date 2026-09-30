package middleware

import (
	"bufio"
	"bytes"
	"log/slog"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// platformAuditBuffer 缓冲审计必开模式下“将被记录审计事件”请求的响应。
//
// 安全理由（AUD-2026-005 / SEC-D4b）：gin 的审计中间件在业务 handler 执行完毕后才能拿到
// Ingest 的结果，而此时业务响应往往已经写给客户端、无法撤回。只有先把响应缓冲在内存里，
// 才能在审计事件写入失败时丢弃业务响应并返回 503，保证“审计写不进去 ⇒ 客户端拿不到成功”，
// 杜绝必开模式下的静默无审计写入。仅在必开模式下启用，非必开模式行为不变。
type platformAuditBuffer struct {
	orig    gin.ResponseWriter
	headers http.Header
	body    bytes.Buffer
	status  int
	size    int
	// limit 是响应体缓冲上限；超过即降级为旧行为（write-through）并输出告警，
	// 防止异常大的响应长期占用内存。
	limit      int
	overflowed bool
	logger     *slog.Logger
}

func newPlatformAuditBuffer(orig gin.ResponseWriter, limit int, logger *slog.Logger) *platformAuditBuffer {
	if logger == nil {
		logger = slog.Default()
	}
	return &platformAuditBuffer{orig: orig, headers: make(http.Header), status: http.StatusOK, size: -1, limit: limit, logger: logger}
}

// flushTo 在审计摄取成功后把缓冲的响应提交到真实连接：
// 先搬响应头，再写状态码与响应体，语义与 gin 自带 responseWriter 一致。
func (b *platformAuditBuffer) flushTo(dst gin.ResponseWriter) {
	b.WriteHeaderNow()
	for key, values := range b.headers {
		dst.Header().Del(key)
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(b.status)
	if b.body.Len() > 0 {
		_, _ = dst.Write(b.body.Bytes())
		return
	}
	dst.WriteHeaderNow()
}

// degradeToWriteThrough 在缓冲超限时把已缓冲内容一次性提交到真实连接并切换为直写，
// 降级为旧行为（响应无法再被审计失败撤回），同时输出告警日志。
func (b *platformAuditBuffer) degradeToWriteThrough() {
	b.overflowed = true
	b.flushTo(b.orig)
	b.logger.Warn("platform audit buffered response exceeded limit; degrading to write-through",
		"limit_bytes", b.limit, "status", b.status)
}

// —— 以下实现 gin.ResponseWriter 接口，行为对齐 gin 的 responseWriter。 ——

func (b *platformAuditBuffer) Header() http.Header { return b.headers }

func (b *platformAuditBuffer) WriteHeader(code int) {
	if code > 0 && b.status != code && !b.Written() {
		b.status = code
	}
}

func (b *platformAuditBuffer) WriteHeaderNow() {
	if !b.Written() {
		b.size = 0
	}
}

func (b *platformAuditBuffer) Write(p []byte) (int, error) {
	if !b.overflowed && b.body.Len()+len(p) > b.limit {
		b.degradeToWriteThrough()
	}
	if b.overflowed {
		return b.orig.Write(p)
	}
	b.WriteHeaderNow()
	n, err := b.body.Write(p)
	b.size += n
	return n, err
}

func (b *platformAuditBuffer) WriteString(s string) (int, error) {
	return b.Write([]byte(s))
}

func (b *platformAuditBuffer) Status() int   { return b.status }
func (b *platformAuditBuffer) Size() int     { return b.size }
func (b *platformAuditBuffer) Written() bool { return b.size != -1 }

// Flush 只标记“已写”，不能把半成品响应提前提交到真实连接，
// 否则审计失败时将无法撤回，拒绝语义会被破坏；超限直写模式下透传给真实连接。
func (b *platformAuditBuffer) Flush() {
	if b.overflowed {
		b.orig.Flush()
		return
	}
	b.WriteHeaderNow()
}

func (b *platformAuditBuffer) Pusher() http.Pusher { return b.orig.Pusher() }

func (b *platformAuditBuffer) Hijack() (net.Conn, *bufio.ReadWriter, error) { return b.orig.Hijack() }

func (b *platformAuditBuffer) CloseNotify() <-chan bool { return b.orig.CloseNotify() }

func (b *platformAuditBuffer) Unwrap() http.ResponseWriter { return b.orig }
