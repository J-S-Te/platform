package credentialhttp

import (
	"testing"
	"time"
)

func TestAccountWindowLimiterRejectsRepeatedAccountAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	limiter := NewAccountWindowLimiter(2, time.Minute)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("LocalAdmin") || !limiter.Allow("localadmin") {
		t.Fatal("first two attempts should pass")
	}
	if limiter.Allow("LOCALADMIN") {
		t.Fatal("third attempt should be limited")
	}
	if !limiter.Allow("other") {
		t.Fatal("different account should have its own bucket")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("localadmin") {
		t.Fatal("expired bucket should reset")
	}
}
