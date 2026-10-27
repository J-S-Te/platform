package middleware

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

// maxPlatformAuditSpilledResponseBytes 是单请求可被缓冲的响应体硬上限（内存 + 临时文件）。
// 超过即视为“无法在审计确认前完整扣留响应”，中间件如实记录为失败并拒绝请求，
// 绝不退回“先放行再补审计”的直写行为。测试可覆盖该变量以缩小阈值。
var maxPlatformAuditSpilledResponseBytes = 512 << 20

// errPlatformAuditResponseTooLarge 在超过硬上限后由 Write 返回；后续写入全部丢弃。
var errPlatformAuditResponseTooLarge = errors.New("platform audit buffered response exceeds hard limit")

// platformAuditBuffer 缓冲审计必开模式下“将被记录审计事件”请求的响应。
//
// 安全理由（AUD-2026-005 / SEC-D4b）：gin 的审计中间件在业务 handler 执行完毕后才能拿到
// Ingest 的结果，而此时业务响应往往已经写给客户端、无法撤回。只有先把响应扣留，
// 才能在审计事件写入失败时丢弃业务响应并返回 503，保证“审计写不进去 ⇒ 客户端拿不到成功”。
//
// 扣留分两级：响应体先缓冲在内存（memLimit）；超过后溢写到临时文件而不是放行直写，
// 因此任意大小的响应都受同一拒绝语义约束；临时文件在提交或丢弃后立即删除。
// 超过 hardLimit 的响应不再接收（Write 返回错误），由中间件按失败记录审计并拒绝。
type platformAuditBuffer struct {
	orig    gin.ResponseWriter
	headers http.Header
	body    bytes.Buffer
	status  int
	size    int
	// memLimit 是内存缓冲上限；hardLimit 是内存 + 临时文件的总上限。
	memLimit  int
	hardLimit int
	// spill 非 nil 表示响应体已溢写到临时文件，此后新写入全部进入文件。
	spill *os.File
	// total 是已接收的响应体字节数（内存 + 文件）。
	total int
	// oversized 表示已超过 hardLimit：响应不可完整扣留，必须拒绝。
	oversized bool
	logger    *slog.Logger
}

func newPlatformAuditBuffer(orig gin.ResponseWriter, memLimit int, logger *slog.Logger) *platformAuditBuffer {
	if logger == nil {
		logger = slog.Default()
	}
	return &platformAuditBuffer{orig: orig, headers: make(http.Header), status: http.StatusOK, size: -1, memLimit: memLimit, hardLimit: maxPlatformAuditSpilledResponseBytes, logger: logger}
}

// flushTo 在审计摄取成功后把扣留的响应提交到真实连接：
// 先搬响应头，再写状态码与响应体，语义与 gin 自带 responseWriter 一致；随后清理临时文件。
func (b *platformAuditBuffer) flushTo(dst gin.ResponseWriter) {
	defer b.dispose()
	b.WriteHeaderNow()
	for key, values := range b.headers {
		dst.Header().Del(key)
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(b.status)
	if b.spill != nil {
		if _, err := b.spill.Seek(0, io.SeekStart); err != nil {
			b.logger.Error("platform audit buffer: rewind spilled response", "error", err)
			return
		}
		if _, err := io.Copy(dst, b.spill); err != nil {
			b.logger.Error("platform audit buffer: replay spilled response", "error", err)
		}
		return
	}
	if b.body.Len() > 0 {
		_, _ = dst.Write(b.body.Bytes())
		return
	}
	dst.WriteHeaderNow()
}

// dispose 释放临时文件；提交、丢弃与 panic 恢复路径都会调用，可重复调用。
func (b *platformAuditBuffer) dispose() {
	if b.spill == nil {
		return
	}
	name := b.spill.Name()
	_ = b.spill.Close()
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		b.logger.Error("platform audit buffer: remove spilled response", "path", name, "error", err)
	}
	b.spill = nil
}

// spillToDisk 把已缓冲的内存内容转移到临时文件，之后的写入直接进入文件。
func (b *platformAuditBuffer) spillToDisk() error {
	file, err := os.CreateTemp("", "platform-audit-response-*.spill")
	if err != nil {
		return err
	}
	if b.body.Len() > 0 {
		if _, err := file.Write(b.body.Bytes()); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return err
		}
	}
	b.body.Reset()
	b.spill = file
	b.logger.Info("platform audit buffered response exceeded memory limit; spilling to temporary file",
		"memory_limit_bytes", b.memLimit, "hard_limit_bytes", b.hardLimit, "status", b.status)
	return nil
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
	if b.oversized {
		return 0, errPlatformAuditResponseTooLarge
	}
	if b.total+len(p) > b.hardLimit {
		// 超过硬上限：不再接收任何字节，保持“未完整扣留 ⇒ 不放行”的语义；
		// 中间件据 oversized 如实记录失败并拒绝请求。
		b.oversized = true
		b.logger.Warn("platform audit buffered response exceeded hard limit; request will be rejected",
			"hard_limit_bytes", b.hardLimit, "status", b.status)
		return 0, errPlatformAuditResponseTooLarge
	}
	b.WriteHeaderNow()
	if b.spill == nil && b.body.Len()+len(p) > b.memLimit {
		if err := b.spillToDisk(); err != nil {
			// 无法建立溢写文件时同样不能放行直写，按超限拒绝处理。
			b.oversized = true
			b.logger.Error("platform audit buffer: spill to temporary file", "error", err)
			return 0, errPlatformAuditResponseTooLarge
		}
	}
	var n int
	var err error
	if b.spill != nil {
		n, err = b.spill.Write(p)
	} else {
		n, err = b.body.Write(p)
	}
	b.total += n
	b.size += n
	return n, err
}

func (b *platformAuditBuffer) WriteString(s string) (int, error) {
	return b.Write([]byte(s))
}

func (b *platformAuditBuffer) Status() int   { return b.status }
func (b *platformAuditBuffer) Size() int     { return b.size }
func (b *platformAuditBuffer) Written() bool { return b.size != -1 }

// Flush 只标记“已写”，绝不把半成品响应提前提交到真实连接，
// 否则审计失败时将无法撤回，拒绝语义会被破坏。
func (b *platformAuditBuffer) Flush() {
	b.WriteHeaderNow()
}

func (b *platformAuditBuffer) Pusher() http.Pusher { return b.orig.Pusher() }

func (b *platformAuditBuffer) Hijack() (net.Conn, *bufio.ReadWriter, error) { return b.orig.Hijack() }

func (b *platformAuditBuffer) CloseNotify() <-chan bool { return b.orig.CloseNotify() }

func (b *platformAuditBuffer) Unwrap() http.ResponseWriter { return b.orig }
