package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

type backchannelLogoutURIModel struct {
	OAuthClientID string    `gorm:"column:oauth_client_id;primaryKey"`
	LogoutURI     string    `gorm:"column:logout_uri;not null"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName 指定 GORM 行映射的 Back-Channel Logout URI 表名。
func (backchannelLogoutURIModel) TableName() string { return "platform_oauth_backchannel_logout_uri" }

// BackchannelLogoutURIRepository 持久化单个 OAuth Client 的标准 Back-Channel Logout URI。
type BackchannelLogoutURIRepository struct{ database *gorm.DB }

// NewBackchannelLogoutURIRepository 创建回调地址仓储。
func NewBackchannelLogoutURIRepository(database *gorm.DB) (*BackchannelLogoutURIRepository, error) {
	if database == nil {
		return nil, errors.New("back-channel logout URI database is required")
	}
	return &BackchannelLogoutURIRepository{database: database}, nil
}

// Get 返回指定租户客户端已登记的地址。
func (r *BackchannelLogoutURIRepository) Get(ctx context.Context, tenantID, clientID string) (string, error) {
	var row backchannelLogoutURIModel
	err := r.database.WithContext(ctx).Table(row.TableName()).Joins("JOIN platform_oauth_client c ON c.id = platform_oauth_backchannel_logout_uri.oauth_client_id").Where("c.tenant_id = ? AND c.id = ? AND c.status = ?", tenantID, clientID, "ACTIVE").Take(&row).Error
	return row.LogoutURI, err
}

// Set 原子新增或替换已登记回调地址，并再次校验地址格式。
func (r *BackchannelLogoutURIRepository) Set(ctx context.Context, input application.BackchannelLogoutURIUpdate, now time.Time) error {
	if application.ValidateBackchannelLogoutURI(input.URI, false) != nil {
		return application.ErrInvalidBackchannelLogoutURI
	}
	return r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var client struct{ ID, TenantID, Status string }
		if err := tx.Table("platform_oauth_client").Where("id = ? AND tenant_id = ? AND status = ?", input.OAuthClientID, input.TenantID, "ACTIVE").Take(&client).Error; err != nil {
			return err
		}
		return UpsertBackchannelLogoutURIWithin(tx, input.OAuthClientID, input.URI, now)
	})
}

// Delete 删除客户端的回调登记。
func (r *BackchannelLogoutURIRepository) Delete(ctx context.Context, tenantID, clientID string) error {
	return r.database.WithContext(ctx).Exec(`DELETE u FROM platform_oauth_backchannel_logout_uri u JOIN platform_oauth_client c ON c.id=u.oauth_client_id WHERE c.tenant_id=? AND c.id=?`, tenantID, clientID).Error
}

// UpsertBackchannelLogoutURIWithin 在调用方事务内登记（或幂等替换）客户端的
// Back-Channel Logout 端点。接入流程用它把"客户端创建"与"登出端点登记"放在同一个
// 数据库事务里：任一步失败都必须整体回滚，避免出现"能登录但永远收不到登出通知"的
// 半接入状态。ON DUPLICATE KEY UPDATE 使受控重放/存量回填天然幂等（BIZ-XEVT2-07）。
//
// uri 为空表示显式 opt-out：不写登记行，由应用层负责审计留痕。
// 校验口径与应用层一致或更严：绝不接受明文 HTTP 到非回环地址。
func UpsertBackchannelLogoutURIWithin(transaction *gorm.DB, oauthClientID, uri string, now time.Time) error {
	oauthClientID = strings.TrimSpace(oauthClientID)
	uri = strings.TrimSpace(uri)
	// 显式 opt-out：没有登记行要写，也就无需事务；审计由应用层负责。
	if uri == "" {
		return nil
	}
	// 协议校验先于依赖检查：端点不合规是输入问题，与事务句柄是否就绪无关。
	if application.ValidateBackchannelLogoutURI(uri, true) != nil {
		return application.ErrInvalidBackchannelLogoutURI
	}
	if transaction == nil {
		return errors.New("back-channel logout URI transaction is required")
	}
	if oauthClientID == "" {
		return application.ErrValidation
	}
	return transaction.Exec(backchannelLogoutURIUpsertSQL, oauthClientID, uri, now.UTC(), now.UTC()).Error
}

// backchannelLogoutURIUpsertSQL 是唯一的登记写语句：ON DUPLICATE KEY UPDATE 让接入、
// 管理 API 与存量回填的重放都保持幂等，不会因重复登记产生第二条行或报唯一键冲突。
const backchannelLogoutURIUpsertSQL = `INSERT INTO platform_oauth_backchannel_logout_uri (oauth_client_id, logout_uri, created_at, updated_at) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE logout_uri=VALUES(logout_uri), updated_at=VALUES(updated_at)`

var _ application.BackchannelLogoutURIRepository = (*BackchannelLogoutURIRepository)(nil)
