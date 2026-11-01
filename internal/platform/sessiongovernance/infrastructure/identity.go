package infrastructure

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
