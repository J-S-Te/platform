package application

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

const loginTargetStatusActive = "ACTIVE"

// LoginTargetResolveInput 定位一个预先登记的业务落地目标。全部字段必填且精确匹配；
// 解析器绝不接受或推导调用方提供的 URI，跳转地址只能来自注册数据。
type LoginTargetResolveInput struct {
	TenantID      string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
}

// LoginTarget 是内部运行时解析器所需的最小持久化视图。
// 刻意与 OAuth redirect_uri 登记相互独立，绝不可当作授权码回调地址使用。
type LoginTarget struct {
	ID            string
	TenantID      string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
	TargetURI     string
	Status        string
}

// LoginTargetRepository 在完整的租户/应用/环境边界内解析 ACTIVE 目标。
// 实现不得提供回退匹配或前缀匹配语义，跨边界的模糊命中一律视为不存在。
type LoginTargetRepository interface {
	FindActiveLoginTarget(context.Context, LoginTargetResolveInput) (LoginTarget, error)
	FindActiveEnvironment(context.Context, string, string, string) (Environment, error)
}

// LoginTargetResolver 是调用方已建立可信应用上下文之后使用的内部契约，
// 刻意不作为匿名 HTTP 面向外部的 API 暴露。
type LoginTargetResolver interface {
	ResolveActiveTargetURI(context.Context, LoginTargetResolveInput) (string, error)
}

// LoginTargetService 围绕精确持久化查询做输入校验与防御性结果复核。
// 目标缺失、非 ACTIVE 或边界不一致一律按 not found 报告，让调用方
// 失败关闭，而不是跳转到未批准的地址。
type LoginTargetService struct {
	repository LoginTargetRepository
}

// NewLoginTargetService 构造内部登录目标解析器；repository 缺失时拒绝构造，
// 避免出现绕过注册数据的服务实例。
func NewLoginTargetService(repository LoginTargetRepository) (*LoginTargetService, error) {
	if repository == nil {
		return nil, errors.New("application login target repository must not be nil")
	}
	return &LoginTargetService{repository: repository}, nil
}

// ResolveActiveTargetURI 解析恰好一个 ACTIVE 的业务落地目标。返回的 URI 是
// 注册数据管控的内容，不是 OAuth redirect_uri，不得接入授权码流程。
//
// TargetURI 接受两种形式：
//   - 绝对 https URL：原样返回。
//   - 以单个 '/' 开头的绝对路径：基于所属环境的公开 BaseURL 与可选 PathPrefix
//     解析，管理员无需在注册数据里写死门户主机与端口。相对路径在 BaseURL
//     不可用时按解析失败处理并报告 not found。
func (service *LoginTargetService) ResolveActiveTargetURI(ctx context.Context, input LoginTargetResolveInput) (string, error) {
	if !validLoginTargetResolveInput(input) {
		return "", ErrValidation
	}

	target, err := service.repository.FindActiveLoginTarget(ctx, input)
	if err != nil {
		return "", err
	}
	if target.TenantID != input.TenantID || target.ApplicationID != input.ApplicationID ||
		target.EnvironmentID != input.EnvironmentID || target.TargetCode != input.TargetCode ||
		target.Status != loginTargetStatusActive || !validLoginTargetURI(target.TargetURI) {
		// 即使仓储错误地返回了其他租户或环境的数据，也在应用层失败关闭；登录目标属于
		// 安全跳转边界，不能只依赖调用方传入的 application_id。
		return "", ErrNotFound
	}
	if isRelativeLoginTargetURI(target.TargetURI) {
		environment, err := service.repository.FindActiveEnvironment(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
		if err != nil || environment.BaseURL == nil || *environment.BaseURL == "" {
			return "", ErrNotFound
		}
		return joinEnvironmentBaseURLAndTargetURI(*environment.BaseURL, environment.PathPrefix, target.TargetURI)
	}
	return target.TargetURI, nil
}

func validLoginTargetResolveInput(input LoginTargetResolveInput) bool {
	return validLoginTargetIdentifier(input.TenantID) &&
		validLoginTargetIdentifier(input.ApplicationID) &&
		validLoginTargetIdentifier(input.EnvironmentID) &&
		validCode(input.TargetCode, 64)
}

func validLoginTargetIdentifier(value string) bool {
	return len(value) == 26 && validIdentifier(value)
}

// validLoginTargetURI 同时接受绝对公开 URL 与门户下的绝对路径。
//
//   - 绝对公开 URL：scheme 必须为 https、host 非空、无 userinfo、无 opaque 成分。
//   - 相对路径：单个前导 '/'，不含 scheme、query/fragment，仅限 ASCII 可打印字符。
//
// 纯主机名字符串（如 "example.com"）、协议相对地址（"//x"）及带协议的杂项
// 一律拒绝，防止本函数被滥用为开放重定向构造器。
func validLoginTargetURI(value string) bool {
	if value == "" || len(value) > 2048 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	if isRelativeLoginTargetURI(value) {
		return true
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil &&
		parsed.Opaque == "" && strings.EqualFold(parsed.Scheme, "https")
}

// isRelativeLoginTargetURI 判定 value 是否为解析器允许基于所属环境 BaseURL
// 展开的安全绝对路径。
func isRelativeLoginTargetURI(value string) bool {
	if !validPortalPath(value, 2048) {
		return false
	}
	return true
}

// joinEnvironmentBaseURLAndTargetURI 由三个独立管理的值拼出对外可见的落地 URL：
// 门户 BaseURL、环境 PathPrefix 与目标相对路径。内部 UpstreamURL 绝不参与
// 浏览器重定向，避免把集群拓扑泄露给外部跳转链。
func joinEnvironmentBaseURLAndTargetURI(baseURL string, pathPrefix *string, targetURI string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrNotFound
	}
	if !isRelativeLoginTargetURI(targetURI) {
		return "", ErrNotFound
	}

	prefix := ""
	if pathPrefix != nil && *pathPrefix != "" {
		prefix = strings.TrimRight(*pathPrefix, "/")
		if !validPortalPath(prefix, 128) {
			return "", ErrNotFound
		}
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/") + prefix + targetURI
	parsed.RawPath = ""
	return parsed.String(), nil
}
