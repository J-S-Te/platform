package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	sessionstore "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// accountIdentityForLock 在中央模式的事务外确定不可变账号属主。
// 参数：ctx为请求，tenant/account为待操作账号范围。
// 返回值：人员锁键；旧模式或无人员账号为空，存储错误原样返回。
func (repository *GORMRepository) accountIdentityForLock(ctx context.Context, tenant, account string) (string, error) {
	if repository.central == nil {
		return "", nil
	}
	return sessionstore.AccountIdentityForLock(ctx, repository.database, tenant, account)
}

// lockLoginIdentity 在账号之前锁定当前人员，与中央会话检查和撤销保持同序。
// 参数：tx为登录事务，account为已验证账号范围，now为本次登录时钟。
// 返回值：无；人员失效返回未认证，数据库失败保留依赖错误。
func lockLoginIdentity(tx *gorm.DB, account domain.LoginAccount, now time.Time) error {
	var user userModel
	// 接管会升级到人员排他锁，提前取得可避免先锁账号再等待人员的反向锁环。
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
		Where("id = ? AND tenant_id = ? AND status = ?", account.UserID,
			account.TenantID, domain.StatusActive).
		Where("valid_until IS NULL OR valid_until > ?", now).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("lock identity before successful login: %w", err)
	}
	return nil
}
