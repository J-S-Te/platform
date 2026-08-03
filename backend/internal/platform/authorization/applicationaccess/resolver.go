package applicationaccess

import (
	"context"
	"errors"

	oidcapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/tokenissuer"
)

// ApplicationAuthorizationResolver 将应用级授权适配为 OIDC 声明。OAuth Client 是目标应用
// 与环境的可信来源；解析过程不接受浏览器指定应用编码，也不写死任何子系统名称。
type ApplicationAuthorizationResolver struct{ service *Service }

func NewApplicationAuthorizationResolver(service *Service) (*ApplicationAuthorizationResolver, error) {
	if service == nil {
		return nil, errors.New("application authorization resolver service must not be nil")
	}
	return &ApplicationAuthorizationResolver{service: service}, nil
}

var _ tokenissuer.AuthorizationResolver = (*ApplicationAuthorizationResolver)(nil)

func (resolver *ApplicationAuthorizationResolver) ResolveOIDCAuthorization(ctx context.Context, tenantID, clientID, userID string) (tokenissuer.AuthorizationClaims, error) {
	resolved, err := resolver.service.ResolveOIDCAuthorization(ctx, tenantID, clientID, userID)
	if err != nil {
		// “未配置”和“策略拒绝”对 OIDC 都统一为 access denied，不向客户端泄露应用授权
		// 配置是否存在；基础设施错误则保留原错误供服务端诊断。
		if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrAccessDenied) {
			return tokenissuer.AuthorizationClaims{}, oidcapplication.ErrAccessDenied
		}
		return tokenissuer.AuthorizationClaims{}, err
	}
	return tokenissuer.AuthorizationClaims{
		TenantID: resolved.TenantID, PersonID: resolved.PersonID, PrimaryOrgID: resolved.PrimaryOrgID,
		OrganizationIDs: append([]string(nil), resolved.OrganizationIDs...), Roles: append([]string(nil), resolved.Roles...),
		Permissions: append([]string(nil), resolved.Permissions...), RoleConfigHash: resolved.RoleConfigHash,
		AuthzRevision: resolved.AuthzRevision,
	}, nil
}
