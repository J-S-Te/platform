package applicationaccess

import (
	"context"
	"errors"
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
