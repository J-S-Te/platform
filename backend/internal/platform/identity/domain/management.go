package domain

import "time"

const (
	// StatusActive marks an entity as available for normal use.
	StatusActive = "ACTIVE"
	// StatusDisabled marks an entity as retained but unavailable for new operations.
	StatusDisabled = "DISABLED"
	// AccountStatusLocked is maintained by the authentication module while a password account is locked.
	AccountStatusLocked = "LOCKED"
	// MembershipPrimary identifies a user's primary organizational appointment.
	MembershipPrimary = "PRIMARY"
	// MembershipSecondary identifies a non-primary organizational appointment.
	MembershipSecondary = "SECONDARY"
)

// User is a natural person within one tenant. MobileCiphertext is deliberately never returned by
// the HTTP adapter; it is only used by the application service to build a masked DTO.
type User struct {
	ID               string
	TenantID         string
	EmployeeNo       *string
	PMSPersonID      *string
	DisplayName      string
	Email            *string
	MobileCiphertext []byte
	Status           string
	Version          uint64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Account is a login subject associated with a user where applicable.
type Account struct {
	ID       string
	TenantID string
	UserID   *string
	// User is the display reference for the linked natural person. UserID remains
	// available for compatibility and write operations, while management reads use
	// this reference to avoid forcing every client to issue one request per account.
	User        *ReferenceName
	AccountName string
	AccountType string
	AuthSource  string
	// PasswordInitialized is management metadata only; no credential hash or failure state is
	// exposed. It lets administrators distinguish a credential-free reserved external account
	// from an account whose password may be reset.
	PasswordInitialized bool
	Status              string
	LastLoginAt         *time.Time
	ValidUntil          *time.Time
	Version             uint64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OrgUnit represents one node in a tenant-scoped organization tree.
type OrgUnit struct {
	ID        string
	TenantID  string
	ParentID  *string
	Code      string
	Name      string
	OrgType   string
	Path      string
	Depth     uint
	SortOrder int
	Status    string
	Version   uint64
}

// Position belongs to exactly one organization unit.
type Position struct {
	ID        string
	TenantID  string
	OrgUnitID string
	Code      string
	Name      string
	Status    string
	Version   uint64
}

// Membership represents a user appointment, including primary and secondary positions.
type Membership struct {
	ID                   string
	TenantID             string
	User                 ReferenceName
	OrgUnit              ReferenceName
	Position             ReferenceName
	MembershipType       string
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
	Status               string
	Version              uint64
	IsPrimary            bool
	InheritAuthorization bool
}
