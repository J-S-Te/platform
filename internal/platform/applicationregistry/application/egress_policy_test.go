package application

import (
	"context"
	"errors"
	"testing"
)

// 安全（SEC-B5）负向测试：云元数据/link-local 永不放行；回环与私网必须显式配置才允许。

func TestEgressPolicyBlocksMetadataAddressesEvenWhenPermissive(t *testing.T) {
	t.Parallel()
	policy := NewEgressPolicy(true, true)
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.169.254/computeMetadata/v1/",
		"http://169.254.0.1/",
		"http://[fe80::1]/",
		"http://100.100.100.200/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
	} {
		if err := policy.ValidateURL(target); !errors.Is(err, ErrEgressBlocked) {
			t.Fatalf("ValidateURL(%q) = %v, want ErrEgressBlocked", target, err)
		}
	}
	// 建连层同样拦截（IP 字面量在拨号前拒绝，不发起任何连接）。
	if _, err := policy.DialContext(context.Background(), "tcp", "169.254.169.254:80"); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("DialContext(metadata) = %v, want ErrEgressBlocked", err)
	}
	if err := policy.ValidateIP(nil); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("ValidateIP(nil) = %v, want ErrEgressBlocked", err)
	}
}

func TestEgressPolicyLoopbackRequiresExplicitConfig(t *testing.T) {
	t.Parallel()
	denyByDefault := NewEgressPolicy(false, false)
	for _, target := range []string{
		"http://127.0.0.1:8080/",
		"http://localhost:8080/health",
		"http://[::1]/",
	} {
		if err := denyByDefault.ValidateURL(target); !errors.Is(err, ErrEgressBlocked) {
			t.Fatalf("ValidateURL(%q) = %v, want ErrEgressBlocked", target, err)
		}
	}
	if _, err := denyByDefault.DialContext(context.Background(), "tcp", "127.0.0.1:9"); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("DialContext(loopback) = %v, want ErrEgressBlocked", err)
	}

	allowLoopback := NewEgressPolicy(true, false)
	if err := allowLoopback.ValidateURL("http://127.0.0.1:8080/"); err != nil {
		t.Fatalf("显式允许回环后仍被拒绝: %v", err)
	}
	if _, err := allowLoopback.DialContext(context.Background(), "tcp", "127.0.0.1:9"); err == nil || errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("显式允许回环后建连不应被策略拒绝: %v", err)
	}
}

func TestEgressPolicyPrivateRequiresExplicitConfig(t *testing.T) {
	t.Parallel()
	denyByDefault := NewEgressPolicy(false, false)
	for _, target := range []string{
		"http://10.1.2.3/api",
		"http://192.168.1.10/",
		"http://172.16.0.9/",
		"http://[fd00::1]/",
	} {
		if err := denyByDefault.ValidateURL(target); !errors.Is(err, ErrEgressBlocked) {
			t.Fatalf("ValidateURL(%q) = %v, want ErrEgressBlocked", target, err)
		}
	}
	// 未显式允许私网时，拨号层也要拒绝（域名解析到私网同样被这一层兜住）。
	if _, err := denyByDefault.DialContext(context.Background(), "tcp", "10.1.2.3:8080"); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("DialContext(private) = %v, want ErrEgressBlocked", err)
	}
	// 公网地址在默认策略下放行。
	if err := denyByDefault.ValidateURL("http://93.184.216.34/"); err != nil {
		t.Fatalf("公网目标被拒绝: %v", err)
	}
	allowPrivate := NewEgressPolicy(false, true)
	if err := allowPrivate.ValidateURL("http://10.1.2.3/api"); err != nil {
		t.Fatalf("显式允许私网后仍被拒绝: %v", err)
	}
}

func TestEgressPolicyRejectsNonHTTPSchemesAndCredentials(t *testing.T) {
	t.Parallel()
	policy := NewEgressPolicy(true, true)
	for _, target := range []string{
		"ftp://10.1.2.3/file",
		"gopher://10.1.2.3/",
		"http://user:pass@10.1.2.3/",
		"",
		"not a url",
	} {
		if err := policy.ValidateURL(target); !errors.Is(err, ErrEgressBlocked) {
			t.Fatalf("ValidateURL(%q) = %v, want ErrEgressBlocked", target, err)
		}
	}
}

func TestEgressPolicyFromEnvFlags(t *testing.T) {
	t.Setenv("PLATFORM_EGRESS_ALLOW_LOOPBACK", "")
	t.Setenv("PLATFORM_EGRESS_ALLOW_PRIVATE", "")
	policy := EgressPolicyFromEnv()
	if policy.AllowLoopback() || policy.AllowPrivate() {
		t.Fatalf("未配置时必须 fail-closed: loopback=%v private=%v", policy.AllowLoopback(), policy.AllowPrivate())
	}
	if err := policy.ValidateURL("http://127.0.0.1/"); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("未配置时回环必须被拒绝: %v", err)
	}

	t.Setenv("PLATFORM_EGRESS_ALLOW_LOOPBACK", "true")
	t.Setenv("PLATFORM_EGRESS_ALLOW_PRIVATE", "1")
	policy = EgressPolicyFromEnv()
	if !policy.AllowLoopback() || !policy.AllowPrivate() {
		t.Fatalf("显式配置后必须放行: loopback=%v private=%v", policy.AllowLoopback(), policy.AllowPrivate())
	}
	if err := policy.ValidateURL("http://10.1.2.3/"); err != nil {
		t.Fatalf("显式允许私网后仍被拒绝: %v", err)
	}
	// 即便两个开关都打开，元数据地址依旧硬拒绝。
	if err := policy.ValidateURL("http://169.254.169.254/"); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("元数据地址必须硬拒绝: %v", err)
	}

	t.Setenv("PLATFORM_EGRESS_ALLOW_LOOPBACK", "false")
	t.Setenv("PLATFORM_EGRESS_ALLOW_PRIVATE", "no")
	policy = EgressPolicyFromEnv()
	if policy.AllowLoopback() || policy.AllowPrivate() {
		t.Fatalf("显式 false 必须拒绝: loopback=%v private=%v", policy.AllowLoopback(), policy.AllowPrivate())
	}
}
