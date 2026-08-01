// Package application coordinates tenant-scoped IAM management use cases.
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

var (
	// ErrNotFound means a tenant-scoped IAM entity does not exist.
	ErrNotFound = errors.New("identity resource not found")
	// ErrConflict means a requested create or state change violates an IAM uniqueness or lifecycle rule.
	ErrConflict = errors.New("identity resource conflict")
	// ErrVersionConflict means the caller supplied a stale aggregate version.
	ErrVersionConflict = errors.New("identity version conflict")
)

const (
	// DefaultPlatformApplicationCode identifies the management console application whose
	// baseline role is assigned to every newly created ordinary user.
	DefaultPlatformApplicationCode = "platform"
	// DefaultUserRoleCode is the built-in, permission-minimal role for ordinary users.
	DefaultUserRoleCode = "platform-user"
	// MaxBatchUserCreateItems bounds one atomic batch to keep request and transaction sizes finite.
	MaxBatchUserCreateItems = 100
)

// DefaultOrganizationPositionNames are created for every organization unit. They are
// organization-side appointment choices only; they do not grant authorization roles.
var DefaultOrganizationPositionNames = [...]string{
	"超级管理员",
	"销售总监",
	"技术总监",
	"财务总监",
	"销售人员",
	"审计管理员",
}

// PageRequest is the common bounded list query contract.
type PageRequest struct {
	Page               int
	PageSize           int
	Keyword            string
	Status             string
	ScopeRestricted    bool
	AllowedOrgUnitIDs  []string
	AllowedResourceIDs []string
}

// PageResult is the common list response model used by the HTTP adapter.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// UserView is the client-safe user representation. It intentionally contains a masked mobile
// value only; plaintext and ciphertext never leave the application layer.
type UserView struct {
	ID           string
	DisplayName  string
	EmployeeNo   *string
	Email        *string
	MobileMasked *string
	Status       string
	Version      uint64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserCreateInput contains the writable fields supported by POST /users.
type UserCreateInput struct {
	TenantID    string
	OperatorID  string
	DisplayName string
	EmployeeNo  *string
	Email       *string
	Mobile      *string
	Status      string
	// ApplicationRoles contains optional application-owned role assignments imported together
	// with the user. The repository resolves these codes against synchronized ACTIVE catalogs and
	// persists all users/bindings atomically; callers can never submit role IDs directly.
	ApplicationRoles []ApplicationRoleAssignment
}

// ApplicationRoleAssignment identifies one application-owned role by either stable codes or the
// human-readable names shown in the management console. Name-based import is resolved exactly in
// the current tenant; persistence still uses stable application/role IDs.
type ApplicationRoleAssignment struct {
	ApplicationCode string
	ApplicationName string
	RoleCode        string
	RoleName        string
}

// UserBatchCreateInput creates multiple ordinary users in one all-or-nothing transaction.
type UserBatchCreateInput struct {
	TenantID   string
	OperatorID string
	Items      []UserCreateInput
}

// UserDeleteInput is the optimistic-lock command for logical user deletion.
type UserDeleteInput struct {
	TenantID   string
	OperatorID string
	UserID     string
	Version    uint64
}

// UserUpdateInput contains patchable user fields. Nil optional pointers mean no change; an empty
// pointed-to value explicitly clears optional text fields.
type UserUpdateInput struct {
	TenantID     string
	OperatorID   string
	UserID       string
	DisplayName  string
	EmployeeNo   *string
	Email        *string
	Mobile       *string
	Status       *string
	Version      uint64
	UpdateMobile bool
}

// AccountUpdateInput contains the only account update fields exposed by the P0 contract.
type AccountUpdateInput struct {
	TenantID   string
	OperatorID string
	AccountID  string
	Status     string
	Version    uint64
}

// OrgUnitCreateInput contains writable P0 organization fields. Code and OrgType are
// platform-managed so callers cannot introduce conflicting organization identifiers or types.
type OrgUnitCreateInput struct {
	TenantID   string
	OperatorID string
	ParentID   *string
	Name       string
	SortOrder  int
}

// OrgUnitUpdateInput updates the mutable organization fields under optimistic locking.
// Organization codes and types stay platform-managed and therefore cannot be changed by callers.
type OrgUnitUpdateInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	ParentID   *string
	Name       string
	SortOrder  int
	Version    uint64
}

// OrgUnitDeleteInput logically deletes one organization subtree. Its positions and appointments
// are disabled together so no active appointment can reference an inactive organization.
type OrgUnitDeleteInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	Version    uint64
}

// PositionCreateInput contains caller-writable position fields. Position codes are generated by
// the platform so callers cannot introduce duplicates or mutable naming conventions.
type PositionCreateInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	Name       string
}

// PositionDeleteInput logically deletes a position using optimistic locking. Historical
// memberships remain queryable but are disabled so the deleted position can no longer confer
// inherited authorization.
type PositionDeleteInput struct {
	TenantID   string
	OperatorID string
	PositionID string
	Version    uint64
}

// MembershipCreateInput contains a user's appointment. PositionID is mandatory at persistence
// level even though it was accidentally optional in the original OpenAPI property list.
type MembershipCreateInput struct {
	TenantID             string
	OperatorID           string
	UserID               string
	OrgUnitID            string
	PositionID           string
	MembershipType       string
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
	InheritAuthorization *bool
}

// MembershipUpdateInput updates one appointment with optimistic locking.
type MembershipUpdateInput struct {
	MembershipCreateInput
	MembershipID string
	Status       *string
	Version      uint64
}

// UserWrite is the storage-ready user mutation. It holds encrypted mobile data only.
type UserWrite struct {
	UserCreateInput
	ID                      string
	RoleBindingID           string
	ApplicationRoleBindings []ApplicationRoleBindingWrite
	MobileCiphertext        []byte
	MobileHash              []byte
}

// ApplicationRoleBindingWrite carries a server-generated binding ID plus a code- or name-based
// catalog reference into the identity repository's atomic user import transaction.
type ApplicationRoleBindingWrite struct {
	ID              string
	ApplicationCode string
	ApplicationName string
	RoleCode        string
	RoleName        string
}

// ManagementRepository defines persistence behavior for the IAM management endpoints. The
// repository owns transactions that modify multiple IAM aggregates atomically.
type ManagementRepository interface {
	ListUsers(context.Context, string, PageRequest) (PageResult[domain.User], error)
	CreateUser(context.Context, UserWrite) (domain.User, error)
	CreateUsers(context.Context, []UserWrite) ([]domain.User, error)
	GetUser(context.Context, string, string) (domain.User, error)
	UpdateUser(context.Context, UserUpdateInput, []byte, []byte) (domain.User, error)
	DeleteUser(context.Context, UserDeleteInput) error

	ListAccounts(context.Context, string, PageRequest) (PageResult[domain.Account], error)
	UpdateAccount(context.Context, AccountUpdateInput) (domain.Account, error)

	ListOrgUnits(context.Context, string, string, string, PageRequest) (PageResult[domain.OrgUnit], error)
	CreateOrgUnit(context.Context, domain.OrgUnit, []domain.Position, string) (domain.OrgUnit, error)
	UpdateOrgUnit(context.Context, OrgUnitUpdateInput) (domain.OrgUnit, error)
	DeleteOrgUnit(context.Context, OrgUnitDeleteInput) error
	ListPositions(context.Context, string, string, string, PageRequest) (PageResult[domain.Position], error)
	CreatePosition(context.Context, domain.Position, string) (domain.Position, error)
	DeletePosition(context.Context, PositionDeleteInput) error

	ListMemberships(context.Context, string, PageRequest) (PageResult[domain.Membership], error)
	CreateMembership(context.Context, MembershipCreateInput, string) (domain.Membership, error)
	UpdateMembership(context.Context, MembershipUpdateInput) (domain.Membership, error)
}

// MobileProtection handles optional at-rest encryption of mobile numbers.
type MobileProtection interface {
	Encrypt(string) ([]byte, error)
	Decrypt([]byte) (string, error)
	Digest(string) []byte
}

// ManagementService implements user, account and organization management independent of HTTP.
type ManagementService struct {
	repository ManagementRepository
	mobiles    MobileProtection
	ids        IDGenerator
	clock      Clock
	hasher     PasswordHasher
}

// NewManagementService creates the P0 IAM management service.
func NewManagementService(repository ManagementRepository, mobiles MobileProtection, ids IDGenerator, clock Clock, passwordHashers ...PasswordHasher) (*ManagementService, error) {
	if repository == nil || mobiles == nil || ids == nil || clock == nil {
		return nil, errors.New("identity management dependencies must not be nil")
	}
	var hasher PasswordHasher
	if len(passwordHashers) > 0 {
		hasher = passwordHashers[0]
	}
	return &ManagementService{repository: repository, mobiles: mobiles, ids: ids, clock: clock, hasher: hasher}, nil
}

// ListUsers returns tenant-scoped users and masks mobile values before returning them.
func (service *ManagementService) ListUsers(ctx context.Context, tenantID string, query PageRequest) (PageResult[UserView], error) {
	query = normalizePageRequest(query)
	result, err := service.repository.ListUsers(ctx, tenantID, query)
	if err != nil {
		return PageResult[UserView]{}, err
	}
	items := make([]UserView, 0, len(result.Items))
	for _, user := range result.Items {
		view, err := service.toUserView(user)
		if err != nil {
			return PageResult[UserView]{}, err
		}
		items = append(items, view)
	}
	return PageResult[UserView]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total}, nil
}

// CreateUser creates one tenant-scoped natural person and assigns the built-in ordinary-user role.
// Login accounts remain a separate lifecycle and are not provisioned by this operation.
func (service *ManagementService) CreateUser(ctx context.Context, input UserCreateInput) (UserView, error) {
	users, err := service.CreateUsersBatch(ctx, UserBatchCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, Items: []UserCreateInput{input},
	})
	if err != nil {
		return UserView{}, err
	}
	return users[0], nil
}

// CreateUsersBatch validates and persists an atomic batch. Employee numbers and role-binding IDs
// are generated by the backend so callers cannot create unbound users or collide on manual IDs.
func (service *ManagementService) CreateUsersBatch(ctx context.Context, input UserBatchCreateInput) ([]UserView, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" ||
		len(input.Items) == 0 || len(input.Items) > MaxBatchUserCreateItems {
		return nil, ErrValidation
	}

	now := service.clock.Now().UTC()
	writes := make([]UserWrite, 0, len(input.Items))
	for _, item := range input.Items {
		item.TenantID = input.TenantID
		item.OperatorID = input.OperatorID
		// Employee numbers are backend-managed. Ignore values supplied by direct
		// application callers before validation, then assign the generated value.
		item.EmployeeNo = nil
		applicationRoles, err := normalizeApplicationRoleAssignments(item.ApplicationRoles)
		if err != nil {
			return nil, err
		}
		item.ApplicationRoles = applicationRoles
		if err := validateUserCreate(item); err != nil {
			return nil, err
		}
		id, err := service.ids.New(now)
		if err != nil {
			return nil, fmt.Errorf("generate user ID: %w", err)
		}
		bindingID, err := service.ids.New(now)
		if err != nil {
			return nil, fmt.Errorf("generate ordinary-user role binding ID: %w", err)
		}
		employeeNo := "EMP-" + strings.ToUpper(id)
		item.EmployeeNo = &employeeNo
		write, err := service.prepareUserWrite(item, id)
		if err != nil {
			return nil, err
		}
		write.RoleBindingID = bindingID
		write.ApplicationRoleBindings = make([]ApplicationRoleBindingWrite, 0, len(applicationRoles))
		for _, role := range applicationRoles {
			roleBindingID, err := service.ids.New(now)
			if err != nil {
				return nil, fmt.Errorf("generate imported application role binding ID: %w", err)
			}
			write.ApplicationRoleBindings = append(write.ApplicationRoleBindings, ApplicationRoleBindingWrite{
				ID:              roleBindingID,
				ApplicationCode: role.ApplicationCode, ApplicationName: role.ApplicationName,
				RoleCode: role.RoleCode, RoleName: role.RoleName,
			})
		}
		writes = append(writes, write)
	}

	users, err := service.repository.CreateUsers(ctx, writes)
	if err != nil {
		return nil, err
	}
	views := make([]UserView, 0, len(users))
	for _, user := range users {
		view, err := service.toUserView(user)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func normalizeApplicationRoleAssignments(assignments []ApplicationRoleAssignment) ([]ApplicationRoleAssignment, error) {
	if len(assignments) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(assignments))
	result := make([]ApplicationRoleAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		assignment.ApplicationCode = strings.TrimSpace(assignment.ApplicationCode)
		assignment.ApplicationName = strings.TrimSpace(assignment.ApplicationName)
		assignment.RoleCode = strings.TrimSpace(assignment.RoleCode)
		assignment.RoleName = strings.TrimSpace(assignment.RoleName)
		if (assignment.ApplicationCode == "") == (assignment.ApplicationName == "") ||
			(assignment.RoleCode == "") == (assignment.RoleName == "") ||
			len(assignment.ApplicationCode) > 64 || len([]rune(assignment.ApplicationName)) > 128 ||
			len(assignment.RoleCode) > 128 || len([]rune(assignment.RoleName)) > 128 {
			return nil, ErrValidation
		}
		key := applicationRoleAssignmentKey(assignment.ApplicationCode, assignment.ApplicationName, assignment.RoleCode, assignment.RoleName)
		if _, exists := seen[key]; exists {
			return nil, ErrValidation
		}
		seen[key] = struct{}{}
		result = append(result, assignment)
	}
	return result, nil
}

func applicationRoleAssignmentKey(applicationCode, applicationName, roleCode, roleName string) string {
	return applicationCode + "\x00" + applicationName + "\x00" + roleCode + "\x00" + roleName
}

// DeleteUser logically removes a user while retaining its identity row for audit and referential integrity.
// All associated accounts, memberships and active browser sessions are disabled/revoked atomically.
func (service *ManagementService) DeleteUser(ctx context.Context, input UserDeleteInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" ||
		strings.TrimSpace(input.UserID) == "" || input.Version == 0 || input.OperatorID == input.UserID {
		return ErrValidation
	}
	return service.repository.DeleteUser(ctx, input)
}

// GetUser returns a single tenant-scoped user.
func (service *ManagementService) GetUser(ctx context.Context, tenantID, userID string) (UserView, error) {
	user, err := service.repository.GetUser(ctx, tenantID, userID)
	if err != nil {
		return UserView{}, err
	}
	return service.toUserView(user)
}

// UpdateUser applies a versioned user update.
func (service *ManagementService) UpdateUser(ctx context.Context, input UserUpdateInput) (UserView, error) {
	if err := validateUserUpdate(input); err != nil {
		return UserView{}, err
	}
	var ciphertext, digest []byte
	var err error
	if input.UpdateMobile {
		ciphertext, digest, err = service.protectMobile(input.Mobile)
		if err != nil {
			return UserView{}, err
		}
	}
	user, err := service.repository.UpdateUser(ctx, input, ciphertext, digest)
	if err != nil {
		return UserView{}, err
	}
	return service.toUserView(user)
}

// ListAccounts lists tenant-scoped accounts without exposing credentials or failed-attempt data.
func (service *ManagementService) ListAccounts(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Account], error) {
	return service.repository.ListAccounts(ctx, tenantID, normalizePageRequest(query))
}

// UpdateAccount changes an account between ACTIVE and DISABLED with optimistic locking. LOCKED is
// owned by password-login failure handling and cannot be set through this management endpoint.
func (service *ManagementService) UpdateAccount(ctx context.Context, input AccountUpdateInput) (domain.Account, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.AccountID == "" || input.Version == 0 ||
		(input.Status != domain.StatusActive && input.Status != domain.StatusDisabled) {
		return domain.Account{}, ErrValidation
	}
	return service.repository.UpdateAccount(ctx, input)
}

// ListOrgUnits lists tenant organization units using the shared, bounded pagination contract.
func (service *ManagementService) ListOrgUnits(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.OrgUnit], error) {
	query = normalizePageRequest(query)
	if tenantID == "" || !validStatusFilter(query.Status) {
		return PageResult[domain.OrgUnit]{}, ErrValidation
	}
	return service.repository.ListOrgUnits(ctx, tenantID, query.Keyword, query.Status, query)
}

// CreateOrgUnit appends a tenant-scoped organization node. The organization code is derived
// from the server-generated ULID and therefore does not depend on mutable names or caller input.
func (service *ManagementService) CreateOrgUnit(ctx context.Context, input OrgUnitCreateInput) (domain.OrgUnit, error) {
	if input.TenantID == "" || input.OperatorID == "" || !validName(input.Name, 100) {
		return domain.OrgUnit{}, ErrValidation
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.OrgUnit{}, fmt.Errorf("generate organization ID: %w", err)
	}
	orgUnit := domain.OrgUnit{ID: id, TenantID: input.TenantID, ParentID: normalizedOptional(input.ParentID), Code: generatedOrganizationCode(id), Name: strings.TrimSpace(input.Name), OrgType: "DEPARTMENT", SortOrder: input.SortOrder, Status: domain.StatusActive, Version: 1}
	positions := make([]domain.Position, 0, len(DefaultOrganizationPositionNames))
	for _, name := range DefaultOrganizationPositionNames {
		positionID, err := service.ids.New(now)
		if err != nil {
			return domain.OrgUnit{}, fmt.Errorf("generate default position ID: %w", err)
		}
		positions = append(positions, domain.Position{
			ID: positionID, TenantID: input.TenantID, OrgUnitID: orgUnit.ID,
			Code: generatedPositionCode(positionID), Name: name, Status: domain.StatusActive, Version: 1,
		})
	}
	return service.repository.CreateOrgUnit(ctx, orgUnit, positions, input.OperatorID)
}

// UpdateOrgUnit updates a tenant-scoped organization node. Moving a node is allowed only when it
// cannot introduce a cycle; the repository rewrites the persisted subtree path atomically.
func (service *ManagementService) UpdateOrgUnit(ctx context.Context, input OrgUnitUpdateInput) (domain.OrgUnit, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || input.Version == 0 || !validName(input.Name, 100) {
		return domain.OrgUnit{}, ErrValidation
	}
	input.ParentID = normalizedOptional(input.ParentID)
	return service.repository.UpdateOrgUnit(ctx, input)
}

// DeleteOrgUnit retains the organization hierarchy for audit/history but disables the target
// subtree and all dependent positions and appointments in one transaction.
func (service *ManagementService) DeleteOrgUnit(ctx context.Context, input OrgUnitDeleteInput) error {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || input.Version == 0 {
		return ErrValidation
	}
	return service.repository.DeleteOrgUnit(ctx, input)
}

// generatedOrganizationCode creates a stable, tenant-unique business code from the same ULID
// used as the organization primary key. The ORG prefix distinguishes it from other managed IDs.
func generatedOrganizationCode(id string) string {
	return "ORG-" + strings.ToUpper(strings.TrimSpace(id))
}

// ListPositions returns tenant-scoped positions using the shared, bounded pagination contract.
func (service *ManagementService) ListPositions(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Position], error) {
	query = normalizePageRequest(query)
	if tenantID == "" || !validStatusFilter(query.Status) {
		return PageResult[domain.Position]{}, ErrValidation
	}
	return service.repository.ListPositions(ctx, tenantID, query.Keyword, query.Status, query)
}

// CreatePosition creates an ACTIVE position under an existing ACTIVE organization unit. The
// business code is derived from the generated ULID and remains stable if the position is renamed.
func (service *ManagementService) CreatePosition(ctx context.Context, input PositionCreateInput) (domain.Position, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || !validName(input.Name, 100) {
		return domain.Position{}, ErrValidation
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Position{}, fmt.Errorf("generate position ID: %w", err)
	}
	position := domain.Position{ID: id, TenantID: input.TenantID, OrgUnitID: input.OrgUnitID, Code: generatedPositionCode(id), Name: strings.TrimSpace(input.Name), Status: domain.StatusActive, Version: 1}
	return service.repository.CreatePosition(ctx, position, input.OperatorID)
}

// generatedPositionCode creates a stable position code from its ULID primary key.
func generatedPositionCode(id string) string {
	return "POS-" + strings.ToUpper(strings.TrimSpace(id))
}

// DeletePosition preserves the position and appointment history while disabling the position and
// all active memberships that reference it. New inherited authorization snapshots are invalidated
// by the repository in the same transaction.
func (service *ManagementService) DeletePosition(ctx context.Context, input PositionDeleteInput) error {
	if input.TenantID == "" || input.OperatorID == "" || input.PositionID == "" || input.Version == 0 {
		return ErrValidation
	}
	return service.repository.DeletePosition(ctx, input)
}

// ListMemberships returns historical and active tenant-scoped appointments.
func (service *ManagementService) ListMemberships(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Membership], error) {
	return service.repository.ListMemberships(ctx, tenantID, normalizePageRequest(query))
}

// CreateMembership creates a user appointment and preserves the one-primary-membership invariant.
func (service *ManagementService) CreateMembership(ctx context.Context, input MembershipCreateInput) (domain.Membership, error) {
	if err := validateMembership(input.TenantID, input.OperatorID, input.UserID, input.OrgUnitID, input.PositionID, input.MembershipType, input.EffectiveFrom, input.EffectiveTo); err != nil {
		return domain.Membership{}, err
	}
	// Backward compatibility: callers that do not send the new switch keep the historical
	// behavior where an active appointment participates in organization/position inheritance.
	if input.InheritAuthorization == nil {
		enabled := true
		input.InheritAuthorization = &enabled
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Membership{}, fmt.Errorf("generate membership ID: %w", err)
	}
	return service.repository.CreateMembership(ctx, input, id)
}

// UpdateMembership applies a versioned appointment update.
func (service *ManagementService) UpdateMembership(ctx context.Context, input MembershipUpdateInput) (domain.Membership, error) {
	if input.MembershipID == "" || input.Version == 0 || input.Status == nil ||
		(*input.Status != domain.StatusActive && *input.Status != domain.StatusDisabled) {
		return domain.Membership{}, ErrValidation
	}
	if err := validateMembership(input.TenantID, input.OperatorID, input.UserID, input.OrgUnitID, input.PositionID, input.MembershipType, input.EffectiveFrom, input.EffectiveTo); err != nil {
		return domain.Membership{}, err
	}
	if input.InheritAuthorization == nil {
		enabled := true
		input.InheritAuthorization = &enabled
	}
	return service.repository.UpdateMembership(ctx, input)
}

// ErrValidation allows adapters to distinguish invalid input from persistence failures.
var ErrValidation = errors.New("identity request validation failed")

func (service *ManagementService) prepareUserWrite(input UserCreateInput, id string) (UserWrite, error) {
	ciphertext, digest, err := service.protectMobile(input.Mobile)
	if err != nil {
		return UserWrite{}, err
	}
	return UserWrite{UserCreateInput: input, ID: id, MobileCiphertext: ciphertext, MobileHash: digest}, nil
}

func (service *ManagementService) protectMobile(mobile *string) ([]byte, []byte, error) {
	if mobile == nil || strings.TrimSpace(*mobile) == "" {
		return nil, nil, nil
	}
	normalized, ok := normalizeMobile(*mobile)
	if !ok {
		return nil, nil, ErrValidation
	}
	ciphertext, err := service.mobiles.Encrypt(normalized)
	if err != nil {
		return nil, nil, fmt.Errorf("protect mobile: %w", err)
	}
	return ciphertext, service.mobiles.Digest(normalized), nil
}

func (service *ManagementService) toUserView(user domain.User) (UserView, error) {
	view := UserView{ID: user.ID, DisplayName: user.DisplayName, EmployeeNo: user.EmployeeNo, Email: user.Email, Status: user.Status, Version: user.Version, CreatedAt: user.CreatedAt.UTC(), UpdatedAt: user.UpdatedAt.UTC()}
	if len(user.MobileCiphertext) == 0 {
		return view, nil
	}
	mobile, err := service.mobiles.Decrypt(user.MobileCiphertext)
	if err != nil {
		return UserView{}, fmt.Errorf("read protected mobile for user %s: %w", user.ID, err)
	}
	masked := maskMobile(mobile)
	view.MobileMasked = &masked
	return view, nil
}

func normalizePageRequest(query PageRequest) PageRequest {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.Status = strings.TrimSpace(query.Status)
	return query
}

func validateUserCreate(input UserCreateInput) error {
	if input.TenantID == "" || input.OperatorID == "" || !validName(input.DisplayName, 100) || !validStatus(input.Status) {
		return ErrValidation
	}
	if input.EmployeeNo != nil && len(strings.TrimSpace(*input.EmployeeNo)) > 64 {
		return ErrValidation
	}
	if input.Email != nil && !validEmail(*input.Email) {
		return ErrValidation
	}
	if input.Mobile != nil && len(strings.TrimSpace(*input.Mobile)) > 32 {
		return ErrValidation
	}
	return nil
}

func validateUserUpdate(input UserUpdateInput) error {
	if input.TenantID == "" || input.OperatorID == "" || input.UserID == "" || input.Version == 0 || !validName(input.DisplayName, 100) {
		return ErrValidation
	}
	if input.EmployeeNo != nil && len(strings.TrimSpace(*input.EmployeeNo)) > 64 {
		return ErrValidation
	}
	if input.Email != nil && !validEmail(*input.Email) {
		return ErrValidation
	}
	if input.Status != nil && !validStatus(*input.Status) {
		return ErrValidation
	}
	if input.UpdateMobile && input.Mobile != nil && len(strings.TrimSpace(*input.Mobile)) > 32 {
		return ErrValidation
	}
	return nil
}

func validateMembership(tenantID, operatorID, userID, orgUnitID, positionID, kind string, from, to *time.Time) error {
	if tenantID == "" || operatorID == "" || userID == "" || orgUnitID == "" || positionID == "" || (kind != domain.MembershipPrimary && kind != domain.MembershipSecondary) {
		return ErrValidation
	}
	// A long-term appointment omits both dates. A short-term appointment must provide the complete
	// inclusive date range; accepting only one endpoint would make validity ambiguous.
	if (from == nil) != (to == nil) {
		return ErrValidation
	}
	if from != nil && from.After(*to) {
		return ErrValidation
	}
	return nil
}

func validName(value string, limit int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= limit
}
func validCode(value string, limit int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= limit
}
func validStatus(value string) bool {
	return value == domain.StatusActive || value == domain.StatusDisabled
}
func validStatusFilter(value string) bool { return value == "" || validStatus(value) }
func validEmail(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || (len(value) <= 320 && strings.Count(value, "@") == 1 && !strings.HasPrefix(value, "@") && !strings.HasSuffix(value, "@"))
}
func normalizedOptional(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	return &normalized
}
func normalizeMobile(value string) (string, bool) {
	value = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(value), " ", ""), "-", "")
	if value == "" || len(value) > 32 {
		return "", false
	}
	for index, character := range value {
		if !(character >= '0' && character <= '9') && !(character == '+' && index == 0) {
			return "", false
		}
	}
	return value, true
}
func maskMobile(value string) string {
	if len(value) <= 4 {
		return "****"
	}
	return value[:3] + "****" + value[len(value)-4:]
}
