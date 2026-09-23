package application

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrEgressBlocked 表示目标被出网策略拒绝（SEC-B5）：平台反向代理与健康探针不得
// 访问云元数据/link-local 地址，回环与私网必须由部署配置显式允许。
var ErrEgressBlocked = errors.New("egress target rejected by policy")

// EgressPolicy 是反向代理（subsystem_service_route_handler.Proxy）与健康探针
// （GetSubsystemHealthDashboard 的运行时探活）共用的出网策略（SEC-B5）。
//
// 规则（按优先级）：
//  1. link-local/链路本地（169.254.0.0/16、fe80::/10）覆盖 AWS/GCP/Azure/Tencent 等
//     云元数据端点 169.254.169.254，任何配置都不得放行；阿里云元数据 100.100.100.200
//     与 metadata.google.internal 等元数据主机名同样硬拒绝；
//  2. 回环（127.0.0.0/8、::1、0.0.0.0、localhost）默认拒绝，需
//     PLATFORM_EGRESS_ALLOW_LOOPBACK=true 显式允许；
//  3. 私网（RFC1918、fc00::/7）默认拒绝，需 PLATFORM_EGRESS_ALLOW_PRIVATE=true
//     显式允许。容器编排（本地 compose 与生产 compose）把上游子系统建在私网中，
//     启用反代/探针的部署必须在环境变量中显式打开该开关——默认 fail-closed，
//     本地开发通过同一开关恢复，不受影响；
//  4. 其余全局单播地址放行；仅接受 http/https 且不允许 URL 内嵌凭据。
//
// 校验分两层：ValidateURL 在请求入口做 URL 层拦截；DialContext 在真正建连前对
// 解析出的每个 IP 复验，防止域名解析到元数据/内网地址（DNS 重绑定）绕过入口校验。
type EgressPolicy struct {
	allowLoopback bool
	allowPrivate  bool
}

// NewEgressPolicy 构造显式策略；测试与特殊部署用它表达"显式配置允许"。
func NewEgressPolicy(allowLoopback, allowPrivate bool) *EgressPolicy {
	return &EgressPolicy{allowLoopback: allowLoopback, allowPrivate: allowPrivate}
}

// EgressPolicyFromEnv 从环境变量加载策略（文档化的两个配置项）：
//
//	PLATFORM_EGRESS_ALLOW_LOOPBACK=true|1|yes|on  允许回环目标
//	PLATFORM_EGRESS_ALLOW_PRIVATE=true|1|yes|on   允许私网目标
//
// 未设置即拒绝；link-local/元数据地址没有开关。
func EgressPolicyFromEnv() *EgressPolicy {
	return &EgressPolicy{
		allowLoopback: envFlagEnabled("PLATFORM_EGRESS_ALLOW_LOOPBACK"),
		allowPrivate:  envFlagEnabled("PLATFORM_EGRESS_ALLOW_PRIVATE"),
	}
}

// AllowLoopbackReports/AllowPrivateReports 用于诊断输出与测试断言当前策略。
func (policy *EgressPolicy) AllowLoopback() bool { return policy.allowLoopback }

func (policy *EgressPolicy) AllowPrivate() bool { return policy.allowPrivate }

func envFlagEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// metadataHostnames 是不解析为 link-local 但属于云元数据面的主机名。
var metadataHostnames = map[string]bool{
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
}

// metadataIPv4Addresses 是不位于 169.254/16 的已知云元数据地址（阿里云）。
var metadataIPv4Addresses = []string{
	"100.100.100.200",
}

// ValidateURL 在请求入口校验出网目标 URL；被拒绝时返回 ErrEgressBlocked。
func (policy *EgressPolicy) ValidateURL(raw string) error {
	target := strings.TrimSpace(raw)
	if target == "" || len(target) > 2048 {
		return ErrEgressBlocked
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return ErrEgressBlocked
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ErrEgressBlocked
	}
	if parsed.User != nil {
		return ErrEgressBlocked
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return ErrEgressBlocked
	}
	if metadataHostnames[host] || strings.HasSuffix(host, ".metadata.google.internal") {
		return ErrEgressBlocked
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return policy.validateLoopback()
	}
	if ip := net.ParseIP(host); ip != nil {
		return policy.ValidateIP(ip)
	}
	// 普通域名不在入口放行结论里做 DNS 解析：真正的逐 IP 校验由 DialContext 在建连前完成，
	// 同时避免入口校验与建连之间出现 DNS 重绑定窗口。
	return nil
}

// ValidateIP 校验一个具体 IP 是否允许作为出网目标。
func (policy *EgressPolicy) ValidateIP(ip net.IP) error {
	if ip == nil {
		return ErrEgressBlocked
	}
	// link-local 覆盖云元数据端点，无条件拒绝。
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return ErrEgressBlocked
	}
	for _, raw := range metadataIPv4Addresses {
		if ip.Equal(net.ParseIP(raw)) {
			return ErrEgressBlocked
		}
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return policy.validateLoopback()
	}
	if ip.IsPrivate() {
		return policy.validatePrivate()
	}
	if !ip.IsGlobalUnicast() {
		return ErrEgressBlocked
	}
	return nil
}

// DialContext 供 http.Transport 使用：在 TCP 建连之前对目标（含域名解析结果）执行
// 与 ValidateURL 相同的策略校验，反向代理与健康探针共用。
func (policy *EgressPolicy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	if ip := net.ParseIP(host); ip != nil {
		if err := policy.ValidateIP(ip); err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, addr)
	}
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, candidate := range resolved {
		if err := policy.ValidateIP(candidate.IP); err != nil {
			continue
		}
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrEgressBlocked
}

// NewEgressHTTPClient 返回带逐 IP 建连校验的 HTTP 客户端（健康探针使用）。
func (policy *EgressPolicy) NewEgressHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: policy.DialContext,
		},
	}
}

func (policy *EgressPolicy) validateLoopback() error {
	if policy.allowLoopback {
		return nil
	}
	return ErrEgressBlocked
}

func (policy *EgressPolicy) validatePrivate() error {
	if policy.allowPrivate {
		return nil
	}
	return ErrEgressBlocked
}
