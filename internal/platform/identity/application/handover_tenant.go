package application

import (
	"context"
	"strings"
)

// handoverTenantContextKey 是交接机器客户端的私有上下文键。平台为每个请求铸造
// application JWT，令牌必须绑定当前批次的租户；租户只能由编排器写入，不能从请求头、
// 查询参数或请求体推断，避免调用方自报扩大租户范围。
type handoverTenantContextKey struct{}

// WithHandoverTenant 把当前批次的租户写入上下文，供机器协议客户端铸造令牌头。
// 空租户不写入，保持"缺失即失败关闭"的语义。
func WithHandoverTenant(ctx context.Context, tenantID string) context.Context {
	tenant := strings.TrimSpace(tenantID)
	if tenant == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, handoverTenantContextKey{}, tenant)
}

// HandoverTenantFromContext 读取当前批次的租户；缺失时返回空串与 false。
func HandoverTenantFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	tenant, ok := ctx.Value(handoverTenantContextKey{}).(string)
	tenant = strings.TrimSpace(tenant)
	if !ok || tenant == "" {
		return "", false
	}
	return tenant, true
}
