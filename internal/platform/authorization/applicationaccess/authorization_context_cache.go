package applicationaccess

import (
	"context"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
)

// authorizationContextCacheTTL 是授权上下文在 API 进程内的短缓存上限。
// 该端点本来就允许短缓存：2 秒只用于吸收页面加载/轮询产生的并发突发，撤销最多延迟 2 秒。
const authorizationContextCacheTTL = 2 * time.Second

type cachedAuthorizationContext struct {
	value     tokenissuer.AuthorizationContext
	expiresAt time.Time
}

// CachingAuthorizationContextResolver 包一层进程内短缓存，降低同一用户在同一时刻
// 反复解析应用授权时的平台数据库压力；失败不缓存，权限/范围仍由解析结果决定。
type CachingAuthorizationContextResolver struct {
	next    tokenissuer.AuthorizationContextResolver
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]cachedAuthorizationContext
}

// NewCachingAuthorizationContextResolver 构造缓存装饰器；next 为空时返回 nil，
// 调用方按未配置处理，不静默放行。
func NewCachingAuthorizationContextResolver(next tokenissuer.AuthorizationContextResolver, ttl time.Duration) tokenissuer.AuthorizationContextResolver {
	if next == nil || ttl <= 0 {
		return next
	}
	return &CachingAuthorizationContextResolver{next: next, ttl: ttl, now: time.Now, entries: make(map[string]cachedAuthorizationContext)}
}

func (resolver *CachingAuthorizationContextResolver) ResolveOIDCAuthorizationContext(ctx context.Context, tenantID, clientID, userID string) (tokenissuer.AuthorizationContext, error) {
	key := tenantID + "\x00" + clientID + "\x00" + userID
	now := resolver.now().UTC()
	resolver.mu.Lock()
	entry, found := resolver.entries[key]
	resolver.mu.Unlock()
	if found && now.Before(entry.expiresAt) {
		return copyAuthorizationContext(entry.value), nil
	}
	value, err := resolver.next.ResolveOIDCAuthorizationContext(ctx, tenantID, clientID, userID)
	if err != nil {
		return tokenissuer.AuthorizationContext{}, err
	}
	resolver.mu.Lock()
	if len(resolver.entries) > 8192 {
		for cachedKey, cached := range resolver.entries {
			if !now.Before(cached.expiresAt) {
				delete(resolver.entries, cachedKey)
			}
		}
	}
	resolver.entries[key] = cachedAuthorizationContext{value: copyAuthorizationContext(value), expiresAt: now.Add(resolver.ttl)}
	resolver.mu.Unlock()
	return copyAuthorizationContext(value), nil
}

func copyAuthorizationContext(value tokenissuer.AuthorizationContext) tokenissuer.AuthorizationContext {
	out := value
	out.Roles = append([]string(nil), value.Roles...)
	out.Permissions = append([]string(nil), value.Permissions...)
	out.CompatibleCatalogVersions = append([]string(nil), value.CompatibleCatalogVersions...)
	out.CompatibleRoleConfigHashes = append([]string(nil), value.CompatibleRoleConfigHashes...)
	out.DataScopes = append([]tokenissuer.DataScope(nil), value.DataScopes...)
	return out
}
