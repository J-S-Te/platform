// Package requestctx stores transport-correlated values on request contexts.
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

// WithTraceID stores a trace identifier that has already been validated by a trusted transport
// middleware. It deliberately does not read an untrusted HTTP header itself.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceID returns the trusted trace identifier associated with the current request, if one exists.
func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(traceIDKey{}).(string)
	return value
}

// WithCorrelationID stores a business correlation identifier validated by trusted transport
// middleware. The value links related operations across independently deployed applications.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, correlationID)
}

// CorrelationID returns the trusted cross-application business correlation identifier, if any.
func CorrelationID(ctx context.Context) string {
	value, _ := ctx.Value(correlationIDKey{}).(string)
	return value
}

// WithClientIP stores a client address resolved by the trusted-proxy middleware. Application
// handlers must use this value instead of reading forwarding headers themselves.
func WithClientIP(ctx context.Context, clientIP string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, clientIP)
}

// ClientIP returns the transport-validated client address, if one was resolved.
func ClientIP(ctx context.Context) string {
	value, _ := ctx.Value(clientIPKey{}).(string)
	return value
}
