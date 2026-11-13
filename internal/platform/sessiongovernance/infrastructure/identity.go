package infrastructure

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccountIdentityForLock 在事务开始前读取不可变账号属主，仅用于确定人员锁键。
// 参数：ctx为请求，db为非事务连接，tenant/account为账号范围；不替代业务校验。
// 返回值：人员标识；账号不存在或无人员时为空，数据库错误原样返回。
func AccountIdentityForLock(ctx context.Context, db *gorm.DB, tenant, account string) (string, error) {
	var owner struct{ UserID *string }
	err := db.WithContext(ctx).Table("iam_account").Select("user_id").
		Where("tenant_id = ? AND id = ?", tenant, account).Take(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil || owner.UserID == nil {
		return "", err
	}
	return *owner.UserID, nil
}

// LockIdentityTx 在其他写锁及一致性读之前锁定人员，不提前固定事务快照。
// 参数：tx为事务，tenant/identity为事务外解析的锁键；空人员表示无人员账号。
// 返回值：无；人员不存在或数据库失败时返回错误，不代表已通过业务授权。
func LockIdentityTx(tx *gorm.DB, tenant, identity string) error {
	if identity == "" {
		return nil
	}
	var user struct{ ID string }
	err := tx.Table("iam_user").Select("id").
		Where("tenant_id = ? AND id = ?", tenant, identity).
		Clauses(clause.Locking{Strength: "UPDATE"}).Take(&user).Error
	return err
}

// activeIdentity 在线复核IAM人员、租户、账号和锁定期限，不依赖Keycloak投影延迟。
// 参数：tx为生命周期事务，tenant/identity为验签身份，now为平台时钟。
// 返回值：无有效身份时未认证，数据库故障保持原错误；共享锁与中央撤销串行。
func activeIdentity(tx *gorm.DB, tenant, identity string, now time.Time) error {
	var row struct{ ID string }
	err := tx.Table("iam_user AS u").Select("u.id").
		Joins("JOIN iam_tenant AS t ON t.id=u.tenant_id AND t.status='ACTIVE'").
		Where("u.tenant_id=? AND u.id=? AND u.status='ACTIVE' AND (u.valid_until IS NULL OR u.valid_until>?)", tenant, identity, now).
		Clauses(clause.Locking{Strength: "SHARE"}).Take(&row).Error
	if err != nil {
		return inactiveMissing(err)
	}
	var account struct{ ID string }
	err = tx.Table("iam_account").Select("id").
		Where("tenant_id=? AND user_id=? AND status='ACTIVE'", tenant, identity).
		Where("(valid_until IS NULL OR valid_until>?) AND (locked_until IS NULL OR locked_until<=?)", now, now).
		Clauses(clause.Locking{Strength: "SHARE"}).Take(&account).Error
	return inactiveMissing(err)
}
