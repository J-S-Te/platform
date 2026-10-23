package coordination

import (
	"errors"
	"time"

	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
)

var (
	ErrInvalid   = errors.New("invalid runtime coordination input")
	ErrForbidden = errors.New("runtime machine binding denied")
	ErrConflict  = errors.New("runtime coordination revision conflict")
	ErrNotReady  = errors.New("runtime coverage is incomplete or stale")
)

// Inventory 是冻结后的安装清单：仅保存项目标识与证据摘要，证据本体在基线行中。
type Inventory struct {
	ID             uint64 `gorm:"primaryKey"`
	Project        string
	EvidenceDigest string
	CollectedAt    time.Time
	CreatedAt      time.Time
}

// TableName 绑定 GORM 模型到 license_runtime_inventory 表。
func (Inventory) TableName() string { return "license_runtime_inventory" }

// InstallationBaseline 独立于目标发布记录遗留安装事实。EvidenceJSON 只包含
// 有界的 Docker 投影，绝不含凭据或环境变量。
type InstallationBaseline struct {
	ID             uint64    `gorm:"primaryKey" json:"-"`
	Scenario       string    `json:"scenario"`
	Project        string    `json:"project"`
	InstanceID     string    `json:"instance_id"`
	Environment    string    `json:"environment"`
	EvidenceDigest string    `json:"evidence_digest"`
	EvidenceJSON   string    `gorm:"type:mediumtext" json:"-"`
	CollectedAt    time.Time `json:"collected_at"`
	CreatedAt      time.Time `json:"created_at"`
	TenantID       string    `json:"tenant_id"`
	UserID         string    `json:"user_id"`
}

// TableName 绑定 GORM 模型到 license_installation_baseline 表。
func (InstallationBaseline) TableName() string { return "license_installation_baseline" }

// Application 是一个应用的运行时强制执行状态与注册模式；Revision 是乐观锁，
// ContentDigest 变化即代表必须重新 READY/ACK。
type Application struct {
	Application       string                   `gorm:"primaryKey;size:128" json:"application"`
	State             runtime.EnforcementState `json:"state"`
	RegistrationMode  catalog.RegistrationMode `gorm:"column:registration_mode;size:32;not null;default:LEGACY_IMAGE_V1" json:"registration_mode"`
	CatalogVersion    int                      `gorm:"column:catalog_version;not null;default:0" json:"catalog_version"`
	Revision          uint64                   `json:"revision"`
	MigrationEligible bool                     `json:"migration_eligible"`
	ContentDigest     string                   `json:"-"`
	UpdatedAt         time.Time                `json:"updated_at"`
}

// TableName 绑定 GORM 模型到 license_runtime_application 表。
func (Application) TableName() string { return "license_runtime_application" }

// Member 是一个已注册的运行时服务成员，承载 READY/ACK/签发状态；
// RetiredAt 非空表示成员已退役且身份不可复用。
type Member struct {
	ServiceID   string `gorm:"primaryKey;size:128" json:"service_id"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	// OAuthClientID 是经审核的外部 client_id，不是 iam OAuth 行的主键。
	OAuthClientID        string     `gorm:"column:oauth_client_id" json:"oauth_client_id"`
	RegistrationProtocol int        `gorm:"column:registration_protocol;not null;default:1" json:"registration_protocol"`
	CatalogVersion       int        `gorm:"column:catalog_version;not null;default:0" json:"catalog_version"`
	CredentialGeneration string     `gorm:"column:credential_generation;size:128;not null;default:''" json:"credential_generation"`
	CoverageDigest       string     `json:"coverage_digest"`
	ImageDigest          string     `json:"image_digest"`
	ReadyAt              *time.Time `json:"ready_at"`
	IssuedRevision       uint64     `json:"issued_revision"`
	IssuedDigest         string     `json:"-"`
	RawJWS               string     `gorm:"column:raw_jws" json:"-"`
	AckRevision          uint64     `json:"ack_revision"`
	AckAt                *time.Time `json:"ack_at"`
	CreatedAt            time.Time  `json:"created_at"`
	RetiredAt            *time.Time `json:"retired_at"`
}

// TableName 绑定 GORM 模型到 license_runtime_member 表。
func (Member) TableName() string { return "license_runtime_member" }

// ServiceSpec 是一次成员注册的规格声明；注册协议决定哪些字段是约束、哪些仅诊断。
type ServiceSpec struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	ServiceID   string `json:"service_id"`
	// OAuthClientID 与 Member 及生命周期墓碑使用同一外部身份。
	OAuthClientID        string `json:"oauth_client_id"`
	RegistrationProtocol int    `json:"registration_protocol,omitempty"`
	CatalogVersion       int    `json:"catalog_version,omitempty"`
	CredentialGeneration string `json:"credential_generation,omitempty"`
	CoverageDigest       string `json:"coverage_digest,omitempty"`
	ImageDigest          string `json:"image_digest,omitempty"`
}

// ReadyInput 是机器成员提交 READY 的载荷。
type ReadyInput struct {
	Protocol       int    `json:"protocol"`
	CoverageDigest string `json:"coverage_digest"`
	ImageDigest    string `json:"image_digest"`
	CatalogVersion int    `json:"catalog_version,omitempty"`
}

// AckInput 是机器成员确认快照的载荷；Digest 必须与已签发摘要一致。
type AckInput struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// SnapshotOutput 是一次签发的快照：原始 JWS、其哈希与应用修订。
type SnapshotOutput struct {
	RawJWS   string `json:"raw_jws"`
	Digest   string `json:"digest"`
	Revision uint64 `json:"revision"`
}

// Status 是应用的协调状态视图：应用行、部署修订、快照是否为当前版本及成员清单。
type Status struct {
	Application
	DeploymentRevision uint64   `json:"deployment_revision"`
	SnapshotCurrent    bool     `json:"snapshot_current"`
	Members            []Member `json:"members"`
}
