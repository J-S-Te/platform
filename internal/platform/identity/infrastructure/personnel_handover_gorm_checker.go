package infrastructure

import (
	"context"
	"errors"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/gorm"
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
	System         string `gorm:"column:system_code"`
	ResourceType   string `gorm:"column:resource_type"`
	ResourceID     string `gorm:"column:resource_id"`
	CurrentOwnerID string `gorm:"column:current_owner_id"`
	TargetOwnerID  string `gorm:"column:target_owner_id"`
	Status         string `gorm:"column:status"`
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
		Where("tenant_id = ? AND request_id = ? AND current_owner_id = ? AND status IN ?", req.TenantID, req.ID, req.UserID, []string{"PENDING", "BLOCKED"}).
		Order("system_code, resource_type, resource_id").Find(&rows).Error
	if err != nil {
		return application.HandoverReport{}, err
	}
	out := make([]application.HandoverItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, application.HandoverItem{System: row.System, ResourceType: row.ResourceType, ResourceID: row.ResourceID, CurrentOwnerID: row.CurrentOwnerID, TargetOwnerID: row.TargetOwnerID, Status: row.Status})
	}
	return application.HandoverReport{Ready: len(out) == 0, Outstanding: out}, nil
}
