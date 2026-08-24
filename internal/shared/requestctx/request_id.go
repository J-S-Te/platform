// Package requestctx 在请求上下文中保存与传输层关联的可信值。
package requestctx

import "context"

type requestIDKey struct{}
type traceIDKey struct{}
type correlationIDKey struct{}
type clientIPKey struct{}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// WithTraceID 保存已由可信传输层中间件校验过的追踪号。
// 函数不会自行读取不可信的 HTTP 请求头。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceID 返回当前请求关联的可信追踪号；不存在时返回空值。
func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(traceIDKey{}).(string)
	return value
}

// WithCorrelationID 保存已由可信传输层中间件校验过的业务关联号。
// 该值用于关联独立部署的多个应用中的相关操作。
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, correlationID)
}

// CorrelationID 返回可信的跨应用业务关联号；不存在时返回空值。
func CorrelationID(ctx context.Context) string {
	value, _ := ctx.Value(correlationIDKey{}).(string)
	return value
}

// WithClientIP 保存可信代理中间件解析出的客户端地址。
// 业务处理器必须使用该值，不得自行读取转发请求头。
func WithClientIP(ctx context.Context, clientIP string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, clientIP)
}

// ClientIP 返回经过传输层校验的客户端地址；未解析出时返回空值。
func ClientIP(ctx context.Context) string {
	value, _ := ctx.Value(clientIPKey{}).(string)
	return value
}
