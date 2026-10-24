package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 不设置文件外键：已取消的 v2 上传可能已分配文件 ID，
// 但尚未创建 file_object；经认证的上传会话可证明归属。
type bindingReleaseModel struct {
	ID, TenantID, ApplicationID, FileID   string
	ResourceType, ResourceID, BindingType string
	CreatedAt                             time.Time
	CreatedBy                             string
}

func (bindingReleaseModel) TableName() string { return "file_binding_release" }

func bindingTuple(db *gorm.DB, b domain.FileBinding) *gorm.DB {
	return db.Where("tenant_id = ? AND application_id = ? AND file_id = ? AND resource_type = ? AND resource_id = ? AND binding_type = ?", b.TenantID, b.ApplicationID, b.FileID, b.ResourceType, b.ResourceID, b.BindingType)
}

// ReleaseBinding 与 CreateBinding 共用文件行锁。无论哪个请求先完成，
// 已提交的释放操作都不能被进行中或迟到的绑定重试撤销。
func (repository *Repository) ReleaseBinding(ctx context.Context, b domain.FileBinding) error {
	return mapError(repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var object fileObjectModel
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_id = ? AND id = ?", b.TenantID, b.ApplicationID, b.FileID).Take(&object).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			var session uploadV2SessionModel
			if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_id = ? AND file_id = ? AND resource_type = ? AND resource_id = ? AND binding_type = ?", b.TenantID, b.ApplicationID, b.FileID, b.ResourceType, b.ResourceID, b.BindingType).Take(&session).Error; err != nil {
				return mapError(err)
			}
		} else if err != nil {
			return mapError(err)
		}
		record := bindingReleaseModel{ID: b.ID, TenantID: b.TenantID, ApplicationID: b.ApplicationID, FileID: b.FileID, ResourceType: b.ResourceType, ResourceID: b.ResourceID, BindingType: b.BindingType, CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error; err != nil {
			return mapError(err)
		}
		// 即使插入遇到唯一性冲突，也要验证确切的墓碑记录。
		var persisted bindingReleaseModel
		if err := bindingTuple(tx, b).Take(&persisted).Error; err != nil {
			return mapError(err)
		}
		return mapError(bindingTuple(tx.Model(&fileBindingModel{}), b).Where("status = ?", "ACTIVE").Update("status", "DISABLED").Error)
	}))
}

func rejectReleasedBinding(tx *gorm.DB, b domain.FileBinding) error {
	var record bindingReleaseModel
	err := bindingTuple(tx, b).Take(&record).Error
	if err == nil {
		return application.ErrConflict
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return mapError(err)
}
