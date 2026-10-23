// Package domain 定义租户级业务字典的领域聚合：字典把业务枚举值归拢在稳定
// 编码之下，字典项是业务数据实际保存的可选值。本包不含行为，仅承载状态与约束。
package domain

import "time"

// Status 控制字典或字典项是否对运行时业务接口可见。停用是删除的可逆替代：
// 已发布到业务的取值应停用而非删除，以保留历史数据的可解释性。
type Status string

const (
	// StatusActive 启用：出现在按编码取启用项的业务接口结果中。
	StatusActive Status = "ACTIVE"
	// StatusDisabled 停用：业务接口不返回，管理界面仍可见以便审计与恢复。
	StatusDisabled Status = "DISABLED"
)

// Dictionary 把一组业务枚举值归拢在租户内唯一的稳定编码之下。编码是业务契约：
// 业务表单按编码消费启用项，重命名编码即破坏历史数据语义。
type Dictionary struct {
	ID          string
	TenantID    string
	Code        string
	Name        string
	Description string
	Status      Status
	ItemCount   int64
	Version     uint64
	UpdatedAt   time.Time
}

// Item 是一个可选的字典值。code 为管理标识，value 才是业务实际保存的值；
// 两者分离允许治理显示名（label）而不触碰存量业务数据。
type Item struct {
	ID           string
	TenantID     string
	DictionaryID string
	Code         string
	Label        string
	Value        string
	SortOrder    uint
	Status       Status
	Version      uint64
	UpdatedAt    time.Time
}
