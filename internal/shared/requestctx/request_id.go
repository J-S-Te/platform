// Package requestctx stores request-scoped metadata in context.Context.
package requestctx

import "context"

type requestIDKey struct{}

// WithRequestID stores the correlation identifier associated with an inbound request.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID returns the request correlation identifier or an empty string when middleware has
// not populated it. Handlers should rely on the HTTP middleware rather than generating their own.
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}
