package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

type tokenMiddleware struct {
	verifier *security.ApplicationJWTManager
}

func (middleware tokenMiddleware) wrap(next http.Handler, permission string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := strings.TrimSpace(request.Header.Get("Authorization"))
		if !strings.HasPrefix(header, "Bearer ") {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		// 已知限制（AUD-2026-026，本轮不修——需平台侧新增客户端状态查询端点，属跨服务设计
		// 变更）：此处只做 EdDSA 验签与 scope 断言，不回查客户端登记的禁用状态，被禁用客户端
		// 在令牌有效期内仍可通过网关上传/下载。internal/shared/security/application_jwt.go 中
		// ApplicationTokenClaims 的既有警告（“认证器仍会回查当前客户端登记；禁用客户端或收回
		// scope 不能仅依赖 Token 到期”）同样适用于本网关。后续方向：平台提供带短 TTL 缓存的
		// 客户端状态查询端点，网关验签后回查并对已禁用客户端 fail-closed。
		claims, err := middleware.verifier.Verify(strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")), time.Now().UTC())
		if err != nil || !hasScope(claims.Scopes, permission) {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		// 机器令牌代表应用客户端，没有平台用户主体。owner_user_id 在 filetask 中是 26 位
		// ULID，把 client_id 放进去会超长；机器上传的文件归属为空（owner_user_id NULL），
		// 由 application_id 与绑定关系承载归属。Account.ID 仍是申请所属应用，供上传时校验。
		principal := authctx.Principal{SessionID: claims.ClientID, Tenant: authctx.ReferenceName{ID: claims.TenantID}, User: authctx.ReferenceName{ID: ""}, Account: authctx.ReferenceName{ID: claims.ApplicationID, Code: claims.ApplicationCode}, PermissionCodes: claims.Scopes}
		ctx := authctx.WithPrincipal(request.Context(), principal)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func hasScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == required || scope == "*" {
			return true
		}
	}
	return false
}
