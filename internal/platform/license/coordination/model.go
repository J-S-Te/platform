package coordination

import (
	"errors"
	runtime "github.com/J-S-Te/license-core/runtime"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid runtime coordination input")
	ErrForbidden = errors.New("runtime machine binding denied")
	ErrConflict  = errors.New("runtime coordination revision conflict")
	ErrNotReady  = errors.New("runtime coverage is incomplete or stale")
)

type Inventory struct {
	ID             uint64 `gorm:"primaryKey"`
	Project        string
	EvidenceDigest string
	CollectedAt    time.Time
	CreatedAt      time.Time
}

func (Inventory) TableName() string { return "license_runtime_inventory" }

// InstallationBaseline records legacy facts independently of the target release.
// EvidenceJSON contains bounded Docker projections, never credentials or env.
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

func (InstallationBaseline) TableName() string { return "license_installation_baseline" }

type Application struct {
	Application       string                   `gorm:"primaryKey;size:128" json:"application"`
	State             runtime.EnforcementState `json:"state"`
	Revision          uint64                   `json:"revision"`
	MigrationEligible bool                     `json:"migration_eligible"`
	ContentDigest     string                   `json:"-"`
	UpdatedAt         time.Time                `json:"updated_at"`
}

func (Application) TableName() string { return "license_runtime_application" }

type Member struct {
	ServiceID   string `gorm:"primaryKey;size:128" json:"service_id"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	// OAuthClientID is the reviewed external client_id, not the iam OAuth row PK.
	OAuthClientID  string     `gorm:"column:oauth_client_id" json:"oauth_client_id"`
	CoverageDigest string     `json:"coverage_digest"`
	ImageDigest    string     `json:"image_digest"`
	ReadyAt        *time.Time `json:"ready_at"`
	IssuedRevision uint64     `json:"issued_revision"`
	IssuedDigest   string     `json:"-"`
	RawJWS         string     `gorm:"column:raw_jws" json:"-"`
	AckRevision    uint64     `json:"ack_revision"`
	AckAt          *time.Time `json:"ack_at"`
	CreatedAt      time.Time  `json:"created_at"`
	RetiredAt      *time.Time `json:"retired_at"`
}

func (Member) TableName() string { return "license_runtime_member" }

type ServiceSpec struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	ServiceID   string `json:"service_id"`
	// OAuthClientID uses the same external identity as Member and lifecycle tombstones.
	OAuthClientID  string `json:"oauth_client_id"`
	CoverageDigest string `json:"coverage_digest"`
	ImageDigest    string `json:"image_digest"`
}
type ReadyInput struct {
	Protocol       int    `json:"protocol"`
	CoverageDigest string `json:"coverage_digest"`
	ImageDigest    string `json:"image_digest"`
}
type AckInput struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}
type SnapshotOutput struct {
	RawJWS   string `json:"raw_jws"`
	Digest   string `json:"digest"`
	Revision uint64 `json:"revision"`
}
type Status struct {
	Application
	DeploymentRevision uint64   `json:"deployment_revision"`
	SnapshotCurrent    bool     `json:"snapshot_current"`
	Members            []Member `json:"members"`
}
