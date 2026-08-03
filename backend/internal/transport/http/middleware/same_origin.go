package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequireSameOrigin 保护 Cookie 认证的 OIDC 同意写操作，阻止外站借用用户会话授权或撤销授权。
// Origin 协议只包含 scheme 与 host，因此比较时有意忽略 issuer 路径，但任何缺失或非法 Origin
// 都按失败关闭处理。
func RequireSameOrigin(issuer string) gin.HandlerFunc {
	allowedOrigin := issuerOrigin(issuer)

	return func(context *gin.Context) {
		origin := normalizedOrigin(context.GetHeader("Origin"))
		if allowedOrigin == "" || origin == "" || origin != allowedOrigin {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
			return
		}
		context.Next()
	}
}

// RequireAllowedOriginForUnsafeMethods 对 Cookie 写接口执行 CSRF 防护。所有不安全方法必须携带
// 明确允许的 Origin；Origin 被代理剥离或缺失时也拒绝。非浏览器自动化应走 Bearer/服务账号边界。
func RequireAllowedOriginForUnsafeMethods(origins ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if normalized := issuerOrigin(origin); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}

	return func(context *gin.Context) {
		if !isUnsafeMethod(context.Request.Method) {
			context.Next()
			return
		}

		originHeader := strings.TrimSpace(context.GetHeader("Origin"))
		if originHeader != "" {
			origin := normalizedOrigin(originHeader)
			if _, ok := allowed[origin]; !ok || origin == "" {
				context.Abort()
				httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
				return
			}
			context.Next()
			return
		}

		// Sec-Fetch-Site 仅作为浏览器侧纵深信号，既不普遍存在也不是身份凭据，不能据此放行缺失 Origin。
		context.Abort()
		httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
	}
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func issuerOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

func normalizedOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

// RequireAllowedOriginForUnsafeMethodsOrBearer 保留浏览器 CSRF 防线，同时允许无 Origin 的后端
// OAuth Bearer 调用。这里只严格判定凭据语法，签名、过期和客户端绑定仍由后续认证中间件完成。
func RequireAllowedOriginForUnsafeMethodsOrBearer(origins ...string) gin.HandlerFunc {
	browserGuard := RequireAllowedOriginForUnsafeMethods(origins...)
	return func(context *gin.Context) {
		if !isUnsafeMethod(context.Request.Method) {
			context.Next()
			return
		}

		// 只要带 Cookie 就按浏览器请求处理，即使同时出现 Authorization；否则攻击者可附加伪 Bearer
		// 把 Cookie 请求降级成不校验 Origin 的服务端请求。
		if strings.TrimSpace(context.GetHeader("Cookie")) != "" {
			browserGuard(context)
			return
		}

		// 请求一旦声明 Origin，它就是权威浏览器信号；格式正确的 Bearer 也不能覆盖明确的跨域来源。
		if strings.TrimSpace(context.GetHeader("Origin")) != "" {
			browserGuard(context)
			return
		}

		// cross-site 值虽不是认证信息，却足以证明请求来自浏览器上下文，应回到 Origin 防护链拒绝。
		if strings.EqualFold(strings.TrimSpace(context.GetHeader("Sec-Fetch-Site")), "cross-site") {
			browserGuard(context)
			return
		}

		if hasStrictBearerAuthorization(context.GetHeader("Authorization")) {
			context.Next()
			return
		}
		browserGuard(context)
	}
}

// hasStrictBearerAuthorization 只接受 OAuth token68 字符集及末尾可选填充，并复用 bearerToken
// 的 scheme、字段数和长度限制；目的只是防止任意非空 Authorization 头绕过 CSRF。
func hasStrictBearerAuthorization(header string) bool {
	token, ok := bearerToken(header)
	if !ok {
		return false
	}

	seenTokenCharacter := false
	seenPadding := false
	for _, character := range token {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			strings.ContainsRune("-._~+/", character):
			if seenPadding {
				return false
			}
			seenTokenCharacter = true
		case character == '=':
			if !seenTokenCharacter {
				return false
			}
			seenPadding = true
		default:
			return false
		}
	}
	return seenTokenCharacter
}
