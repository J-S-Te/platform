// Package securityevent 保存无法归属可信租户的全局安全事件（认证失败等）。
// 它与租户审计表物理隔离，避免匿名事件污染租户查询与哈希链。
package securityevent

import "time"

type Event struct {
	ID              uint64    `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	EventID         string    `gorm:"column:event_id" json:"event_id"`
	TenantID        string    `gorm:"column:tenant_id" json:"tenant_id,omitempty"`
	ApplicationID   string    `gorm:"column:application_id" json:"application_id,omitempty"`
	ApplicationCode string    `gorm:"column:application_code" json:"application_code"`
	EnvironmentID   string    `gorm:"column:environment_id" json:"environment_id,omitempty"`
	EnvironmentCode string    `gorm:"column:environment_code" json:"environment_code"`
	OccurredAt      time.Time `gorm:"column:occurred_at" json:"occurred_at"`
	ReceivedAt      time.Time `gorm:"column:received_at" json:"received_at"`
	ActorType       string    `gorm:"column:actor_type" json:"actor_type"`
	ActorID         string    `gorm:"column:actor_id" json:"actor_id,omitempty"`
	ActorName       string    `gorm:"column:actor_name" json:"actor_name,omitempty"`
	Action          string    `gorm:"column:action" json:"action"`
	Method          string    `gorm:"column:method" json:"method"`
	Path            string    `gorm:"column:path" json:"path"`
	ClientIP        string    `gorm:"column:client_ip" json:"client_ip,omitempty"`
	UserAgent       string    `gorm:"column:user_agent" json:"user_agent,omitempty"`
	RequestID       string    `gorm:"column:request_id" json:"request_id,omitempty"`
	Result          string    `gorm:"column:result" json:"result"`
	ReasonCode      string    `gorm:"column:reason_code" json:"reason_code,omitempty"`
	RiskLevel       string    `gorm:"column:risk_level" json:"risk_level"`
	Summary         string    `gorm:"column:summary" json:"summary,omitempty"`
}

// TableName 固定表名；仓储不使用 AutoMigrate，迁移由 migrations 目录统一管理。
func (Event) TableName() string { return "platform_security_event" }
