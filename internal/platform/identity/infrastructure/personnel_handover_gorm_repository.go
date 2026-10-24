package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// newHandoverID 生成交接控制记录的主键（ULID，按时间有序）。
func newHandoverID(now time.Time) (string, error) { return ulid.Generator{}.New(now.UTC()) }

// isHandoverDuplicate 同时识别 GORM 翻译后的重复键与驱动原始 1062；
// 生产连接开启了 TranslateError，测试与其它连接可能没有，两者都必须正确判定。
func isHandoverDuplicate(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var mysqlError *mysql.MySQLError
	return errors.As(err, &mysqlError) && mysqlError.Number == 1062
}

// mapHandoverError 只把"未找到"映射为领域错误，其余数据库错误原样上抛，避免掩盖真实故障。
func mapHandoverError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	return err
}

// PersonnelHandoverGORMRepository 是交接控制面的持久化实现：批次、系统冻结状态、
// 权威责任项与真实回执。它只保存平台侧控制记录，业务责任仍由各子系统持有。
type PersonnelHandoverGORMRepository struct{ db *gorm.DB }

// 编排服务只依赖端口；这里在编译期确认仓储满足该端口。
var _ application.HandoverOrchestrationRepository = (*PersonnelHandoverGORMRepository)(nil)
var _ application.HandoverPendingCompletionRepository = (*PersonnelHandoverGORMRepository)(nil)

// NewPersonnelHandoverGORMRepository 构造交接控制面仓储；数据库句柄为 nil 时返回错误。
func NewPersonnelHandoverGORMRepository(db *gorm.DB) (*PersonnelHandoverGORMRepository, error) {
	if db == nil {
		return nil, errors.New("personnel handover database must not be nil")
	}
	return &PersonnelHandoverGORMRepository{db: db}, nil
}

type personnelHandoverBatchRow struct {
	ID, TenantID, RequestID, UserID, Status string
	CancelRequestedAt, CompletedAt          *time.Time
	Version                                 uint64
	CreatedAt, UpdatedAt                    time.Time
}

// TableName 指定 GORM 行映射的交接批次表名。
func (personnelHandoverBatchRow) TableName() string { return "iam_personnel_handover_batch" }

type personnelHandoverSystemRow struct {
	BatchID, TenantID, SystemCode, Capability string
	Frozen                                    bool
	FrozenAt                                  *time.Time
	DiscoveredItems                           uint64
	BlockingReason                            *string
	CompletedAt                               *time.Time
	UpdatedAt                                 time.Time
}

// TableName 指定 GORM 行映射的批次系统状态表名。
func (personnelHandoverSystemRow) TableName() string { return "iam_personnel_handover_system" }

type personnelHandoverItemRow struct {
	ID, TenantID, RequestID, BatchID, SystemCode     string
	ResourceType, ResourceID, Category, ResourceName string
	CurrentOwnerID                                   string
	TargetOwnerID                                    *string
	Status                                           string
	ExpectedVersion, AppliedVersion                  uint64
	IdempotencyKey, LeaseID                          *string
	LeaseDeadline, NextRetryAt                       *time.Time
	Attempts                                         uint64
	LastErrorCode, EvidenceRef                       *string
	Version                                          uint64
	CreatedAt, UpdatedAt                             time.Time
	CompletedBy                                      *string
	CompletedAt                                      *time.Time
}

// TableName 指定 GORM 行映射的交接责任项表名。
func (personnelHandoverItemRow) TableName() string { return "iam_personnel_handover_item" }

type personnelHandoverReceiptRow struct {
	ID, TenantID, BatchID, ItemID, SystemCode, IdempotencyKey, Status string
	AppliedVersion                                                    uint64
	ReasonCode, EvidenceRef                                           *string
	ReceivedAt, CreatedAt                                             time.Time
}

// TableName 指定 GORM 行映射的转移回执表名。
func (personnelHandoverReceiptRow) TableName() string { return "iam_personnel_handover_receipt" }

// EnsureBatch 幂等地为一次人员异动请求创建交接批次。
func (repository *PersonnelHandoverGORMRepository) EnsureBatch(ctx context.Context, tenantID, requestID, userID string, now time.Time) (application.HandoverBatch, error) {
	tenantID, requestID, userID = strings.TrimSpace(tenantID), strings.TrimSpace(requestID), strings.TrimSpace(userID)
	if tenantID == "" || requestID == "" || userID == "" {
		return application.HandoverBatch{}, application.ErrValidation
	}
	batchID, err := newHandoverID(now)
	if err != nil {
		return application.HandoverBatch{}, err
	}
	row := personnelHandoverBatchRow{
		ID: batchID, TenantID: tenantID, RequestID: requestID, UserID: userID,
		Status: application.HandoverBatchDiscovering, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	// 显式重复键判定：MySQL 的 INSERT IGNORE 会连同 CHECK/FK 违规一起静默吞掉，
	// 因此这里只在确认是重复键时走幂等分支，其他错误一律上抛。
	result := repository.db.WithContext(ctx).Create(&row)
	if result.Error != nil {
		if isHandoverDuplicate(result.Error) {
			return repository.GetBatch(ctx, tenantID, requestID)
		}
		return application.HandoverBatch{}, mapHandoverError(result.Error)
	}
	return toHandoverBatch(row), nil
}

// GetBatchByID 按批次主键读取（派发循环与完成/取消使用）。
func (repository *PersonnelHandoverGORMRepository) GetBatchByID(ctx context.Context, tenantID, batchID string) (application.HandoverBatch, error) {
	var row personnelHandoverBatchRow
	if err := repository.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, batchID).Take(&row).Error; err != nil {
		return application.HandoverBatch{}, mapHandoverError(err)
	}
	return toHandoverBatch(row), nil
}

// GetBatch 按租户与异动请求 ID 读取交接批次（每请求恰好一个批次）。
func (repository *PersonnelHandoverGORMRepository) GetBatch(ctx context.Context, tenantID, requestID string) (application.HandoverBatch, error) {
	var row personnelHandoverBatchRow
	if err := repository.db.WithContext(ctx).Where("tenant_id = ? AND request_id = ?", tenantID, requestID).Take(&row).Error; err != nil {
		return application.HandoverBatch{}, mapHandoverError(err)
	}
	return toHandoverBatch(row), nil
}

// UpsertSystemState 记录系统适用性；重复上报保持既有冻结状态不变。
func (repository *PersonnelHandoverGORMRepository) UpsertSystemState(ctx context.Context, tenantID, batchID, system, capability, blockingReason string, now time.Time) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(batchID) == "" || strings.TrimSpace(system) == "" {
		return application.ErrValidation
	}
	var reason *string
	if strings.TrimSpace(blockingReason) != "" {
		value := strings.TrimSpace(blockingReason)
		reason = &value
	}
	row := personnelHandoverSystemRow{
		BatchID: batchID, TenantID: tenantID, SystemCode: system, Capability: capability,
		BlockingReason: reason, UpdatedAt: now.UTC(),
	}
	return mapHandoverError(repository.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "batch_id"}, {Name: "system_code"}},
		DoUpdates: clause.AssignmentColumns([]string{"capability", "blocking_reason", "updated_at"}),
	}).Create(&row).Error)
}

// MarkFrozen 在业务系统确认冻结后落库；冻结必须先于责任清单返回。
func (repository *PersonnelHandoverGORMRepository) MarkFrozen(ctx context.Context, tenantID, batchID, system string, frozenAt time.Time, discovered int) error {
	result := repository.db.WithContext(ctx).Model(&personnelHandoverSystemRow{}).
		Where("tenant_id = ? AND batch_id = ? AND system_code = ?", tenantID, batchID, system).
		Updates(map[string]any{"frozen": true, "frozen_at": frozenAt.UTC(), "discovered_items": discovered, "updated_at": frozenAt.UTC()})
	if result.Error != nil {
		return mapHandoverError(result.Error)
	}
	// MySQL 对"值未变化"的 UPDATE 报 0 行；重复冻结是幂等操作，不能误判为记录缺失。
	if result.RowsAffected == 0 {
		var count int64
		if err := repository.db.WithContext(ctx).Model(&personnelHandoverSystemRow{}).
			Where("tenant_id = ? AND batch_id = ? AND system_code = ?", tenantID, batchID, system).Count(&count).Error; err != nil {
			return mapHandoverError(err)
		}
		if count == 0 {
			return application.ErrNotFound
		}
	}
	return nil
}

// InsertResponsibilityItems 幂等落库权威责任项：同一 (租户, 请求, 系统, 资源) 只保留一行。
func (repository *PersonnelHandoverGORMRepository) InsertResponsibilityItems(ctx context.Context, tenantID, batchID, requestID, userID, targetUserID string, items []application.HandoverResponsibility, now time.Time) (int, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(batchID) == "" || len(items) == 0 {
		return 0, nil
	}
	inserted := 0
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, item := range items {
			itemID, err := newHandoverID(now)
			if err != nil {
				return err
			}
			key := application.HandoverIdempotencyKey(batchID, itemID)
			// 承接人由管理员指定；未指定时留空并在派发阶段拦下，绝不能默认成离职者本人。
			var target *string
			if strings.TrimSpace(targetUserID) != "" {
				value := strings.TrimSpace(targetUserID)
				target = &value
			}
			row := personnelHandoverItemRow{
				ID: itemID, TenantID: tenantID, RequestID: requestID, BatchID: batchID, SystemCode: item.System,
				ResourceType: item.ResourceType, ResourceID: item.ResourceID, Category: item.Category, ResourceName: item.ResourceName,
				CurrentOwnerID: userID, TargetOwnerID: target, Status: application.HandoverItemPending,
				ExpectedVersion: item.Version, IdempotencyKey: &key, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
			}
			result := tx.Create(&row)
			if result.Error != nil {
				if isHandoverDuplicate(result.Error) {
					// 旧人工入口可能把批次责任误写 COMPLETED。只有权威系统仍返回原持有人、
					// 从未派发且没有回执时才恢复待转移；本地人工项和真实转移终态均不触碰。
					if item.OwnerUserID == userID {
						if err := tx.Model(&personnelHandoverItemRow{}).
							Where("tenant_id = ? AND request_id = ? AND batch_id = ? AND system_code = ? AND resource_type = ? AND resource_id = ? AND current_owner_id = ? AND status = 'COMPLETED' AND attempts = 0", tenantID, requestID, batchID, item.System, item.ResourceType, item.ResourceID, userID).
							Where("NOT EXISTS (SELECT 1 FROM iam_personnel_handover_receipt r WHERE r.tenant_id = ? AND r.item_id = iam_personnel_handover_item.id)", tenantID).
							Updates(map[string]any{"status": application.HandoverItemPending, "expected_version": item.Version, "completed_by": nil, "completed_at": nil, "last_error_code": "MANUAL_REMOTE_COMPLETION_RESET", "updated_at": now.UTC(), "version": gorm.Expr("version + 1")}).Error; err != nil {
							return err
						}
					}
					continue
				}
				return result.Error
			}
			inserted++
		}
		return nil
	})
	return inserted, mapHandoverError(err)
}

// ClaimNextItem 以行锁 + 租约领取一条待派发项；同一项在租约到期前不会被第二个执行者领取。
func (repository *PersonnelHandoverGORMRepository) ClaimNextItem(ctx context.Context, tenantID, batchID, leaseID string, leaseDeadline, now time.Time) (application.HandoverItem, bool, error) {
	var claimed application.HandoverItem
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var batch personnelHandoverBatchRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, batchID).Take(&batch).Error; err != nil {
			return err
		}
		if batch.CancelRequestedAt != nil || (batch.Status != application.HandoverBatchFrozen && batch.Status != application.HandoverBatchTransferring) {
			return application.ErrConflict
		}
		var row personnelHandoverItemRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("tenant_id = ? AND batch_id = ? AND status = ?", tenantID, batchID, application.HandoverItemPending).
			// 未指定承接人的责任项不派发：不允许把责任转移给离职者本人或凭空生成承接人。
			Where("target_owner_id IS NOT NULL AND target_owner_id <> ''").
			Where("(next_retry_at IS NULL OR next_retry_at <= ?)", now.UTC()).
			Where("(lease_deadline IS NULL OR lease_deadline <= ?)", now.UTC()).
			Order("updated_at ASC, id ASC").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		update := tx.Model(&personnelHandoverItemRow{}).
			Where("id = ? AND version = ?", row.ID, row.Version).
			Updates(map[string]any{
				"lease_id": leaseID, "lease_deadline": leaseDeadline.UTC(), "attempts": row.Attempts + 1,
				"updated_at": now.UTC(), "version": gorm.Expr("version + 1"),
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return application.ErrConflict
		}
		row.LeaseID, row.LeaseDeadline, row.Attempts, row.Version = &leaseID, &leaseDeadline, row.Attempts+1, row.Version+1
		claimed = toOrchestrationHandoverItem(row)
		return nil
	})
	if err != nil {
		return application.HandoverItem{}, false, mapHandoverError(err)
	}
	return claimed, claimed.ID != "", nil
}

// RecordReceipt 幂等写入真实回执并推进交接项：
// 成功终态 → TRANSFERRED；失败终态 → FAILED；可重试 → 保留 PENDING 并安排退避；
// 版本不符表示并发变更，返回冲突交由对账处理。
func (repository *PersonnelHandoverGORMRepository) RecordReceipt(ctx context.Context, tenantID, batchID string, expectedVersion uint64, receipt application.HandoverTransferReceipt, now time.Time) (bool, error) {
	if strings.TrimSpace(receipt.ItemID) == "" || strings.TrimSpace(receipt.IdempotencyKey) == "" {
		return false, application.ErrValidation
	}
	inserted := false
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item personnelHandoverItemRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND batch_id = ? AND id = ?", tenantID, batchID, receipt.ItemID).Take(&item).Error; err != nil {
			return err
		}
		if item.IdempotencyKey == nil || *item.IdempotencyKey != receipt.IdempotencyKey || item.SystemCode != receipt.System {
			return application.ErrConflict
		}
		receiptID, err := newHandoverID(now)
		if err != nil {
			return err
		}
		var reason, evidence *string
		if strings.TrimSpace(receipt.ReasonCode) != "" {
			value := receipt.ReasonCode
			reason = &value
		}
		if strings.TrimSpace(receipt.EvidenceRef) != "" {
			value := receipt.EvidenceRef
			evidence = &value
		}
		row := personnelHandoverReceiptRow{
			ID: receiptID, TenantID: tenantID, BatchID: batchID, ItemID: receipt.ItemID, SystemCode: receipt.System,
			IdempotencyKey: receipt.IdempotencyKey, Status: receipt.Status, AppliedVersion: receipt.AppliedVersion,
			ReasonCode: reason, EvidenceRef: evidence, ReceivedAt: now.UTC(), CreatedAt: now.UTC(),
		}
		result := tx.Create(&row)
		if result.Error != nil {
			if isHandoverDuplicate(result.Error) {
				var previous personnelHandoverReceiptRow
				if err := tx.Where("tenant_id = ? AND batch_id = ? AND item_id = ? AND idempotency_key = ?", tenantID, batchID, receipt.ItemID, receipt.IdempotencyKey).Take(&previous).Error; err != nil {
					return err
				}
				if terminal, _ := application.HandoverReceiptTerminal(previous.Status); terminal {
					return nil
				}
				// 同键的非终态回执是可更新快照，不能吞掉之后的真实成功回执。
				if err := tx.Model(&personnelHandoverReceiptRow{}).Where("id = ? AND tenant_id = ?", previous.ID, tenantID).Updates(map[string]any{"status": receipt.Status, "applied_version": receipt.AppliedVersion, "reason_code": reason, "evidence_ref": evidence, "received_at": now.UTC()}).Error; err != nil {
					return err
				}
			} else {
				return result.Error
			}
		}
		inserted = true
		if item.Version != expectedVersion {
			return application.ErrConflict
		}
		updates := map[string]any{"updated_at": now.UTC(), "version": gorm.Expr("version + 1"), "lease_id": nil, "lease_deadline": nil}
		switch receipt.Status {
		case application.HandoverReceiptApplied, application.HandoverReceiptAlreadyApplied:
			updates["status"] = application.HandoverItemTransferred
			updates["applied_version"] = receipt.AppliedVersion
			updates["completed_at"] = now.UTC()
			updates["evidence_ref"] = evidence
			updates["last_error_code"] = nil
		case application.HandoverReceiptRejected:
			updates["status"] = application.HandoverItemFailed
			updates["last_error_code"] = reason
			updates["evidence_ref"] = evidence
		default: // RETRYABLE：保留待派发并按退避安排下一次
			updates["next_retry_at"] = application.NextHandoverRetryAt(int(item.Attempts), now)
			updates["last_error_code"] = reason
		}
		update := tx.Model(&personnelHandoverItemRow{}).Where("id = ? AND version = ?", item.ID, item.Version).Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return application.ErrConflict
		}
		return nil
	})
	if err != nil {
		return false, mapHandoverError(err)
	}
	return inserted, nil
}

// AssignTarget 指定责任项的承接人（管理员按资源或类别逐一选择）；已终态项不允许改派。
func (repository *PersonnelHandoverGORMRepository) AssignTarget(ctx context.Context, tenantID, itemID, targetUserID string, now time.Time) error {
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return application.ErrValidation
	}
	return mapHandoverError(repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item personnelHandoverItemRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, itemID).Take(&item).Error; err != nil {
			return err
		}
		if item.BatchID == "" || item.CurrentOwnerID == targetUserID || item.Status != application.HandoverItemPending {
			return application.ErrConflict
		}
		if item.TargetOwnerID != nil && *item.TargetOwnerID == targetUserID {
			return nil
		}
		// 派发过的键绑定既定意图，不能在结果未知时换接收人或改租约版本。
		if item.Attempts > 0 || (item.LeaseDeadline != nil && item.LeaseDeadline.After(now)) {
			return application.ErrConflict
		}
		var active int64
		if err := tx.Table("iam_user").Where("tenant_id = ? AND id = ? AND status = 'ACTIVE' AND deleted_at IS NULL", tenantID, targetUserID).Count(&active).Error; err != nil {
			return err
		}
		if active != 1 {
			return application.ErrValidation
		}
		return tx.Model(&personnelHandoverItemRow{}).Where("tenant_id = ? AND id = ? AND version = ?", tenantID, itemID, item.Version).Updates(map[string]any{"target_owner_id": targetUserID, "updated_at": now.UTC(), "version": gorm.Expr("version + 1")}).Error
	}))
}

func (repository *PersonnelHandoverGORMRepository) RequestCancellation(ctx context.Context, tenantID, batchID string, now time.Time) error {
	return mapHandoverError(repository.db.WithContext(ctx).Model(&personnelHandoverBatchRow{}).Where("tenant_id = ? AND id = ? AND cancel_requested_at IS NULL", tenantID, batchID).Updates(map[string]any{"cancel_requested_at": now.UTC(), "updated_at": now.UTC(), "version": gorm.Expr("version + 1")}).Error)
}

// CompleteBatch 推进批次终态（COMPLETED / CANCELLED / BLOCKED）。
func (repository *PersonnelHandoverGORMRepository) CompleteBatch(ctx context.Context, tenantID, batchID, status string, now time.Time) error {
	if strings.TrimSpace(status) == "" {
		return application.ErrValidation
	}
	updates := map[string]any{"status": status, "completed_at": nil, "updated_at": now.UTC(), "version": gorm.Expr("version + 1")}
	if status == application.HandoverBatchCompleted || status == application.HandoverBatchCancelled {
		updates["completed_at"] = now.UTC()
	}
	query := repository.db.WithContext(ctx).Model(&personnelHandoverBatchRow{}).Where("tenant_id = ? AND id = ?", tenantID, batchID)
	if status != application.HandoverBatchCancelled {
		query = query.Where("cancel_requested_at IS NULL AND status NOT IN ?", []string{application.HandoverBatchCancelled, application.HandoverBatchCompleted})
	}
	update := query.Updates(updates)
	if update.Error != nil {
		return mapHandoverError(update.Error)
	}
	// 同上：终态重复推进（例如重复取消）必须保持幂等，而不是报"记录不存在"。
	if update.RowsAffected == 0 {
		var count int64
		if err := repository.db.WithContext(ctx).Model(&personnelHandoverBatchRow{}).
			Where("tenant_id = ? AND id = ?", tenantID, batchID).Count(&count).Error; err != nil {
			return mapHandoverError(err)
		}
		if count == 0 {
			return application.ErrNotFound
		}
		var existing personnelHandoverBatchRow
		if err := repository.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, batchID).Take(&existing).Error; err != nil {
			return mapHandoverError(err)
		}
		if existing.Status != status {
			return application.ErrConflict
		}
	}
	return nil
}

// ListSystemStates 返回批次内各系统的适用性与冻结状态，按系统编码稳定排序供编排器复核。
func (repository *PersonnelHandoverGORMRepository) ListSystemStates(ctx context.Context, tenantID, batchID string) ([]application.HandoverSystemState, error) {
	var rows []personnelHandoverSystemRow
	if err := repository.db.WithContext(ctx).Where("tenant_id = ? AND batch_id = ?", tenantID, batchID).Order("system_code").Find(&rows).Error; err != nil {
		return nil, mapHandoverError(err)
	}
	out := make([]application.HandoverSystemState, 0, len(rows))
	for _, row := range rows {
		out = append(out, application.HandoverSystemState{System: row.SystemCode, Capability: row.Capability, Frozen: row.Frozen, FrozenAt: row.FrozenAt})
	}
	return out, nil
}

// ListPendingCompletionBatches 将未结束批次与已提交的离职请求关联作为持久待办。
// 同时限定租户、请求与人员，取消/排期/失败执行和历史终态均不会进入完成队列。
func (repository *PersonnelHandoverGORMRepository) ListPendingCompletionBatches(ctx context.Context, afterID string, limit int) ([]application.HandoverBatch, error) {
	if limit <= 0 || limit > 100 {
		return nil, application.ErrValidation
	}
	var rows []personnelHandoverBatchRow
	err := repository.db.WithContext(ctx).Table("iam_personnel_handover_batch AS b").Select("b.*").
		Joins("JOIN iam_personnel_change_request AS r ON r.tenant_id = b.tenant_id AND r.id = b.request_id AND r.user_id = b.user_id").
		Where("b.status IN ? AND b.cancel_requested_at IS NULL AND b.id > ?", []string{application.HandoverBatchFrozen, application.HandoverBatchTransferring}, afterID).
		Where("r.status = ? AND r.change_type = ?", domain.PersonnelChangeExecuted, domain.PersonnelChangeTermination).
		Order("b.id ASC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, mapHandoverError(err)
	}
	out := make([]application.HandoverBatch, 0, len(rows))
	for _, row := range rows {
		out = append(out, toHandoverBatch(row))
	}
	return out, nil
}

// ListStaleBatches 有界列出超过截止时间仍未推进的交接批次（按租户），供历史离职重新发现使用。
func (repository *PersonnelHandoverGORMRepository) ListStaleBatches(ctx context.Context, tenantID string, olderThan time.Time, limit int) ([]application.HandoverBatch, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []personnelHandoverBatchRow
	if err := repository.db.WithContext(ctx).
		Where("tenant_id = ? AND status IN ? AND updated_at < ?", tenantID,
			[]string{application.HandoverBatchDiscovering, application.HandoverBatchFrozen, application.HandoverBatchTransferring, application.HandoverBatchBlocked}, olderThan.UTC()).
		Order("updated_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, mapHandoverError(err)
	}
	out := make([]application.HandoverBatch, 0, len(rows))
	for _, row := range rows {
		out = append(out, toHandoverBatch(row))
	}
	return out, nil
}

// ListItems 返回请求下全部交接责任项（含已完成），按系统、资源类型与资源 ID 稳定排序，
// 供前端明细与对账使用。
func (repository *PersonnelHandoverGORMRepository) ListItems(ctx context.Context, tenantID, requestID string) ([]application.HandoverItem, error) {
	var rows []personnelHandoverItemRow
	if err := repository.db.WithContext(ctx).Where("tenant_id = ? AND request_id = ?", tenantID, requestID).
		Order("system_code, resource_type, resource_id").Find(&rows).Error; err != nil {
		return nil, mapHandoverError(err)
	}
	out := make([]application.HandoverItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, toOrchestrationHandoverItem(row))
	}
	return out, nil
}

func toHandoverBatch(row personnelHandoverBatchRow) application.HandoverBatch {
	return application.HandoverBatch{
		ID: row.ID, TenantID: row.TenantID, RequestID: row.RequestID, UserID: row.UserID, Status: row.Status,
		Version: row.Version, CancelRequestedAt: row.CancelRequestedAt, CompletedAt: row.CompletedAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func toOrchestrationHandoverItem(row personnelHandoverItemRow) application.HandoverItem {
	target := ""
	if row.TargetOwnerID != nil {
		target = *row.TargetOwnerID
	}
	item := application.HandoverItem{
		ID: row.ID, System: row.SystemCode, ResourceType: row.ResourceType, ResourceID: row.ResourceID,
		ResourceName: row.ResourceName, Category: row.Category, CurrentOwnerID: row.CurrentOwnerID,
		TargetOwnerID: target, Status: row.Status, BatchID: row.BatchID,
		ExpectedVersion: row.ExpectedVersion, AppliedVersion: row.AppliedVersion,
		LeaseDeadline: row.LeaseDeadline, Attempts: row.Attempts, NextRetryAt: row.NextRetryAt,
		Version: row.Version, CompletedAt: row.CompletedAt,
	}
	if row.IdempotencyKey != nil {
		item.IdempotencyKey = *row.IdempotencyKey
	}
	if row.LeaseID != nil {
		item.LeaseID = *row.LeaseID
	}
	if row.LastErrorCode != nil {
		item.LastErrorCode = *row.LastErrorCode
	}
	if row.EvidenceRef != nil {
		item.EvidenceRef = *row.EvidenceRef
	}
	if row.CompletedBy != nil {
		item.CompletedBy = *row.CompletedBy
	}
	return item
}
