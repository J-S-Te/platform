package domain

import (
	"errors"
	core "github.com/J-S-Te/license-core"
	"time"
)

var (
	ErrNotInitialized     = errors.New("license deployment is not initialized")
	ErrConflict           = errors.New("license state changed or version is not increasing")
	ErrConfirmation       = errors.New("explicit license change confirmation is required")
	ErrClock              = errors.New("license clock rollback requires signed recovery")
	ErrCorrupt            = errors.New("persisted license state failed verification")
	ErrInvalid            = errors.New("invalid license management input")
	ErrTrustNotConfigured = errors.New("LICENSE_TRUST_NOT_CONFIGURED")
)

type Actor struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
}
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

func (Deployment) TableName() string { return "license_deployment" }

type Artifact struct {
	Digest    string `gorm:"primaryKey;size:64"`
	RawJWS    string `gorm:"column:raw_jws;type:mediumtext" json:"-"`
	Version   uint64
	CreatedAt time.Time
}

func (Artifact) TableName() string { return "license_artifact" }

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

func (Event) TableName() string { return "license_event" }

type ClockRecovery struct {
	ID            string `gorm:"primaryKey;size:128"`
	LicenseDigest string
	AnchorAt      int64
	RawJWS        string `gorm:"column:raw_jws;type:mediumtext" json:"-"`
	CreatedAt     time.Time
}

func (ClockRecovery) TableName() string { return "license_clock_recovery" }

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
type Preview struct {
	State
	Digest                     string                   `json:"digest"`
	License                    core.License             `json:"license"`
	Changes                    []core.ApplicationChange `json:"changes"`
	RequiresChangeConfirmation bool                     `json:"requires_change_confirmation"`
	RequiresPendingReplacement bool                     `json:"requires_pending_replacement"`
}
type EventPage struct {
	Items    []Event `json:"items"`
	Total    int64   `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
}
