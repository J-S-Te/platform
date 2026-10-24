package positiongrant

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 模板写入与任务分配写入否则会以相反顺序获取模板/分配锁。这类低频配置写入必须先按
// 租户串行化再执行任何读取，以保证当前读校验与原子物化成立。不同租户之间保持独立，
// 无需 schema/advisory 锁。
func (s *Service) templateWriteTransaction(ctx context.Context, tenantID string, write func(*gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var tenant struct{ ID string }
		err := tx.Table("iam_tenant").Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", tenantID).Take(&tenant).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock tenant template configuration: %w", err)
		}
		return write(tx)
	})
}
