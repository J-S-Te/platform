package infrastructure

import "time"

// subsystemAccessRecoveryAuditModel 是"原代次已被取代"人工收口的追加式审计行。
//
// 该表只插入：不更新、不删除。它记录操作者实际审核到的审核基准（原回执清单摘要与
// 当前清单摘要）以及收口前的部署状态快照，供事后说明"为什么可以判定原代次没有成功、
// 也没有终止"，同时保留租户/应用/环境隔离与操作者、原因。
type subsystemAccessRecoveryAuditModel struct {
	ID                      uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID                string    `gorm:"column:tenant_id"`
	ApplicationCode         string    `gorm:"column:application_code"`
	EnvironmentCode         string    `gorm:"column:environment_code"`
	OperationID             string    `gorm:"column:operation_id"`
	Generation              uint64    `gorm:"column:generation"`
	ReceiptManifestChecksum string    `gorm:"column:receipt_manifest_checksum"`
	CurrentProfileChecksum  string    `gorm:"column:current_profile_checksum"`
	StateStatus             string    `gorm:"column:state_status"`
	StateGeneration         uint64    `gorm:"column:state_generation"`
	StateDesiredManifest    string    `gorm:"column:state_desired_manifest_checksum"`
	StateAppliedManifest    string    `gorm:"column:state_applied_manifest_checksum"`
	OperatorID              string    `gorm:"column:operator_id"`
	Reason                  string    `gorm:"column:reason"`
	CreatedAt               time.Time `gorm:"column:created_at"`
}

// TableName 指定追加式审计表名。
func (subsystemAccessRecoveryAuditModel) TableName() string {
	return "subsystem_access_recovery_audit"
}
