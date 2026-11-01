package domain

import (
	"errors"
	"testing"
	"time"
)

// TestCentralSessionActivitySharesOneImmutableLifetime 验证活动只推进统一空闲窗口。
// 参数：t为测试上下文。
// 返回值：无；空闲、绝对期限或复活判断错误时失败。
func TestCentralSessionActivitySharesOneImmutableLifetime(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	session, err := NewSession("central-1", "tenant-1", "person-1", start, start, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if session.ValidAt(start.Add(30*time.Minute), policy) {
		t.Fatal("a session with only background requests survived 30 minutes")
	}
	updated, err := session.Interact(start.Add(29*time.Minute), policy)
	if err != nil || !updated.ValidAt(start.Add(58*time.Minute), policy) {
		t.Fatalf("trusted activity did not extend the shared idle window: %v", err)
	}
	if !updated.ExpiresAt.Equal(start.Add(8 * time.Hour)) {
		t.Fatal("interactive activity changed the original eight-hour expiry")
	}
	if _, err := session.Interact(start.Add(30*time.Minute), policy); !errors.Is(err, ErrInactive) {
		t.Fatalf("an expired session was revived: %v", err)
	}
}

// TestAbsoluteBoundaryRejectsEvenContinuousActivity 验证连续操作不能延长八小时上限。
// 参数：t为测试上下文。
// 返回值：无；边界不符合固定预期时失败。
func TestAbsoluteBoundaryRejectsEvenContinuousActivity(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	session, err := NewSession("central-1", "tenant-1", "person-1", start, start, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for minute := 20; minute < 480; minute += 20 {
		session, err = session.Interact(start.Add(time.Duration(minute)*time.Minute), DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
	}
	if session.ValidAt(start.Add(8*time.Hour), DefaultPolicy()) {
		t.Fatal("continuous interaction extended the original login beyond eight hours")
	}
}

// TestBridgeInheritsOlderPlatformDeadline 验证新开RP不能重算平台原始八小时。
// 参数：t为测试上下文。
// 返回值：无；期限或交互基准被错误延长时失败。
func TestBridgeInheritsOlderPlatformDeadline(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(7 * time.Hour)
	parent := &PlatformAnchor{ID: "platform-1", TenantID: "tenant-1", IdentityID: "person-1",
		AuthenticatedAt: start, LastInteractiveAt: now.Add(-time.Minute), ExpiresAt: start.Add(8 * time.Hour)}
	session, err := NewSession("central-1", "tenant-1", "person-1", now.Add(-10*time.Minute), now, DefaultPolicy(), parent)
	if err != nil || !session.ExpiresAt.Equal(start.Add(8*time.Hour)) {
		t.Fatalf("new RP did not inherit older platform expiry: %v", err)
	}
	if !session.LastInteractiveAt.Equal(parent.LastInteractiveAt) {
		t.Fatal("automatic bridge was counted as a new user interaction")
	}
	parent.IdentityID = "person-2"
	if _, err := NewSession("central-2", "tenant-1", "person-1", now, now, DefaultPolicy(), parent); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("cross-identity platform anchor accepted: %v", err)
	}
}

// TestMissingOrStaleAuthenticationDoesNotStartANewWindow 验证缺原始时间或旧认证不可续命。
// 参数：t为测试上下文。
// 返回值：无；错误原始时间被采用时失败。
func TestMissingOrStaleAuthenticationDoesNotStartANewWindow(t *testing.T) {
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	for _, authAt := range []time.Time{{}, now.Add(-31 * time.Minute), now.Add(time.Minute)} {
		if _, err := NewSession("central-1", "tenant-1", "person-1", authAt, now, DefaultPolicy(), nil); err == nil {
			t.Fatalf("unsafe initial authentication time accepted: %v", authAt)
		}
	}
}

// TestExistingAnchorsAreNeverMerged 验证不同已存在会话锚点必须拒绝自动归并。
// 参数：t为测试上下文。
// 返回值：无；串绑或幂等复用判断错误时失败。
func TestExistingAnchorsAreNeverMerged(t *testing.T) {
	for _, pair := range [][2]string{{"", "a"}, {"a", ""}, {"a", "a"}} {
		if err := CheckAnchorBinding(pair[0], pair[1]); err != nil {
			t.Fatalf("safe link rejected: %v", err)
		}
	}
	if err := CheckAnchorBinding("a", "b"); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("two existing authentication periods were merged: %v", err)
	}
}
