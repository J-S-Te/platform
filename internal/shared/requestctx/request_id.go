// Package requestctx 在请求上下文中保存与传输层关联的可信值。
package requestctx

import "context"

type requestIDKey struct{}
type traceIDKey struct{}
type correlationIDKey struct{}
type clientIPKey struct{}

// WithRequestID 保存由传输层生成的请求 ID，供日志关联与响应头回显使用。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID 返回当前请求的请求 ID；不存在时返回空值。
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
type clientIPSourceKey struct{}

func WithClientIP(ctx context.Context, clientIP string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, clientIP)
}

// WithClientIPSource 保存可信代理中间件解析出的客户端地址与来源标记。
// 来源由解析过程产生（DIRECT / TRUSTED_PROXY / UNKNOWN），下游不得自行推断。
func WithClientIPSource(ctx context.Context, clientIP, source string) context.Context {
	ctx = context.WithValue(ctx, clientIPKey{}, clientIP)
	return context.WithValue(ctx, clientIPSourceKey{}, source)
}

// ClientIPWithSource 返回客户端地址与解析来源；来源为空表示调用方只写了地址。
func ClientIPWithSource(ctx context.Context) (string, string) {
	clientIP, _ := ctx.Value(clientIPKey{}).(string)
	source, _ := ctx.Value(clientIPSourceKey{}).(string)
	return clientIP, source
}

// ClientIP 返回经过传输层校验的客户端地址；未解析出时返回空值。
func ClientIP(ctx context.Context) string {
	value, _ := ctx.Value(clientIPKey{}).(string)
	return value
}
