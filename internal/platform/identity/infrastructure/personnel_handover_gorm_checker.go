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

// PersonnelHandoverGORMChecker 检查 CRM、合同与审批适配器产生的持久责任快照。读取缺失或
// 失败会原样返回给服务，在离职前失败关闭。
type PersonnelHandoverGORMChecker struct{ db *gorm.DB }

// NewPersonnelHandoverGORMChecker 构造交接检查器；数据库句柄为 nil 时返回错误。
func NewPersonnelHandoverGORMChecker(db *gorm.DB) (*PersonnelHandoverGORMChecker, error) {
	if db == nil {
		return nil, errors.New("personnel handover database must not be nil")
	}
	return &PersonnelHandoverGORMChecker{db: db}, nil
}

type personnelHandoverRow struct {
	BatchID        string     `gorm:"column:batch_id"`
	NextRetryAt    *time.Time `gorm:"column:next_retry_at"`
	Attempts       uint64     `gorm:"column:attempts"`
	LastErrorCode  string     `gorm:"column:last_error_code"`
	EvidenceRef    string     `gorm:"column:evidence_ref"`
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

// Check 汇总离职请求的责任交接就绪状态。快照为空或存在未完成项都判为未就绪；
// 非离职异动直接视为就绪，不触发责任扫描。
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
	// 空快照曾让离职在未转移任何责任的情况下通过；平台强制的责任项存在之前一律失败关闭。
	return summarizeHandoverRows(rows), nil
}

// handoverItemResolved 统一“本地已无未处理责任”的状态集合。COMPLETED 是本地人工
// 签认；TRANSFERRED/SKIPPED 是跨系统编排依据真实回执推进的结清状态。远端完成的
// 严格性由跨系统协调器的 CrossSystemComplete 核验单独把关，不在这里混同。
func handoverItemResolved(status string) bool {
	switch status {
	case "COMPLETED", application.HandoverItemTransferred, application.HandoverItemSkipped:
		return true
	default:
		return false
	}
}

func summarizeHandoverRows(rows []personnelHandoverRow) application.HandoverReport {
	out := make([]application.HandoverItem, 0, len(rows))
	for _, row := range rows {
		if handoverItemResolved(row.Status) && !(row.BatchID != "" && row.Status == "COMPLETED") {
			continue
		}
		out = append(out, toHandoverItem(row))
	}
	return application.HandoverReport{Ready: len(rows) > 0 && len(out) == 0, Outstanding: out}
}

// List 返回请求对应的全部责任项（含已完成），供交接明细页展示与对账。
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

// Complete 在事务中完成单个责任项转移：行级锁防止并发重复完成，接收人必须不同于原持有人
// 且为同租户在职用户，条件更新影响行数必须为 1 才算成功。
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
		if row.BatchID != "" || row.CurrentOwnerID == targetUserID || handoverItemResolved(row.Status) {
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
	return application.HandoverItem{ID: row.ID, BatchID: row.BatchID, NextRetryAt: row.NextRetryAt, Attempts: row.Attempts, LastErrorCode: row.LastErrorCode, EvidenceRef: row.EvidenceRef, System: row.System, ResourceType: row.ResourceType, ResourceID: row.ResourceID, CurrentOwnerID: row.CurrentOwnerID, TargetOwnerID: row.TargetOwnerID, Status: row.Status, CompletedBy: row.CompletedBy, CompletedAt: row.CompletedAt}
}
