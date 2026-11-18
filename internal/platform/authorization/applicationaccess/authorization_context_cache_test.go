package applicationaccess

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/authorizationrefresh"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
)

type countingAuthorizationContextResolver struct {
	calls int
	value tokenissuer.AuthorizationContext
	err   error
}

func (r *countingAuthorizationContextResolver) ResolveOIDCAuthorizationContext(context.Context, string, string, string) (tokenissuer.AuthorizationContext, error) {
	r.calls++
	return r.value, r.err
}

// TestCachingAuthorizationContextResolverAbsorbsBursts 锁定 2 秒缓存只吸收短时突发，
// 过期后必须重新解析；失败结果不得缓存。
func TestCachingAuthorizationContextResolverAbsorbsBursts(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	next := &countingAuthorizationContextResolver{value: tokenissuer.AuthorizationContext{TenantID: "tenant-1", ClientID: "client-1", Permissions: []string{"project.read"}}}
	resolver := NewCachingAuthorizationContextResolver(next, 2*time.Second).(*CachingAuthorizationContextResolver)
	resolver.now = func() time.Time { return now }

	first, err := resolver.ResolveOIDCAuthorizationContext(context.Background(), "tenant-1", "client-1", "user-1")
	if err != nil || len(first.Permissions) != 1 || next.calls != 1 {
		t.Fatalf("first resolve value=%+v err=%v calls=%d", first, err, next.calls)
	}
	if _, err := resolver.ResolveOIDCAuthorizationContext(context.Background(), "tenant-1", "client-1", "user-1"); err != nil || next.calls != 1 {
		t.Fatalf("cached resolve err=%v calls=%d", err, next.calls)
	}
	now = now.Add(3 * time.Second)
	if _, err := resolver.ResolveOIDCAuthorizationContext(context.Background(), "tenant-1", "client-1", "user-1"); err != nil || next.calls != 2 {
		t.Fatalf("expired resolve err=%v calls=%d", err, next.calls)
	}

	now = now.Add(time.Second)
	next.err = errors.New("database unavailable")
	if _, err := resolver.ResolveOIDCAuthorizationContext(context.Background(), "tenant-1", "client-1", "user-2"); err == nil {
		t.Fatal("expected resolver error to remain visible")
	}
	next.err = nil
	if _, err := resolver.ResolveOIDCAuthorizationContext(context.Background(), "tenant-1", "client-1", "user-2"); err != nil || next.calls != 4 {
		t.Fatalf("failed result must not be cached: err=%v calls=%d", err, next.calls)
	}
}

type scriptedAuthorizationContextResolver struct {
	resolve func(context.Context) (tokenissuer.AuthorizationContext, error)
}

// ResolveOIDCAuthorizationContext 运行隔离的下游授权计算脚本。
// 参数：ctx 保留真实取消边界，其余参数为本例固定的授权范围。
// 返回值：用例控制的权威快照或依赖错误。
func (r *scriptedAuthorizationContextResolver) ResolveOIDCAuthorizationContext(ctx context.Context, _, _, _ string) (tokenissuer.AuthorizationContext, error) {
	return r.resolve(ctx)
}

// TestFreshAuthorizationBypassesCache 验证同修订号的权限时间窗变化也会立即重算。
// 参数：t 为隔离测试上下文。
// 返回值：无；返回旧权限或污染后续正常读取时失败。
func TestFreshAuthorizationBypassesCache(t *testing.T) {
	next := &countingAuthorizationContextResolver{value: tokenissuer.AuthorizationContext{AuthorizationRevision: 7, Permissions: []string{"project.read", "project.team.assign"}}}
	resolver := NewCachingAuthorizationContextResolver(next, time.Minute)
	ctx := context.Background()
	if _, err := resolver.ResolveOIDCAuthorizationContext(ctx, "T", "C", "U"); err != nil {
		t.Fatal(err)
	}
	next.value.Permissions = []string{"project.read"}
	current, err := resolver.ResolveOIDCAuthorizationContext(authorizationrefresh.WithForce(ctx), "T", "C", "U")
	if err != nil || len(current.Permissions) != 1 || current.Permissions[0] != "project.read" {
		t.Fatalf("fresh request returned stale authorization: %+v, %v", current, err)
	}
	cached, err := resolver.ResolveOIDCAuthorizationContext(ctx, "T", "C", "U")
	if err != nil || len(cached.Permissions) != 1 || next.calls != 2 {
		t.Fatalf("fresh result not cached: %+v, %v, calls=%d", cached, err, next.calls)
	}
}

// TestFreshAuthorizationFailureInvalidatesOldCache 验证强制读取失败不以旧授权成功兜底。
// 参数：t 为隔离测试上下文。
// 返回值：无；失败被吞掉或后续继续命中旧快照时失败。
func TestFreshAuthorizationFailureInvalidatesOldCache(t *testing.T) {
	next := &countingAuthorizationContextResolver{value: tokenissuer.AuthorizationContext{Permissions: []string{"project.team.assign"}}}
	resolver := NewCachingAuthorizationContextResolver(next, time.Minute)
	ctx := context.Background()
	if _, err := resolver.ResolveOIDCAuthorizationContext(ctx, "T", "C", "U"); err != nil {
		t.Fatal(err)
	}
	next.err = errors.New("dependency unavailable")
	if _, err := resolver.ResolveOIDCAuthorizationContext(authorizationrefresh.WithForce(ctx), "T", "C", "U"); err == nil {
		t.Fatal("fresh failure reused old permissions")
	}
	if _, err := resolver.ResolveOIDCAuthorizationContext(ctx, "T", "C", "U"); err == nil {
		t.Fatal("old cache survived failed fresh read")
	}
}

// TestFreshAuthorizationBlocksLateCacheFill 验证刷新前的迟到请求不能覆盖刷新结果。
// 参数：t 为隔离测试上下文。
// 返回值：无；最终缓存回退到旧角色时失败。
func TestFreshAuthorizationBlocksLateCacheFill(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	next := &scriptedAuthorizationContextResolver{resolve: func(ctx context.Context) (tokenissuer.AuthorizationContext, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return tokenissuer.AuthorizationContext{}, ctx.Err()
			}
			return tokenissuer.AuthorizationContext{AuthorizationRevision: 1, Permissions: []string{"project.team.assign"}}, nil
		}
		return tokenissuer.AuthorizationContext{AuthorizationRevision: 2, Permissions: []string{"project.read"}}, nil
	}}
	resolver := NewCachingAuthorizationContextResolver(next, time.Minute)
	done := make(chan error, 1)
	go func() { _, err := resolver.ResolveOIDCAuthorizationContext(ctx, "T", "C", "U"); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fresh, err := resolver.ResolveOIDCAuthorizationContext(authorizationrefresh.WithForce(ctx), "T", "C", "U")
	if err != nil || fresh.AuthorizationRevision != 2 {
		t.Fatalf("fresh result=%+v, %v", fresh, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := resolver.ResolveOIDCAuthorizationContext(ctx, "T", "C", "U")
	if err != nil || current.AuthorizationRevision != 2 || len(current.Permissions) != 1 || current.Permissions[0] != "project.read" {
		t.Fatalf("late result overwrote fresh authorization: %+v, %v", current, err)
	}
}
