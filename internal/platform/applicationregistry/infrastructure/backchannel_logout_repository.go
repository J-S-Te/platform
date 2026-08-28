package infrastructure

import (
	"context"
	"errors"
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
		return tx.Exec(`INSERT INTO platform_oauth_backchannel_logout_uri (oauth_client_id, logout_uri, created_at, updated_at) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE logout_uri=VALUES(logout_uri), updated_at=VALUES(updated_at)`, input.OAuthClientID, input.URI, now.UTC(), now.UTC()).Error
	})
}

// Delete 删除客户端的回调登记。
func (r *BackchannelLogoutURIRepository) Delete(ctx context.Context, tenantID, clientID string) error {
	return r.database.WithContext(ctx).Exec(`DELETE u FROM platform_oauth_backchannel_logout_uri u JOIN platform_oauth_client c ON c.id=u.oauth_client_id WHERE c.tenant_id=? AND c.id=?`, tenantID, clientID).Error
}

var _ application.BackchannelLogoutURIRepository = (*BackchannelLogoutURIRepository)(nil)
