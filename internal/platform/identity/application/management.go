// Package application coordinates tenant-scoped IAM management use cases.
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

var (
	// ErrNotFound means a tenant-scoped IAM entity does not exist.
	ErrNotFound = errors.New("identity resource not found")
	// ErrConflict means a requested create or state change violates an IAM uniqueness or lifecycle rule.
	ErrConflict = errors.New("identity resource conflict")
	// ErrVersionConflict means the caller supplied a stale aggregate version.
	ErrVersionConflict = errors.New("identity version conflict")
)

// PageRequest is the common bounded list query contract.
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
	Status   string
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

// OrgUnitCreateInput contains writable P0 organization fields. OrgType is platform-managed as
// DEPARTMENT because the current public contract does not expose organization type selection.
type OrgUnitCreateInput struct {
	TenantID   string
	OperatorID string
	ParentID   *string
	Code       string
	Name       string
	SortOrder  int
}

// PositionCreateInput contains writable P0 position fields.
type PositionCreateInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	Code       string
	Name       string
}

// MembershipCreateInput contains a user's appointment. PositionID is mandatory at persistence
// level even though it was accidentally optional in the original OpenAPI property list.
type MembershipCreateInput struct {
	TenantID       string
	OperatorID     string
	UserID         string
	OrgUnitID      string
	PositionID     string
	MembershipType string
	EffectiveFrom  *time.Time
	EffectiveTo    *time.Time
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
	ID               string
	MobileCiphertext []byte
	MobileHash       []byte
}

// ManagementRepository defines persistence behavior for the IAM management endpoints. The
// repository owns transactions that modify multiple IAM aggregates atomically.
type ManagementRepository interface {
	ListUsers(context.Context, string, PageRequest) (PageResult[domain.User], error)
	CreateUser(context.Context, UserWrite) (domain.User, error)
	GetUser(context.Context, string, string) (domain.User, error)
	UpdateUser(context.Context, UserUpdateInput, []byte, []byte) (domain.User, error)

	ListAccounts(context.Context, string, PageRequest) (PageResult[domain.Account], error)
	UpdateAccount(context.Context, AccountUpdateInput) (domain.Account, error)

	ListOrgUnits(context.Context, string, string, string, PageRequest) (PageResult[domain.OrgUnit], error)
	CreateOrgUnit(context.Context, domain.OrgUnit, string) (domain.OrgUnit, error)
	ListPositions(context.Context, string, string, string, PageRequest) (PageResult[domain.Position], error)
	CreatePosition(context.Context, domain.Position, string) (domain.Position, error)

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
}

// NewManagementService creates the P0 IAM management service.
func NewManagementService(repository ManagementRepository, mobiles MobileProtection, ids IDGenerator, clock Clock) (*ManagementService, error) {
	if repository == nil || mobiles == nil || ids == nil || clock == nil {
		return nil, errors.New("identity management dependencies must not be nil")
	}
	return &ManagementService{repository: repository, mobiles: mobiles, ids: ids, clock: clock}, nil
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

// CreateUser creates a tenant-scoped natural person. It never creates an iam_account because the
// published P0 contract has no account-creation or password-provisioning endpoint.
func (service *ManagementService) CreateUser(ctx context.Context, input UserCreateInput) (UserView, error) {
	if err := validateUserCreate(input); err != nil {
		return UserView{}, err
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return UserView{}, fmt.Errorf("generate user ID: %w", err)
	}
	write, err := service.prepareUserWrite(input, id)
	if err != nil {
		return UserView{}, err
	}
	user, err := service.repository.CreateUser(ctx, write)
	if err != nil {
		return UserView{}, err
	}
	return service.toUserView(user)
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

// ListOrgUnits lists the tenant organization tree. Pagination is intentionally not applied because
// clients need parent and path information to construct a complete tree.
func (service *ManagementService) ListOrgUnits(ctx context.Context, tenantID, keyword, status string) (PageResult[domain.OrgUnit], error) {
	if tenantID == "" || !validStatusFilter(status) {
		return PageResult[domain.OrgUnit]{}, ErrValidation
	}
	return service.repository.ListOrgUnits(ctx, tenantID, strings.TrimSpace(keyword), status, PageRequest{Page: 1, PageSize: 100})
}

// CreateOrgUnit appends a tenant-scoped organization node.
func (service *ManagementService) CreateOrgUnit(ctx context.Context, input OrgUnitCreateInput) (domain.OrgUnit, error) {
	if input.TenantID == "" || input.OperatorID == "" || !validCode(input.Code, 64) || !validName(input.Name, 100) {
		return domain.OrgUnit{}, ErrValidation
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.OrgUnit{}, fmt.Errorf("generate organization ID: %w", err)
	}
	orgUnit := domain.OrgUnit{ID: id, TenantID: input.TenantID, ParentID: normalizedOptional(input.ParentID), Code: strings.TrimSpace(input.Code), Name: strings.TrimSpace(input.Name), OrgType: "DEPARTMENT", SortOrder: input.SortOrder, Status: domain.StatusActive, Version: 1}
	return service.repository.CreateOrgUnit(ctx, orgUnit, input.OperatorID)
}

// ListPositions returns tenant-scoped positions.
func (service *ManagementService) ListPositions(ctx context.Context, tenantID, keyword, status string) (PageResult[domain.Position], error) {
	if tenantID == "" || !validStatusFilter(status) {
		return PageResult[domain.Position]{}, ErrValidation
	}
	return service.repository.ListPositions(ctx, tenantID, strings.TrimSpace(keyword), status, PageRequest{Page: 1, PageSize: 100})
}

// CreatePosition creates an ACTIVE position under an existing ACTIVE organization unit.
func (service *ManagementService) CreatePosition(ctx context.Context, input PositionCreateInput) (domain.Position, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || !validCode(input.Code, 64) || !validName(input.Name, 100) {
		return domain.Position{}, ErrValidation
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Position{}, fmt.Errorf("generate position ID: %w", err)
	}
	position := domain.Position{ID: id, TenantID: input.TenantID, OrgUnitID: input.OrgUnitID, Code: strings.TrimSpace(input.Code), Name: strings.TrimSpace(input.Name), Status: domain.StatusActive, Version: 1}
	return service.repository.CreatePosition(ctx, position, input.OperatorID)
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
	if from != nil && to != nil && from.After(*to) {
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
