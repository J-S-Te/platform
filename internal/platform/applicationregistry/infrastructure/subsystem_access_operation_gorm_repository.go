package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 统一接入向导的操作/阶段仓储。并发语义要点：
//   - active_slot 仅在 PENDING/RUNNING/WAITING 时写入 → 唯一索引保证同目标只有一个执行中操作；
//   - 领取用条件更新（状态 + 租约过期），续租与收口要求 lease_owner 匹配 → 过期执行者不能覆盖新结果；
//   - 所有写入都带 tenant_id 条件，跨租户读取或被拒。
type SubsystemAccessOperationGORMRepository struct {
	database *gorm.DB
}

type subsystemAccessOperationModel struct {
	ID              string     `gorm:"column:id;primaryKey"`
	TenantID        string     `gorm:"column:tenant_id"`
	ApplicationID   *string    `gorm:"column:application_id"`
	EnvironmentID   *string    `gorm:"column:environment_id"`
	ApplicationCode string     `gorm:"column:application_code"`
	EnvironmentCode string     `gorm:"column:environment_code"`
	Action          string     `gorm:"column:action"`
	Stage           string     `gorm:"column:stage"`
	Status          string     `gorm:"column:status"`
	PreviewVersion  string     `gorm:"column:preview_version"`
	RequestDigest   string     `gorm:"column:request_digest"`
	PublicOrigin    string     `gorm:"column:public_origin"`
	AuthProvider    string     `gorm:"column:auth_provider"`
	InitialAdminID  *string    `gorm:"column:initial_admin_id"`
	IdempotencyKey  string     `gorm:"column:idempotency_key"`
	OperatorID      string     `gorm:"column:operator_id"`
	LeaseOwner      *string    `gorm:"column:lease_owner"`
	LeaseExpiresAt  *time.Time `gorm:"column:lease_expires_at"`
	AttemptCount    uint32     `gorm:"column:attempt_count"`
	ErrorCode       *string    `gorm:"column:error_code"`
	ErrorMessage    *string    `gorm:"column:error_message"`
	ResultRef       *string    `gorm:"column:result_ref"`
	ActiveSlot      *string    `gorm:"column:active_slot"`
	Version         uint64     `gorm:"column:version"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName 指定 GORM 行映射的操作表名。
func (subsystemAccessOperationModel) TableName() string { return "subsystem_access_operation" }

type subsystemAccessOperationStageModel struct {
	ID           string     `gorm:"column:id;primaryKey"`
	TenantID     string     `gorm:"column:tenant_id"`
	OperationID  string     `gorm:"column:operation_id"`
	Stage        string     `gorm:"column:stage"`
	Status       string     `gorm:"column:status"`
	AttemptCount uint32     `gorm:"column:attempt_count"`
	ErrorCode    *string    `gorm:"column:error_code"`
	ErrorMessage *string    `gorm:"column:error_message"`
	EvidenceRef  *string    `gorm:"column:evidence_ref"`
	StartedAt    *time.Time `gorm:"column:started_at"`
	FinishedAt   *time.Time `gorm:"column:finished_at"`
	Version      uint64     `gorm:"column:version"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// TableName 指定 GORM 行映射的阶段表名。
func (subsystemAccessOperationStageModel) TableName() string {
	return "subsystem_access_operation_stage"
}

// NewSubsystemAccessOperationGORMRepository 构造操作仓储。
func NewSubsystemAccessOperationGORMRepository(database *gorm.DB) (*SubsystemAccessOperationGORMRepository, error) {
	if database == nil {
		return nil, errors.New("subsystem access operation repository requires a database")
	}
	return &SubsystemAccessOperationGORMRepository{database: database}, nil
}

// CreateOperation 幂等创建：同键同摘要返回既有操作；同键不同摘要或同目标已有执行中操作返回冲突。
func (repository *SubsystemAccessOperationGORMRepository) CreateOperation(ctx context.Context, create application.SubsystemAccessOperationCreate) (application.SubsystemAccessOperation, bool, error) {
	tenantID := strings.TrimSpace(create.TenantID)
	applicationCode := strings.TrimSpace(create.ApplicationCode)
	environmentCode := strings.ToLower(strings.TrimSpace(create.EnvironmentCode))
	idempotencyKey := strings.TrimSpace(create.IdempotencyKey)
	if tenantID == "" || applicationCode == "" || environmentCode == "" || idempotencyKey == "" {
		return application.SubsystemAccessOperation{}, false, fmt.Errorf("subsystem access operation requires tenant, target and idempotency key: %w", application.ErrSubsystemAccessInvalidRequest)
	}
	slotKey, err := application.ActiveSlotKey(tenantID, applicationCode, environmentCode)
	if err != nil {
		return application.SubsystemAccessOperation{}, false, err
	}
	status := application.SubsystemAccessStatusPending
	slot := (*string)(nil)
	if status.HoldsActiveSlot() {
		slot = &slotKey
	}
	now := create.Now.UTC()
	model := subsystemAccessOperationModel{
		ID: create.ID, TenantID: tenantID, ApplicationID: accessOptionalString(create.ApplicationID), EnvironmentID: accessOptionalString(create.EnvironmentID),
		ApplicationCode: applicationCode, EnvironmentCode: environmentCode,
		Action: string(create.Action), Stage: string(create.Stage), Status: string(status),
		PreviewVersion: strings.TrimSpace(create.PreviewVersion), RequestDigest: strings.TrimSpace(create.RequestDigest),
		PublicOrigin: strings.TrimSpace(create.PublicOrigin), AuthProvider: strings.TrimSpace(create.AuthProvider),
		InitialAdminID: accessOptionalString(create.InitialAdminID),
		IdempotencyKey: idempotencyKey, OperatorID: strings.TrimSpace(create.OperatorID),
		ActiveSlot: slot, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
		if !isSubsystemAccessDuplicateKey(err) {
			return application.SubsystemAccessOperation{}, false, err
		}
		// 唯一键冲突：先按幂等键定位既有操作。
		existing, found, lookupErr := repository.findByKey(ctx, tenantID, idempotencyKey)
		if lookupErr != nil {
			return application.SubsystemAccessOperation{}, false, lookupErr
		}
		if !found {
			// 键不存在 → 冲突来自 active_slot：同目标已有执行中操作。
			return application.SubsystemAccessOperation{}, false, application.ErrSubsystemAccessOperationConflict
		}
		if existing.RequestDigest != strings.TrimSpace(create.RequestDigest) {
			return application.SubsystemAccessOperation{}, false, application.ErrSubsystemAccessOperationConflict
		}
		return existing, false, nil
	}
	return toSubsystemAccessOperation(model), true, nil
}

// GetOperation 按租户 + 操作 ID 读取；跨租户视为不存在。
func (repository *SubsystemAccessOperationGORMRepository) GetOperation(ctx context.Context, tenantID, id string) (application.SubsystemAccessOperation, error) {
	var model subsystemAccessOperationModel
	err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", strings.TrimSpace(tenantID), strings.TrimSpace(id)).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.SubsystemAccessOperation{}, application.ErrSubsystemAccessOperationNotFound
	}
	if err != nil {
		return application.SubsystemAccessOperation{}, err
	}
	return toSubsystemAccessOperation(model), nil
}

// FindOperationByTarget 返回该目标最近一次操作（供页面刷新/CLI 重连定位）。
func (repository *SubsystemAccessOperationGORMRepository) FindOperationByTarget(ctx context.Context, tenantID, applicationCode, environmentCode string) (application.SubsystemAccessOperation, bool, error) {
	var model subsystemAccessOperationModel
	err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environmentCode))).
		Order("created_at DESC, id DESC").Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.SubsystemAccessOperation{}, false, nil
	}
	if err != nil {
		return application.SubsystemAccessOperation{}, false, err
	}
	return toSubsystemAccessOperation(model), true, nil
}

// ListOperationsByTarget 返回该目标的操作历史（有界）。
func (repository *SubsystemAccessOperationGORMRepository) ListOperationsByTarget(ctx context.Context, tenantID, applicationCode, environmentCode string, limit int) ([]application.SubsystemAccessOperation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var models []subsystemAccessOperationModel
	if err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environmentCode))).
		Order("created_at DESC, id DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, err
	}
	operations := make([]application.SubsystemAccessOperation, 0, len(models))
	for _, model := range models {
		operations = append(operations, toSubsystemAccessOperation(model))
	}
	return operations, nil
}

// ClaimOperation 领取：条件更新（状态可领取 + 租约为空或已过期），过期执行者无法领取在职租约。
func (repository *SubsystemAccessOperationGORMRepository) ClaimOperation(ctx context.Context, tenantID, id, workerID string, leaseTTL time.Duration, now time.Time) (application.SubsystemAccessOperation, bool, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return application.SubsystemAccessOperation{}, false, fmt.Errorf("claim requires a worker identity: %w", application.ErrSubsystemAccessInvalidRequest)
	}
	now = now.UTC()
	if leaseTTL <= 0 {
		leaseTTL = 2 * time.Minute
	}
	expires := now.Add(leaseTTL)
	result := repository.database.WithContext(ctx).Model(&subsystemAccessOperationModel{}).
		Where("tenant_id = ? AND id = ?", strings.TrimSpace(tenantID), strings.TrimSpace(id)).
		Where("status IN ?", []string{string(application.SubsystemAccessStatusPending), string(application.SubsystemAccessStatusRunning), string(application.SubsystemAccessStatusWaiting)}).
		Where("lease_expires_at IS NULL OR lease_expires_at < ?", now).
		Updates(map[string]any{
			"status":           string(application.SubsystemAccessStatusRunning),
			"lease_owner":      workerID,
			"lease_expires_at": expires,
			"attempt_count":    gorm.Expr("attempt_count + 1"),
			"version":          gorm.Expr("version + 1"),
			"updated_at":       now,
		})
	if result.Error != nil {
		return application.SubsystemAccessOperation{}, false, result.Error
	}
	if result.RowsAffected != 1 {
		return application.SubsystemAccessOperation{}, false, nil
	}
	operation, err := repository.GetOperation(ctx, tenantID, id)
	if err != nil {
		return application.SubsystemAccessOperation{}, false, err
	}
	return operation, true, nil
}

// RenewOperationLease 续租：仅当租约仍属于该执行者。
func (repository *SubsystemAccessOperationGORMRepository) RenewOperationLease(ctx context.Context, tenantID, id, workerID string, leaseTTL time.Duration, now time.Time) (bool, error) {
	if leaseTTL <= 0 {
		leaseTTL = 2 * time.Minute
	}
	now = now.UTC()
	result := repository.database.WithContext(ctx).Model(&subsystemAccessOperationModel{}).
		Where("tenant_id = ? AND id = ? AND lease_owner = ? AND status = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(id), strings.TrimSpace(workerID), string(application.SubsystemAccessStatusRunning)).
		Updates(map[string]any{"lease_expires_at": now.Add(leaseTTL), "version": gorm.Expr("version + 1"), "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompleteOperation 收口：要求租约仍属于该执行者（过期执行者返回 ErrLeaseLost）。
// 终态释放 active_slot；WAITING 保留槽位供 resume 继续。
func (repository *SubsystemAccessOperationGORMRepository) CompleteOperation(ctx context.Context, completion application.SubsystemAccessOperationCompletion) (bool, error) {
	now := completion.Now.UTC()
	updates := map[string]any{
		"status":        string(completion.Status),
		"stage":         string(completion.Stage),
		"error_code":    accessOptionalString(completion.ErrorCode),
		"error_message": accessOptionalString(completion.ErrorMessage),
		"result_ref":    accessOptionalString(completion.ResultRef),
		"lease_owner":   nil,
		"version":       gorm.Expr("version + 1"),
		"updated_at":    now,
	}
	if completion.Status.Terminal() {
		updates["lease_expires_at"] = nil
		updates["active_slot"] = nil
	} else {
		updates["lease_expires_at"] = now
	}
	result := repository.database.WithContext(ctx).Model(&subsystemAccessOperationModel{}).
		Where("tenant_id = ? AND id = ? AND lease_owner = ?", strings.TrimSpace(completion.TenantID), strings.TrimSpace(completion.ID), strings.TrimSpace(completion.LeaseOwner)).
		Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		return true, nil
	}
	// 未命中：区分"租约丢失"与"操作不存在/已收口"。
	if _, err := repository.GetOperation(ctx, completion.TenantID, completion.ID); err != nil {
		return false, err
	}
	return false, application.ErrSubsystemAccessOperationLeaseLost
}

// ListClaimableOperations 列出可领取的操作：待处理，或运行中但租约已过期（过期执行者不得继续持有）。
func (repository *SubsystemAccessOperationGORMRepository) ListClaimableOperations(ctx context.Context, limit int, now time.Time) ([]application.SubsystemAccessOperation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var models []subsystemAccessOperationModel
	if err := repository.database.WithContext(ctx).
		Where("status = ?", string(application.SubsystemAccessStatusPending)).
		Or("status = ? AND (lease_expires_at IS NULL OR lease_expires_at < ?)", string(application.SubsystemAccessStatusRunning), now.UTC()).
		Order("updated_at ASC, id ASC").Limit(limit).Find(&models).Error; err != nil {
		return nil, err
	}
	operations := make([]application.SubsystemAccessOperation, 0, len(models))
	for _, model := range models {
		operations = append(operations, toSubsystemAccessOperation(model))
	}
	return operations, nil
}

// AdvanceOperationStage 推进阶段：要求租约仍属于该执行者且状态为 RUNNING。
func (repository *SubsystemAccessOperationGORMRepository) AdvanceOperationStage(ctx context.Context, tenantID, id, leaseOwner string, stage application.SubsystemAccessStage, now time.Time) (bool, error) {
	result := repository.database.WithContext(ctx).Model(&subsystemAccessOperationModel{}).
		Where("tenant_id = ? AND id = ? AND lease_owner = ? AND status = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(id), strings.TrimSpace(leaseOwner), string(application.SubsystemAccessStatusRunning)).
		Updates(map[string]any{"stage": string(stage), "version": gorm.Expr("version + 1"), "updated_at": now.UTC()})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// RequeueOperation 用途：原子恢复等待或失败操作，清单核验失败时重新执行部署。
// 参数：ctx 为数据库上下文，tenantID 与 id 为授权操作身份，now 为恢复时间。
// 返回值：成功重占槽位返回 true；重复调用或目标被占用返回 false，存储失败返回错误。
func (repository *SubsystemAccessOperationGORMRepository) RequeueOperation(ctx context.Context, tenantID, id string, now time.Time) (bool, error) {
	reacquired := false
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row subsystemAccessOperationModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", strings.TrimSpace(tenantID), strings.TrimSpace(id)).
			Take(&row).Error; err != nil {
			return err
		}
		if row.Status != string(application.SubsystemAccessStatusWaiting) &&
			row.Status != string(application.SubsystemAccessStatusFailed) {
			return nil
		}
		slotKey, err := application.ActiveSlotKey(row.TenantID, row.ApplicationCode, row.EnvironmentCode)
		if err != nil {
			return err
		}
		if row.ActiveSlot == nil {
			var held int64
			if err := tx.Model(&subsystemAccessOperationModel{}).
				Where("active_slot = ? AND id <> ?", slotKey, row.ID).Count(&held).Error; err != nil {
				return err
			}
			if held > 0 {
				return nil
			}
		}
		result := tx.Model(&subsystemAccessOperationModel{}).
			Where("tenant_id = ? AND id = ? AND status = ? AND version = ?", row.TenantID, row.ID, row.Status, row.Version).
			Updates(map[string]any{
				"status": string(application.SubsystemAccessStatusPending),
				"stage":  subsystemAccessRecoveryStage(row), "active_slot": slotKey,
				"lease_owner": nil, "lease_expires_at": nil, "error_code": nil,
				"error_message": nil, "result_ref": nil,
				"version": gorm.Expr("version + 1"), "updated_at": now.UTC(),
			})
		reacquired = result.Error == nil && result.RowsAffected == 1
		return result.Error
	})
	return reacquired, err
}

// subsystemAccessRecoveryStage 用途：把需要重新部署的核验失败恢复到部署阶段。
// 参数：row 为已锁定且允许恢复的操作；其它失败保留原阶段。
// 返回值：返回本次恢复应执行的阶段，不重跑目录或认证登记。
func subsystemAccessRecoveryStage(row subsystemAccessOperationModel) string {
	if row.Stage == string(application.SubsystemAccessStageReadiness) && row.ErrorCode != nil {
		switch *row.ErrorCode {
		case "MANIFEST_MISMATCH", "DEPLOYMENT_FAILED":
			return string(application.SubsystemAccessStageDeploy)
		}
	}
	return row.Stage
}

// UpsertOperationStage 记录阶段结果（同操作同阶段唯一）。
func (repository *SubsystemAccessOperationGORMRepository) UpsertOperationStage(ctx context.Context, tenantID, operationID string, stage application.SubsystemAccessStage, status application.SubsystemAccessStatus, errorCode, errorMessage, evidenceRef string, now time.Time) error {
	now = now.UTC()
	var startedAt, finishedAt *time.Time
	if status == application.SubsystemAccessStatusRunning {
		startedAt = &now
	}
	if status.Terminal() {
		finishedAt = &now
	}
	model := subsystemAccessOperationStageModel{
		ID: stageID(tenantID, operationID, stage), TenantID: strings.TrimSpace(tenantID), OperationID: strings.TrimSpace(operationID),
		Stage: string(stage), Status: string(status), ErrorCode: accessOptionalString(errorCode), ErrorMessage: accessOptionalString(errorMessage),
		EvidenceRef: accessOptionalString(evidenceRef), StartedAt: startedAt, FinishedAt: finishedAt, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	return repository.database.WithContext(ctx).Exec(
		`INSERT INTO subsystem_access_operation_stage
		   (id, tenant_id, operation_id, stage, status, attempt_count, error_code, error_message, evidence_ref, started_at, finished_at, version, created_at, updated_at)
		 VALUES (?,?,?,?,?,1,?,?,?,?,?,1,?,?)
		 ON DUPLICATE KEY UPDATE
		   status = VALUES(status), attempt_count = attempt_count + 1,
		   error_code = VALUES(error_code), error_message = VALUES(error_message), evidence_ref = VALUES(evidence_ref),
		   finished_at = VALUES(finished_at), version = version + 1, updated_at = VALUES(updated_at)`,
		model.ID, model.TenantID, model.OperationID, model.Stage, model.Status, model.ErrorCode, model.ErrorMessage,
		model.EvidenceRef, model.StartedAt, model.FinishedAt, model.CreatedAt, model.UpdatedAt).Error
}

// ListOperationStages 按固定阶段顺序返回阶段记录。
func (repository *SubsystemAccessOperationGORMRepository) ListOperationStages(ctx context.Context, tenantID, operationID string) ([]application.SubsystemAccessOperationStage, error) {
	var models []subsystemAccessOperationStageModel
	if err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND operation_id = ?", strings.TrimSpace(tenantID), strings.TrimSpace(operationID)).
		Order("FIELD(stage,'CHECK','DIRECTORY','AUTH','DEPLOY','READINESS','RESULT')").Find(&models).Error; err != nil {
		return nil, err
	}
	stages := make([]application.SubsystemAccessOperationStage, 0, len(models))
	for _, model := range models {
		stages = append(stages, application.SubsystemAccessOperationStage{
			ID: model.ID, TenantID: model.TenantID, OperationID: model.OperationID,
			Stage: application.SubsystemAccessStage(model.Stage), Status: application.SubsystemAccessStatus(model.Status),
			AttemptCount: model.AttemptCount, ErrorCode: accessDerefString(model.ErrorCode), ErrorMessage: accessDerefString(model.ErrorMessage),
			EvidenceRef: accessDerefString(model.EvidenceRef), StartedAt: model.StartedAt, FinishedAt: model.FinishedAt, UpdatedAt: model.UpdatedAt,
		})
	}
	return stages, nil
}

func (repository *SubsystemAccessOperationGORMRepository) findByKey(ctx context.Context, tenantID, idempotencyKey string) (application.SubsystemAccessOperation, bool, error) {
	var model subsystemAccessOperationModel
	err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND idempotency_key = ?", tenantID, idempotencyKey).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.SubsystemAccessOperation{}, false, nil
	}
	if err != nil {
		return application.SubsystemAccessOperation{}, false, err
	}
	return toSubsystemAccessOperation(model), true, nil
}

func toSubsystemAccessOperation(model subsystemAccessOperationModel) application.SubsystemAccessOperation {
	return application.SubsystemAccessOperation{
		ID: model.ID, TenantID: model.TenantID, ApplicationID: accessDerefString(model.ApplicationID), EnvironmentID: accessDerefString(model.EnvironmentID),
		ApplicationCode: model.ApplicationCode, EnvironmentCode: model.EnvironmentCode,
		Action: application.SubsystemAccessAction(model.Action), Stage: application.SubsystemAccessStage(model.Stage), Status: application.SubsystemAccessStatus(model.Status),
		PreviewVersion: model.PreviewVersion, RequestDigest: model.RequestDigest, IdempotencyKey: model.IdempotencyKey, OperatorID: model.OperatorID,
		PublicOrigin: model.PublicOrigin, AuthProvider: model.AuthProvider, InitialAdminID: accessDerefString(model.InitialAdminID),
		LeaseOwner: accessDerefString(model.LeaseOwner), LeaseExpiresAt: model.LeaseExpiresAt, AttemptCount: model.AttemptCount,
		ErrorCode: accessDerefString(model.ErrorCode), ErrorMessage: accessDerefString(model.ErrorMessage), ResultRef: accessDerefString(model.ResultRef),
		Version: model.Version, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

// stageID 用确定性 ID 保证 (tenant, operation, stage) 唯一，重复写入走 ON DUPLICATE KEY。
func stageID(tenantID, operationID string, stage application.SubsystemAccessStage) string {
	seed := strings.TrimSpace(tenantID) + ":" + strings.TrimSpace(operationID) + ":" + string(stage)
	sum := 0
	for _, r := range seed {
		sum = (sum*131 + int(r)) % 1000000000000000000
	}
	return fmt.Sprintf("01STG%021d", sum)
}

// accessOptionalString 把空串转成 nil，便于写入可空列。
func accessOptionalString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// accessDerefString 读取可空列。
func accessDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// isSubsystemAccessDuplicateKey 识别 MySQL 唯一键冲突（1062）。
func isSubsystemAccessDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate") || strings.Contains(message, "1062")
}
