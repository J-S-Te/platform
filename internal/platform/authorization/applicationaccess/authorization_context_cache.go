package applicationaccess

import (
	"context"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/authorizationrefresh"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
)

// authorizationContextCacheTTL 是授权上下文在 API 进程内的短缓存上限。
// 该端点本来就允许短缓存：2 秒只用于吸收页面加载/轮询产生的并发突发，撤销最多延迟 2 秒。
const authorizationContextCacheTTL = 2 * time.Second

type authorizationContextReadState struct {
	generation    uint64
	inFlight      int
	freshInFlight int
}

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
	reads   map[string]*authorizationContextReadState
}

// NewCachingAuthorizationContextResolver 构造隔离强制复核与普通突发读取的缓存。
// 参数：next 为权威授权解析器，ttl 为普通读取的短缓存有效期。
// 返回值：缓存解析器；依赖为空或缓存关闭时保留原解析行为。
func NewCachingAuthorizationContextResolver(next tokenissuer.AuthorizationContextResolver, ttl time.Duration) tokenissuer.AuthorizationContextResolver {
	if next == nil || ttl <= 0 {
		return next
	}
	return &CachingAuthorizationContextResolver{
		next: next, ttl: ttl, now: time.Now,
		entries: make(map[string]cachedAuthorizationContext),
		reads:   make(map[string]*authorizationContextReadState),
	}
}

// ResolveOIDCAuthorizationContext 读取当前有效授权，强制复核隔离旧缓存和迟到回填。
// 参数：ctx 为请求上下文，tenantID/clientID/userID 为已认证的精确授权范围。
// 返回值：独立授权快照；依赖失败不缓存且强制请求绝不返回旧快照。
func (resolver *CachingAuthorizationContextResolver) ResolveOIDCAuthorizationContext(ctx context.Context, tenantID, clientID, userID string) (tokenissuer.AuthorizationContext, error) {
	key := tenantID + "\x00" + clientID + "\x00" + userID
	now := resolver.now().UTC()
	fresh := authorizationrefresh.Forced(ctx)
	resolver.mu.Lock()
	if entry, found := resolver.entries[key]; !fresh && found && now.Before(entry.expiresAt) {
		resolver.mu.Unlock()
		return copyAuthorizationContext(entry.value), nil
	}
	state := resolver.reads[key]
	if state == nil {
		state = &authorizationContextReadState{}
		resolver.reads[key] = state
	}
	state.inFlight++
	if fresh {
		state.generation++
		state.freshInFlight++
		delete(resolver.entries, key)
	}
	generation := state.generation
	cacheable := fresh || state.freshInFlight == 0
	resolver.mu.Unlock()

	value, err := resolver.next.ResolveOIDCAuthorizationContext(ctx, tenantID, clientID, userID)
	resolver.mu.Lock()
	state.inFlight--
	if fresh {
		state.freshInFlight--
	}
	if err == nil && cacheable && generation == state.generation {
		if len(resolver.entries) > 8192 {
			for cachedKey, cached := range resolver.entries {
				if !now.Before(cached.expiresAt) {
					delete(resolver.entries, cachedKey)
				}
			}
		}
		resolver.entries[key] = cachedAuthorizationContext{
			value: copyAuthorizationContext(value), expiresAt: now.Add(resolver.ttl),
		}
	}
	// 仅保存仍有在途读取的代际，避免用户键永久积累；最后一个旧响应仍受代际约束。
	if state.inFlight == 0 {
		delete(resolver.reads, key)
	}
	resolver.mu.Unlock()
	if err != nil {
		return tokenissuer.AuthorizationContext{}, err
	}
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
