package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const inboxHeartbeat = 12 * time.Second

// InboxStream 发出收件箱变更信号，收件箱正文仍由原有受保护的 API 返回。
type InboxStream struct {
	subscribe    func(string, string) (<-chan struct{}, func())
	authenticate func(context.Context, string) (authctx.Principal, error)
	cookieName   string
	logger       *slog.Logger
}

// NewInboxStream 用途：绑定同源平台会话认证与在线收件箱订阅。
// 参数：subscribe 注册租户收件人，authenticate 重新核验会话，cookieName 是平台 Cookie 名，logger 记录写入异常。
// 返回值：接入已认证路由组的流式处理器。
func NewInboxStream(
	subscribe func(string, string) (<-chan struct{}, func()),
	authenticate func(context.Context, string) (authctx.Principal, error),
	cookieName string,
	logger *slog.Logger,
) *InboxStream {
	return &InboxStream{subscribe, authenticate, cookieName, logger}
}

// ServeHTTP 用途：在当前会话权限内发送无正文的收件箱变更和心跳。
// 参数：writer 为流式 HTTP 响应，request 为已由平台认证的请求。
// 返回值：无。会话失效、网络中断或写超时均结束流。
func (stream *InboxStream) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	cookie, err := request.Cookie(stream.cookieName)
	if !ok || err != nil || cookie.Value == "" || principal.SessionID == "" ||
		principal.Tenant.ID == "" || principal.User.ID == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	changes, unsubscribe := stream.subscribe(principal.Tenant.ID, principal.User.ID)
	defer unsubscribe()
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("X-Accel-Buffering", "no")
	if err := flushInboxEvent(writer, "event: ready\ndata: {}\n\n"); err != nil {
		stream.logger.Debug("open notification stream", "error", err)
		return
	}
	stream.forward(writer, request.Context(), changes, principal, cookie.Value)
}

// forward 用途：逐条核验当前会话后发送变更或心跳，并按连接上下文退出。
// 参数：writer 为流响应，ctx 为请求上下文，changes 为收件箱更新，initial 为初始身份，token 为原会话凭据。
// 返回值：无。
func (stream *InboxStream) forward(writer http.ResponseWriter, ctx context.Context, changes <-chan struct{}, initial authctx.Principal, token string) {
	ticker := time.NewTicker(inboxHeartbeat)
	defer ticker.Stop()
	for {
		event := ": heartbeat\n\n"
		select {
		case <-ctx.Done():
			return
		case _, open := <-changes:
			if !open {
				return
			}
			event = "event: inbox-changed\ndata: {}\n\n"
		case <-ticker.C:
		}
		authCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		current, err := stream.authenticate(authCtx, token)
		cancel()
		if err != nil || current.SessionID != initial.SessionID ||
			current.Tenant.ID != initial.Tenant.ID || current.User.ID != initial.User.ID {
			return
		}
		if err := flushInboxEvent(writer, event); err != nil {
			stream.logger.Debug("write notification stream", "error", err)
			return
		}
	}
}

// flushInboxEvent 用途：为单次 SSE 写入刷新截止时间并立即冲刷网关。
// 参数：writer 为支持流式写入的响应，event 为完整的 SSE 帧。
// 返回值：写入或冲刷失败时返回错误。
func flushInboxEvent(writer http.ResponseWriter, event string) error {
	controller := http.NewResponseController(writer)
	if err := controller.SetWriteDeadline(time.Now().Add(2 * inboxHeartbeat)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := io.WriteString(writer, event); err != nil {
		return err
	}
	return controller.Flush()
}
