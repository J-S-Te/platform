package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PersonnelHandoverGORMChecker checks the durable responsibility snapshot
// emitted by CRM, contract and approval adapters. Missing/failed reads are
// returned to the service and therefore fail closed before termination.
type PersonnelHandoverGORMChecker struct{ db *gorm.DB }

func NewPersonnelHandoverGORMChecker(db *gorm.DB) (*PersonnelHandoverGORMChecker, error) {
	if db == nil {
		return nil, errors.New("personnel handover database must not be nil")
	}
	return &PersonnelHandoverGORMChecker{db: db}, nil
}

type personnelHandoverRow struct {
	ID             string     `gorm:"column:id"`
	System         string     `gorm:"column:system_code"`
	ResourceType   string     `gorm:"column:resource_type"`
	ResourceID     string     `gorm:"column:resource_id"`
	CurrentOwnerID string     `gorm:"column:current_owner_id"`
	TargetOwnerID  string     `gorm:"column:target_owner_id"`
	Status         string     `gorm:"column:status"`
	CompletedBy    string     `gorm:"column:completed_by"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
}

func (c *PersonnelHandoverGORMChecker) Check(ctx context.Context, req application.PersonnelChangeRequest) (application.HandoverReport, error) {
	if req.TenantID == "" || req.UserID == "" || req.ID == "" {
		return application.HandoverReport{}, application.ErrValidation
	}
	if req.ChangeType != domain.PersonnelChangeTermination {
		return application.HandoverReport{Ready: true, Outstanding: []application.HandoverItem{}}, nil
	}
	var rows []personnelHandoverRow
	err := c.db.WithContext(ctx).Table("iam_personnel_handover_item").
		Where("tenant_id = ? AND request_id = ? AND current_owner_id = ?", req.TenantID, req.ID, req.UserID).
		Order("system_code, resource_type, resource_id").Find(&rows).Error
	if err != nil {
		return application.HandoverReport{}, err
	}
	// Empty snapshots previously let a termination pass without transferring any
	// responsibility. Fail closed until the platform-owned mandatory item exists.
	return summarizeHandoverRows(rows), nil
}

func summarizeHandoverRows(rows []personnelHandoverRow) application.HandoverReport {
	out := make([]application.HandoverItem, 0, len(rows))
	for _, row := range rows {
		if row.Status != "COMPLETED" {
			out = append(out, toHandoverItem(row))
		}
	}
	return application.HandoverReport{Ready: len(rows) > 0 && len(out) == 0, Outstanding: out}
}

func (c *PersonnelHandoverGORMChecker) List(ctx context.Context, tenant, requestID string) ([]application.HandoverItem, error) {
	var rows []personnelHandoverRow
	if err := c.db.WithContext(ctx).Table("iam_personnel_handover_item").Where("tenant_id = ? AND request_id = ?", tenant, requestID).Order("system_code, resource_type, resource_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]application.HandoverItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toHandoverItem(row))
	}
	return items, nil
}

func (c *PersonnelHandoverGORMChecker) Complete(ctx context.Context, tenant, requestID, itemID, targetUserID, operator string) (application.HandoverItem, error) {
	targetUserID = strings.TrimSpace(targetUserID)
	now := time.Now().UTC()
	var row personnelHandoverRow
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("iam_personnel_handover_item").Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND request_id = ? AND id = ?", tenant, requestID, itemID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrNotFound
			}
			return err
		}
		if row.CurrentOwnerID == targetUserID || row.Status == "COMPLETED" {
			return application.ErrConflict
		}
		var active int64
		if err := tx.Table("iam_user").Where("tenant_id = ? AND id = ? AND status = ? AND deleted_at IS NULL", tenant, targetUserID, domain.StatusActive).Count(&active).Error; err != nil {
			return err
		}
		if active != 1 {
			return fmt.Errorf("handover target must be an active user: %w", application.ErrValidation)
		}
		result := tx.Table("iam_personnel_handover_item").Where("tenant_id = ? AND request_id = ? AND id = ? AND status <> ?", tenant, requestID, itemID, "COMPLETED").Updates(map[string]any{"target_owner_id": targetUserID, "status": "COMPLETED", "completed_by": operator, "completed_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrConflict
		}
		row.TargetOwnerID = targetUserID
		row.Status = "COMPLETED"
		row.CompletedBy = operator
		row.CompletedAt = &now
		return nil
	})
	return toHandoverItem(row), err
}

func toHandoverItem(row personnelHandoverRow) application.HandoverItem {
	return application.HandoverItem{ID: row.ID, System: row.System, ResourceType: row.ResourceType, ResourceID: row.ResourceID, CurrentOwnerID: row.CurrentOwnerID, TargetOwnerID: row.TargetOwnerID, Status: row.Status, CompletedBy: row.CompletedBy, CompletedAt: row.CompletedAt}
}
