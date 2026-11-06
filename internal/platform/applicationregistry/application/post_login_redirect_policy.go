package application

import (
	"net"
	"net/url"
	"strings"
)

// PostLoginRedirectPolicy 汇总"登录成功后允许浏览器跳转到的绝对地址"来源集合。
// 允许集合只能由装配层从受信配置汇总（平台公开 origin、CORS 允许来源、OIDC issuer
// 与显式运维白名单），绝不接受请求输入，也不从已登记的存量数据反推。
//
// 零值表示"未配置来源集合"：此时保留既有结构校验口径（http/https 绝对地址均按登记
// 数据处理），不限制主机名。装配层（bootstrap）始终注入真实允许集合，因此运行时不会
// 停留在零值语义；零值只为未接线的单元测试与嵌入调用保留既有行为。
type PostLoginRedirectPolicy struct {
	// allowedHosts 记录允许来源主机名；值为该来源是否已被显式声明为明文 HTTP 部署
	// （origin 条目写成 http:// 时置真）。隔离网络的明文本地/局域网部署依赖这条
	// 显式记录，网络中间人可改写的明文来源不会因"登记过"而自动放行。
	allowedHosts map[string]bool
}

// NewPostLoginRedirectPolicy 由 origin 或裸主机条目构造策略。条目可以是完整 origin
// （如 https://portal.example.com:8443、http://192.168.1.20:8081）或裸主机名
// （如 portal.example.com）；畸形条目被直接忽略，让配置错误退化为"该来源不被允许"，
// 而不是放开跳转。
func NewPostLoginRedirectPolicy(entries ...string) PostLoginRedirectPolicy {
	hosts := make(map[string]bool, len(entries))
	for _, entry := range entries {
		host, plaintext, ok := postLoginRedirectHost(entry)
		if !ok {
			continue
		}
		hosts[host] = hosts[host] || plaintext
	}
	if len(hosts) == 0 {
		return PostLoginRedirectPolicy{}
	}
	return PostLoginRedirectPolicy{allowedHosts: hosts}
}

// Enabled 报告该策略是否已配置来源集合。
func (policy PostLoginRedirectPolicy) Enabled() bool { return len(policy.allowedHosts) > 0 }

// AllowsHost 报告主机是否属于允许集合。主机比较大小写不敏感并忽略端口，避免同一
// 来源因默认端口写法不同被判为不同来源。未配置集合时返回 true（零值语义）。
func (policy PostLoginRedirectPolicy) AllowsHost(host string) bool {
	if !policy.Enabled() {
		return true
	}
	_, ok := policy.allowedHosts[strings.ToLower(strings.TrimSpace(host))]
	return ok
}

// allowsPlaintextHost 报告主机是否被显式声明为明文 HTTP 来源。只有运维在配置里
// 写成 http://<host> 的条目才算，裸主机名与 https 条目都不会顺带放行明文。
func (policy PostLoginRedirectPolicy) allowsPlaintextHost(host string) bool {
	return policy.allowedHosts[strings.ToLower(strings.TrimSpace(host))]
}

// ValidAbsoluteRedirect 判定一个绝对跳转地址是否允许：
//   - 必须是绝对 URL、包含主机、无 userinfo/opaque 成分；
//   - HTTPS：主机必须位于允许集合内；
//   - 明文 HTTP：只允许回环主机（localhost/127.0.0.0/8/[::1]），或该主机已被运维
//     显式写成 http:// 来源（隔离网络的明文部署）；其余明文来源一律拒绝；
//   - 未配置来源集合时只做结构校验，保持既有行为。
func (policy PostLoginRedirectPolicy) ValidAbsoluteRedirect(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "https") && !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	if !policy.Enabled() {
		return true
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return policy.AllowsHost(parsed.Hostname())
	}
	return isLoopbackRedirectHost(parsed.Hostname()) || policy.allowsPlaintextHost(parsed.Hostname())
}

// ValidTargetURI 判定登录目标：相对门户路径始终允许，绝对地址按
// ValidAbsoluteRedirect 追加协议与来源判定。
func (policy PostLoginRedirectPolicy) ValidTargetURI(value string) bool {
	if !validLoginTargetURI(value) {
		return false
	}
	if isRelativeLoginTargetURI(value) {
		return true
	}
	return policy.ValidAbsoluteRedirect(value)
}

// ValidOptionalBaseURL 在结构化公开基址校验之上追加来源集合判定。BaseURL 与
// 相对登录目标拼接后就是浏览器最终跳转地址，因此必须与绝对目标同口径。
func (policy PostLoginRedirectPolicy) ValidOptionalBaseURL(value *string) bool {
	if !validOptionalBaseURL(value) {
		return false
	}
	if value == nil || *value == "" {
		return true
	}
	return policy.ValidAbsoluteRedirect(*value)
}

// isLoopbackRedirectHost 判定主机是否为本机回环地址：localhost 与 127.0.0.0/8、::1。
func isLoopbackRedirectHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// postLoginRedirectHost 从 origin 或裸主机条目提取规范化主机名，并报告该来源是否
// 被显式声明为明文 HTTP 部署（origin scheme 为 http）。
func postLoginRedirectHost(entry string) (host string, plaintext bool, ok bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", false, false
	}
	if strings.Contains(entry, "://") {
		parsed, err := url.Parse(entry)
		if err != nil || parsed.User != nil || parsed.Opaque != "" {
			return "", false, false
		}
		host := strings.ToLower(parsed.Hostname())
		if !validRedirectPolicyHost(host) {
			return "", false, false
		}
		return host, strings.EqualFold(parsed.Scheme, "http"), true
	}
	if strings.ContainsAny(entry, " \t\r\n/?#@") {
		return "", false, false
	}
	if value, _, err := net.SplitHostPort(entry); err == nil {
		entry = value
	}
	entry = strings.ToLower(strings.Trim(strings.TrimSpace(entry), "[]"))
	if !validRedirectPolicyHost(entry) {
		return "", false, false
	}
	return entry, false, true
}

// validRedirectPolicyHost 对允许集合条目做保守的形态校验：只接受 IP 字面量或
// 由字母、数字、点、连字符、下划线组成的主机名，避免把自由文本当成主机。
func validRedirectPolicyHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	for _, character := range host {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			character == '.' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return !strings.HasPrefix(host, ".") && !strings.HasSuffix(host, ".")
}
