// Package domain 定义商业授权安装状态的领域模型与错误语义。
package domain

import (
	"errors"
	core "github.com/J-S-Te/license-core"
	"time"
)

var (
	// ErrNotInitialized 表示部署尚未执行受控初始化。
	ErrNotInitialized = errors.New("license deployment is not initialized")
	// ErrConflict 只表示瞬时的并发冲突（MySQL 重复键/死锁/等锁超时，或预览与提交之间状态被推进），重试有效。
	// 确定性失败必须使用下面的专有错误，否则会把“刷新重试即可”的提示用在重试无效的场景上。
	ErrConflict = errors.New("license state revision conflict")
	// ErrVersionNotIncreasing 表示许可证版本未高于当前生效版本，续签基准错误。
	ErrVersionNotIncreasing = errors.New("license version is not higher than the effective version")
	// ErrBindingMismatch 表示许可证绑定的客户或环境与部署不一致。
	ErrBindingMismatch = errors.New("license deployment is bound to another customer or environment")
	// ErrConfirmation 表示缺少 Preview 要求的显式变更确认。
	ErrConfirmation = errors.New("explicit license change confirmation is required")
	// ErrClock 表示检测到时钟回拨，必须使用签名恢复凭证解锁。
	ErrClock = errors.New("license clock rollback requires signed recovery")
	// ErrCorrupt 表示持久化状态未通过验签或一致性校验，属严重异常。
	ErrCorrupt = errors.New("persisted license state failed verification")
	// ErrInvalid 表示管理输入非法。
	ErrInvalid = errors.New("invalid license management input")
	// ErrTrustNotConfigured 表示厂商信任密钥未配置，验签失败关闭。
	ErrTrustNotConfigured = errors.New("LICENSE_TRUST_NOT_CONFIGURED")
)

// Actor 记录触发授权状态变更的操作者。
type Actor struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
}

// Deployment 是单实例部署的授权状态聚合：当前/挂起摘要、最高版本、时钟闩锁。
type Deployment struct {
	ID                uint64 `gorm:"primaryKey"`
	InstanceID        string
	CustomerID        string
	Environment       string
	Revision          uint64
	CurrentDigest     string
	PendingDigest     string
	HighestVersion    uint64
	HighestObservedAt int64
	ClockBlocked      bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TableName 指定部署状态表名。
func (Deployment) TableName() string { return "license_deployment" }

// Artifact 是按内容摘要寻址的许可证原文存储；原文绝不出现在响应中。
type Artifact struct {
	Digest    string `gorm:"primaryKey;size:64"`
	RawJWS    string `gorm:"column:raw_jws;type:mediumtext" json:"-"`
	Version   uint64
	CreatedAt time.Time
}

// TableName 指定许可证产物表名。
func (Artifact) TableName() string { return "license_artifact" }

// Event 是授权状态机的不可变审计事件。
type Event struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	Kind      string    `json:"kind"`
	Digest    string    `json:"digest"`
	Version   uint64    `json:"version"`
	Revision  uint64    `json:"revision"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 指定授权事件表名。
func (Event) TableName() string { return "license_event" }

// ClockRecovery 是一次性时钟恢复凭证的持久化记录，防止同一凭证重复使用。
type ClockRecovery struct {
	ID            string `gorm:"primaryKey;size:128"`
	LicenseDigest string
	AnchorAt      int64
	RawJWS        string `gorm:"column:raw_jws;type:mediumtext" json:"-"`
	CreatedAt     time.Time
}

// TableName 指定时钟恢复凭证表名。
func (ClockRecovery) TableName() string { return "license_clock_recovery" }

// State 是验签后的授权状态只读投影，供管理端与运行时展示。
type State struct {
	InstanceID        string        `json:"instance_id"`
	CustomerID        string        `json:"customer_id"`
	Environment       string        `json:"environment"`
	Revision          uint64        `json:"revision"`
	CurrentVersion    uint64        `json:"current_version"`
	HighestVersion    uint64        `json:"highest_version"`
	CurrentDigest     string        `json:"current_digest"`
	PendingDigest     string        `json:"pending_digest"`
	Current           *core.License `json:"current,omitempty"`
	Pending           *core.License `json:"pending,omitempty"`
	HighestObservedAt int64         `json:"highest_observed_at"`
	ClockBlocked      bool          `json:"clock_blocked"`
}

// ExportedLicense 是已持久化许可证产物的受控导出项。厂商续签只能由上一版许可证推导版本号
// （version = 上一版 + 1，见 commercial-license-tool/internal/issuer/issuer.go），
// 因此许可证文件丢失时，这是恢复续签能力的唯一来源；平台不接受通过配置新增可信密钥。
type ExportedLicense struct {
	Version   uint64    `json:"version"`
	Digest    string    `json:"digest"`
	RawJWS    string    `json:"raw_jws"`
	CreatedAt time.Time `json:"created_at"`
}

// LicenseExport 汇总当前与待生效许可证，并给出可用于核对续签基准的最高版本。
// 续签必须使用最高版本作为基准，否则生成的版本号会被平台以“版本不递增”拒绝。
type LicenseExport struct {
	InstanceID     string           `json:"instance_id"`
	Environment    string           `json:"environment"`
	HighestVersion uint64           `json:"highest_version"`
	Current        *ExportedLicense `json:"current,omitempty"`
	Pending        *ExportedLicense `json:"pending,omitempty"`
}

// Preview 是双阶段导入第一阶段的 dry-run 结果：差异与所需确认开关，不落库。
type Preview struct {
	State
	Digest                     string                   `json:"digest"`
	License                    core.License             `json:"license"`
	Changes                    []core.ApplicationChange `json:"changes"`
	RequiresChangeConfirmation bool                     `json:"requires_change_confirmation"`
	RequiresPendingReplacement bool                     `json:"requires_pending_replacement"`
}

// EventPage 是授权事件的分页结果。
type EventPage struct {
	Items    []Event `json:"items"`
	Total    int64   `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
}
