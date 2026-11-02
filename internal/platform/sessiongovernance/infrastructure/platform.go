package infrastructure

import (
	"context"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LinkPlatformTx 在IAM创建事务内关联锚点，避免认证成功但中央登记丢失。
// 参数：tx为已有事务，anchor为可信IAM原始期限，centralID为OIDC已解析锚点或空。
// 返回值：不越过原期限的中央记录；冲突、过期或存储失败返回错误并由外层回滚。
func LinkPlatformTx(tx *gorm.DB, anchor domain.PlatformAnchor, centralID string) (domain.Session, error) {
	now := anchor.LastInteractiveAt.UTC()
	p, err := policy(tx, anchor.TenantID)
	if err != nil {
		return domain.Session{}, err
	}
	var session domain.Session
	if centralID != "" {
		session, _, err = activeAnchor(tx, centralID, now)
		if err != nil {
			return session, err
		}
		if session.TenantID != anchor.TenantID || session.IdentityID != anchor.IdentityID {
			return session, domain.ErrBindingConflict
		}
	} else {
		session, err = domain.NewSession(anchor.ID, anchor.TenantID, anchor.IdentityID, anchor.AuthenticatedAt, now, p, &anchor)
		if err != nil {
			return session, err
		}
		if err = tx.Table("sgov_session").Create(&session).Error; err != nil {
			return session, err
		}
	}
	link := platformLink{IAMSessionID: anchor.ID, SessionID: session.ID}
	return session, tx.Table("sgov_platform_link").Create(&link).Error
}

// ResolveOIDC 在签名验证后为控制台解析原始上游周期，不创建新八小时。
// 参数：ctx为请求，identity为经过完整验证的声明，parent为当前可信中央锚点或空，now为时钟。
// 返回值：同sid原锚点或新鲜认证锚点；旧sid失效或冲突时拒绝。
func (s *Store) ResolveOIDC(ctx context.Context, identity VerifiedIdentity, parent string, now time.Time) (domain.Session, error) {
	var result domain.Session
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p, err := policy(tx, identity.TenantID)
		if err != nil {
			return err
		}
		result, err = resolveIdentity(tx, identity, parent, now, p)
		return err
	})
	return result, err
}

// PlatformState 读取IAM到中央会话的精确关联；普通认证不会延长窗口。
// 参数：ctx为请求，iamID为已签名Cookie会话ID，now为时钟，interact为显式同源交互。
// 返回值：中央期限；旧会话无绑定时要求重新登录，绝不补造期限。
func (s *Store) PlatformState(ctx context.Context, iamID string, now time.Time, interact bool) (Context, error) {
	var result Context
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link platformLink
		if err := tx.Table("sgov_platform_link").Where("iam_session_id = ?", iamID).Take(&link).Error; err != nil {
			return inactiveMissing(err)
		}
		anchor, p, err := activeAnchor(tx, link.SessionID, now)
		if err != nil {
			return err
		}
		if interact {
			anchor, err = anchor.Interact(now, p)
			if err != nil {
				return err
			}
			if err = tx.Table("sgov_session").Where("id = ?", anchor.ID).Updates(map[string]any{"last_interactive_at": anchor.LastInteractiveAt, "version": anchor.Version}).Error; err != nil {
				return err
			}
		}
		result = view(anchor, bindingRow{ClientID: "platform", ApplicationCode: "platform"}, p, now)
		return nil
	})
	return result, err
}

// RevokePlatformTx 将IAM关联的同一中央周期同步撤销，不误杀其他浏览器。
// 参数：tx为IAM撤销事务，iamID为目标Cookie会话，now为时钟，reason为固定原因码。
// 返回值：数据库错误；没有旧中央关联时保持IAM撤销幂等。
func RevokePlatformTx(tx *gorm.DB, iamID string, now time.Time, reason string) error {
	links := tx.Table("sgov_platform_link").Select("session_id").Where("iam_session_id = ?", iamID)
	return tx.Table("sgov_session").Where("id IN (?) AND revoked_at IS NULL", links).
		Updates(map[string]any{"revoked_at": now.UTC(), "revoke_reason": reason, "version": gorm.Expr("version + 1")}).Error
}

// RevokeIdentityTx 撤销同租户身份的全部中央周期，包括未创建平台Cookie的业务周期。
// 参数：tx为已有事务，tenant/identity为可信IAM范围，now/reason为撤销证据，keepID为要继承的周期或空。
// 返回值：参数或数据库错误；已经撤销的周期保持幂等。
func RevokeIdentityTx(tx *gorm.DB, tenant, identity string, now time.Time, reason, keepID string) error {
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(identity) == "" || reason == "" {
		return domain.ErrValidation
	}
	var user struct{ ID string }
	if err := tx.Table("iam_user").Select("id").Where("tenant_id=? AND id=?", tenant, identity).
		Clauses(clause.Locking{Strength: "UPDATE"}).Take(&user).Error; err != nil {
		return inactiveMissing(err)
	}
	query := tx.Table("sgov_session").Where("tenant_id = ? AND identity_id = ? AND revoked_at IS NULL", tenant, identity)
	if keepID != "" {
		query = query.Where("id <> ?", keepID)
	}
	return query.Updates(map[string]any{"revoked_at": now.UTC(), "revoke_reason": reason,
		"version": gorm.Expr("version + 1")}).Error
}

// RevokeAccountTx 从持久化账号解析身份，不依赖IAM会话列表枚举业务周期。
// 参数：tx为账号变更事务，tenant/account为可信范围，now/reason为撤销证据。
// 返回值：账号不存在或存储失败时错误；无人员绑定的机器账号不撤销其他身份。
func RevokeAccountTx(tx *gorm.DB, tenant, account string, now time.Time, reason string) error {
	var row struct{ UserID *string }
	if err := tx.Table("iam_account").Select("user_id").Where("tenant_id = ? AND id = ?", tenant, account).Take(&row).Error; err != nil {
		return inactiveMissing(err)
	}
	if row.UserID == nil {
		return nil
	}
	return RevokeIdentityTx(tx, tenant, *row.UserID, now, reason, "")
}

// RegisteredClient 精确核验网页回调及已同步别名的同租户、应用、环境边界。
// 参数：ctx为请求，id为已登记client_id，redirect为精确回调URI。
// 返回值：可信客户端投影；未登记或失效返回参数错误，数据库异常保持原错误。
func (s *Store) RegisteredClient(ctx context.Context, id, redirect string) (Client, error) {
	direct := s.db.Table("platform_oauth_redirect_uri AS registered").Select("1").
		Where("registered.oauth_client_id=c.id AND registered.redirect_uri=?", redirect)
	// 既有Keycloak别名可同时用作平台到Keycloak的broker客户端，其本身登记的回调
	// 并非业务应用回调。仅复用同应用环境标准网页客户端已有的精确URI，不改OAuth登记。
	mapped := s.db.Table("keycloak_application_client_mapping AS mapping").Select("1").
		Joins(`JOIN platform_oauth_client AS browser ON browser.tenant_id=mapping.tenant_id
			AND browser.application_id=mapping.application_id
			AND browser.environment_id=mapping.environment_id AND browser.status='ACTIVE'`).
		Joins(`JOIN platform_oauth_redirect_uri AS registered ON registered.oauth_client_id=browser.id
			AND registered.redirect_uri=?`, redirect).
		Joins(`JOIN platform_oauth_grant_type AS browser_grant ON browser_grant.oauth_client_id=browser.id
			AND browser_grant.grant_type='authorization_code'`).
		Where(`mapping.tenant_id=c.tenant_id AND mapping.application_id=c.application_id
			AND mapping.environment_id=c.environment_id AND mapping.keycloak_client_id=c.client_id
			AND mapping.status='SYNCED' AND browser.id<>c.id
			AND browser.client_id=CONCAT(a.code,'-',e.environment,'-web')`)
	var result Client
	err := s.db.WithContext(ctx).Table("platform_oauth_client AS c").
		Select("c.client_id AS id,c.tenant_id,a.code AS application_code,e.environment AS environment_code").
		Joins("JOIN platform_application AS a ON a.id=c.application_id AND a.tenant_id=c.tenant_id AND a.status='ACTIVE'").
		Joins("JOIN platform_application_environment AS e ON e.id=c.environment_id AND e.application_id=a.id AND e.tenant_id=c.tenant_id AND e.status='ACTIVE'").
		Joins("JOIN iam_tenant AS t ON t.id=c.tenant_id AND t.status='ACTIVE'").
		Joins("JOIN platform_oauth_grant_type AS g ON g.oauth_client_id=c.id AND g.grant_type='authorization_code'").
		Where("c.client_id=? AND c.status='ACTIVE'", id).
		Where("(EXISTS (?) OR EXISTS (?))", direct, mapped).Take(&result).Error
	if err == gorm.ErrRecordNotFound {
		return result, domain.ErrValidation
	}
	return result, err
}
