package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	safeUpstreamURLPattern = regexp.MustCompile(`^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~-]+)(:([0-9]{1,5}))?(/[A-Za-z0-9._~!%+/@-]*)?$`)

	// ErrNotFound means a tenant-scoped application registry resource does not exist.
	ErrNotFound = errors.New("application registry resource not found")
	// ErrConflict means a requested registry mutation violates a uniqueness or lifecycle rule.
	ErrConflict = errors.New("application registry resource conflict")
	// ErrVersionConflict means the mutation was made against an outdated aggregate version.
	ErrVersionConflict = errors.New("application registry version conflict")
	// ErrValidation means a management request cannot be safely persisted.
	ErrValidation = errors.New("invalid application registry input")
	// ErrEnvironmentDeletionBlocked means an environment still has configuration or audit evidence
	// that must be retained instead of being deleted.
	ErrEnvironmentDeletionBlocked = errors.New("application environment deletion blocked by retained records")
)

const (
	defaultManagementPageSize = 20
	maxManagementPageSize     = 100
	maxMetadataBytes          = 64 << 10
	builtInApplicationCode    = "platform"
)

// IdentifierGenerator supplies sortable identifiers for applications and environments.
type IdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// PageRequest contains the supported bounded application-management list filters.
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
	Status   string
}

// PageResult is the tenant-safe list response returned by management queries.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// Application is a tenant-scoped business-system registration. It intentionally excludes all
// OAuth client, grant, callback and credential data.
type Application struct {
	ID              string
	TenantID        string
	Code            string
	Name            string
	ApplicationType string
	OwnerOrgID      *string
	OwnerUserID     *string
	HomepageURL     *string
	Description     *string
	Status          string
	Version         uint64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Environment is a deployment boundary under an application. Metadata is non-secret JSON
// object data only; OAuth credentials and private keys are intentionally not modeled here.
//
// Gateway fields split the public-facing address from the internal upstream so the platform can
// act as a single-entry reverse proxy:
//   - BaseURL:     public base URL every external caller (browser, OAuth client) sees for this
//     environment; used as the prefix when LoginTarget.TargetURI is relative.
//   - UpstreamURL: internal address of the registered sub-system, only reachable from the portal
//     host (e.g. http://127.0.0.1:8081). Used to render the nginx upstream map.
//   - PathPrefix:  sub-system path prefix under the portal (e.g. /contract); when set, the
//     portal gateway strips/forwards it to UpstreamURL.
type Environment struct {
	ID            string
	TenantID      string
	ApplicationID string
	Environment   string
	BaseURL       *string
	UpstreamURL   *string
	PathPrefix    *string
	IssuerAlias   *string
	Metadata      json.RawMessage
	Status        string
	Version       uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ApplicationCreateInput contains writable fields for a new application registration.
type ApplicationCreateInput struct {
	TenantID        string
	OperatorID      string
	Code            string
	Name            string
	ApplicationType string
	OwnerOrgID      *string
	OwnerUserID     *string
	HomepageURL     *string
	Description     *string
	Status          string
}

// ApplicationUpdateInput updates an application with optimistic locking. The stable code is not
// writable: it is used by permissions, audit provenance and configuration namespaces.
type ApplicationUpdateInput struct {
	TenantID        string
	OperatorID      string
	ApplicationID   string
	Name            string
	ApplicationType string
	OwnerOrgID      *string
	OwnerUserID     *string
	HomepageURL     *string
	Description     *string
	Status          string
	Version         uint64
}

// ApplicationDeleteInput retires an application registration after the caller confirms its
// stable code. The operation is intentionally logical: related environments, OAuth clients,
// login targets and audit history remain available for traceability.
type ApplicationDeleteInput struct {
	TenantID         string
	OperatorID       string
	ApplicationID    string
	ConfirmationCode string
	Version          uint64
}

// EnvironmentCreateInput contains writable fields for one deployment environment.
type EnvironmentCreateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	Environment   string
	BaseURL       *string
	UpstreamURL   *string
	PathPrefix    *string
	IssuerAlias   *string
	Metadata      json.RawMessage
	Status        string
}

// EnvironmentUpdateInput updates an environment with optimistic locking.
type EnvironmentUpdateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	EnvironmentID string
	BaseURL       *string
	UpstreamURL   *string
	PathPrefix    *string
	IssuerAlias   *string
	Metadata      json.RawMessage
	Status        string
	Version       uint64
}

// EnvironmentDeleteInput physically removes one non-development deployment environment and its
// derived login-target/OAuth integration records after explicit scoped confirmation.
type EnvironmentDeleteInput struct {
	TenantID         string
	OperatorID       string
	ApplicationID    string
	EnvironmentID    string
	ConfirmationCode string
	Version          uint64
}

// ManagementRepository owns tenant-scoped persistence for application and environment
// management. It does not expose OAuth client or credential mutation operations.
type ManagementRepository interface {
	ListApplications(context.Context, string, PageRequest) (PageResult[Application], error)
	CreateApplication(context.Context, ApplicationCreateInput, string, time.Time) (Application, error)
	GetApplication(context.Context, string, string) (Application, error)
	UpdateApplication(context.Context, ApplicationUpdateInput, time.Time) (Application, error)

	ListEnvironments(context.Context, string, string, PageRequest) (PageResult[Environment], error)
	CreateEnvironment(context.Context, EnvironmentCreateInput, string, time.Time) (Environment, error)
	GetEnvironment(context.Context, string, string, string) (Environment, error)
	UpdateEnvironment(context.Context, EnvironmentUpdateInput, time.Time) (Environment, error)
	DeleteEnvironment(context.Context, EnvironmentDeleteInput) (Environment, error)
}

// ManagementService coordinates controlled, tenant-isolated application and environment changes.
type ManagementService struct {
	repository ManagementRepository
	ids        IdentifierGenerator
	clock      Clock
}

// NewManagementService constructs the application/environment management use cases.
func NewManagementService(repository ManagementRepository, ids IdentifierGenerator, clock Clock) (*ManagementService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("application registry management dependencies must not be nil")
	}
	return &ManagementService{repository: repository, ids: ids, clock: clock}, nil
}

// ListApplications returns applications visible within the authenticated tenant.
func (service *ManagementService) ListApplications(ctx context.Context, tenantID string, query PageRequest) (PageResult[Application], error) {
	tenantID = strings.TrimSpace(tenantID)
	query = normalizePageRequest(query)
	if tenantID == "" || !validApplicationStatusFilter(query.Status) {
		return PageResult[Application]{}, ErrValidation
	}
	return service.repository.ListApplications(ctx, tenantID, query)
}

// CreateApplication registers a tenant-scoped business system with a stable code.
func (service *ManagementService) CreateApplication(ctx context.Context, input ApplicationCreateInput) (Application, error) {
	input = normalizeApplicationCreate(input)
	if !validApplicationCreate(input) {
		return Application{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	applicationID, err := service.ids.New(now)
	if err != nil {
		return Application{}, fmt.Errorf("generate application ID: %w", err)
	}
	return service.repository.CreateApplication(ctx, input, applicationID, now)
}

// GetApplication returns one application scoped to the tenant.
func (service *ManagementService) GetApplication(ctx context.Context, tenantID, applicationID string) (Application, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	if tenantID == "" || applicationID == "" {
		return Application{}, ErrValidation
	}
	return service.repository.GetApplication(ctx, tenantID, applicationID)
}

// UpdateApplication changes mutable registration details while preserving the stable code.
func (service *ManagementService) UpdateApplication(ctx context.Context, input ApplicationUpdateInput) (Application, error) {
	input = normalizeApplicationUpdate(input)
	if !validApplicationUpdate(input) {
		return Application{}, ErrValidation
	}
	return service.repository.UpdateApplication(ctx, input, service.clock.Now().UTC())
}

// DeleteApplication logically deletes one application registration by moving it to RETIRED.
// Physical deletion is deliberately avoided because environments, OAuth clients, login targets
// and audit records may still reference the stable application identity.
func (service *ManagementService) DeleteApplication(ctx context.Context, input ApplicationDeleteInput) (Application, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.ConfirmationCode = strings.TrimSpace(input.ConfirmationCode)
	if input.TenantID == "" || input.OperatorID == "" || input.ApplicationID == "" || input.ConfirmationCode == "" || input.Version == 0 {
		return Application{}, ErrValidation
	}

	current, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Application{}, err
	}
	if current.Code == builtInApplicationCode {
		return Application{}, ErrConflict
	}
	if input.ConfirmationCode != current.Code {
		return Application{}, ErrValidation
	}
	if input.Version != current.Version {
		return Application{}, ErrVersionConflict
	}
	if current.Status == "RETIRED" {
		return current, nil
	}

	return service.repository.UpdateApplication(ctx, ApplicationUpdateInput{
		TenantID:        current.TenantID,
		OperatorID:      input.OperatorID,
		ApplicationID:   current.ID,
		Name:            current.Name,
		ApplicationType: current.ApplicationType,
		OwnerOrgID:      current.OwnerOrgID,
		OwnerUserID:     current.OwnerUserID,
		HomepageURL:     current.HomepageURL,
		Description:     current.Description,
		Status:          "RETIRED",
		Version:         input.Version,
	}, service.clock.Now().UTC())
}

// ListEnvironments returns the environments registered beneath one tenant-scoped application.
func (service *ManagementService) ListEnvironments(ctx context.Context, tenantID, applicationID string, query PageRequest) (PageResult[Environment], error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	query = normalizePageRequest(query)
	if tenantID == "" || applicationID == "" || !validEnvironmentStatusFilter(query.Status) {
		return PageResult[Environment]{}, ErrValidation
	}
	if _, err := service.repository.GetApplication(ctx, tenantID, applicationID); err != nil {
		return PageResult[Environment]{}, err
	}
	return service.repository.ListEnvironments(ctx, tenantID, applicationID, query)
}

// CreateEnvironment registers an isolated deployment environment. Retired and suspended
// applications cannot gain new environments; DRAFT applications can be prepared before launch.
func (service *ManagementService) CreateEnvironment(ctx context.Context, input EnvironmentCreateInput) (Environment, error) {
	input = normalizeEnvironmentCreate(input)
	if !validEnvironmentCreate(input) {
		return Environment{}, ErrValidation
	}

	parent, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Environment{}, err
	}
	if parent.Status != "DRAFT" && parent.Status != "ACTIVE" {
		return Environment{}, ErrConflict
	}

	now := service.clock.Now().UTC()
	environmentID, err := service.ids.New(now)
	if err != nil {
		return Environment{}, fmt.Errorf("generate application environment ID: %w", err)
	}
	return service.repository.CreateEnvironment(ctx, input, environmentID, now)
}

// GetEnvironment returns a single tenant/application-scoped environment.
func (service *ManagementService) GetEnvironment(ctx context.Context, tenantID, applicationID, environmentID string) (Environment, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	environmentID = strings.TrimSpace(environmentID)
	if tenantID == "" || applicationID == "" || environmentID == "" {
		return Environment{}, ErrValidation
	}
	return service.repository.GetEnvironment(ctx, tenantID, applicationID, environmentID)
}

// UpdateEnvironment changes deployment metadata and lifecycle state using optimistic locking.
func (service *ManagementService) UpdateEnvironment(ctx context.Context, input EnvironmentUpdateInput) (Environment, error) {
	input = normalizeEnvironmentUpdate(input)
	if !validEnvironmentUpdate(input) {
		return Environment{}, ErrValidation
	}
	return service.repository.UpdateEnvironment(ctx, input, service.clock.Now().UTC())
}

// DeleteEnvironment removes one non-development environment while preserving its parent
// application and every other environment. The repository atomically removes only derived
// integration records and rejects deletion when configuration or audit evidence exists.
func (service *ManagementService) DeleteEnvironment(ctx context.Context, input EnvironmentDeleteInput) (Environment, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.ConfirmationCode = strings.TrimSpace(input.ConfirmationCode)
	if input.TenantID == "" || input.OperatorID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || input.ConfirmationCode == "" || input.Version == 0 {
		return Environment{}, ErrValidation
	}

	registeredApplication, err := service.repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
	if err != nil {
		return Environment{}, err
	}
	environment, err := service.repository.GetEnvironment(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
	if err != nil {
		return Environment{}, err
	}
	if registeredApplication.Code == builtInApplicationCode || environment.Environment == "dev" {
		return Environment{}, ErrConflict
	}
	if input.ConfirmationCode != registeredApplication.Code+"/"+environment.Environment {
		return Environment{}, ErrValidation
	}
	if input.Version != environment.Version {
		return Environment{}, ErrVersionConflict
	}
	return service.repository.DeleteEnvironment(ctx, input)
}

func normalizePageRequest(query PageRequest) PageRequest {
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.Status = strings.ToUpper(strings.TrimSpace(query.Status))
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = defaultManagementPageSize
	}
	if query.PageSize > maxManagementPageSize {
		query.PageSize = maxManagementPageSize
	}
	return query
}

func normalizeApplicationCreate(input ApplicationCreateInput) ApplicationCreateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.ApplicationType = strings.ToLower(strings.TrimSpace(input.ApplicationType))
	input.OwnerOrgID = normalizeOptional(input.OwnerOrgID)
	input.OwnerUserID = normalizeOptional(input.OwnerUserID)
	input.HomepageURL = normalizeOptional(input.HomepageURL)
	input.Description = normalizeOptional(input.Description)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeApplicationUpdate(input ApplicationUpdateInput) ApplicationUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.Name = strings.TrimSpace(input.Name)
	input.ApplicationType = strings.ToLower(strings.TrimSpace(input.ApplicationType))
	input.OwnerOrgID = normalizeOptional(input.OwnerOrgID)
	input.OwnerUserID = normalizeOptional(input.OwnerUserID)
	input.HomepageURL = normalizeOptional(input.HomepageURL)
	input.Description = normalizeOptional(input.Description)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeEnvironmentCreate(input EnvironmentCreateInput) EnvironmentCreateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	input.BaseURL = normalizeOptionalBaseURL(input.BaseURL)
	input.UpstreamURL = normalizeOptionalUpstreamURL(input.UpstreamURL)
	input.PathPrefix = normalizeOptionalPathPrefix(input.PathPrefix)
	input.IssuerAlias = normalizeOptional(input.IssuerAlias)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeEnvironmentUpdate(input EnvironmentUpdateInput) EnvironmentUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.BaseURL = normalizeOptionalBaseURL(input.BaseURL)
	input.UpstreamURL = normalizeOptionalUpstreamURL(input.UpstreamURL)
	input.PathPrefix = normalizeOptionalPathPrefix(input.PathPrefix)
	input.IssuerAlias = normalizeOptional(input.IssuerAlias)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func validApplicationCreate(input ApplicationCreateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && validCode(input.Code, 64) &&
		validManagementText(input.Name, 128, false) && validApplicationType(input.ApplicationType) &&
		validOptionalIdentifier(input.OwnerOrgID) && validOptionalIdentifier(input.OwnerUserID) &&
		validOptionalURL(input.HomepageURL) && validOptionalText(input.Description, 1000) &&
		validApplicationStatus(input.Status)
}

func validApplicationUpdate(input ApplicationUpdateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.ApplicationID != "" && input.Version > 0 &&
		validManagementText(input.Name, 128, false) && validApplicationType(input.ApplicationType) &&
		validOptionalIdentifier(input.OwnerOrgID) && validOptionalIdentifier(input.OwnerUserID) &&
		validOptionalURL(input.HomepageURL) && validOptionalText(input.Description, 1000) &&
		validApplicationStatus(input.Status)
}

func validEnvironmentCreate(input EnvironmentCreateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.ApplicationID != "" &&
		validEnvironmentCode(input.Environment) && validOptionalBaseURL(input.BaseURL) &&
		validOptionalUpstreamURL(input.UpstreamURL) && validOptionalPathPrefix(input.PathPrefix) &&
		validGatewayTripleConsistent(input.BaseURL, input.UpstreamURL, input.PathPrefix) &&
		validOptionalCode(input.IssuerAlias, 128) && validMetadata(input.Metadata) && validEnvironmentStatus(input.Status)
}

func validEnvironmentUpdate(input EnvironmentUpdateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.ApplicationID != "" && input.EnvironmentID != "" && input.Version > 0 &&
		validOptionalBaseURL(input.BaseURL) && validOptionalUpstreamURL(input.UpstreamURL) &&
		validOptionalPathPrefix(input.PathPrefix) &&
		validGatewayTripleConsistent(input.BaseURL, input.UpstreamURL, input.PathPrefix) &&
		validOptionalCode(input.IssuerAlias, 128) && validMetadata(input.Metadata) && validEnvironmentStatus(input.Status)
}

func validApplicationStatus(value string) bool {
	switch value {
	case "DRAFT", "ACTIVE", "SUSPENDED", "RETIRED":
		return true
	default:
		return false
	}
}

func validApplicationStatusFilter(value string) bool {
	return value == "" || validApplicationStatus(value)
}

func validEnvironmentStatus(value string) bool { return value == "ACTIVE" || value == "DISABLED" }

func validEnvironmentStatusFilter(value string) bool {
	return value == "" || validEnvironmentStatus(value)
}

func validApplicationType(value string) bool {
	switch value {
	case "spa", "web", "backend", "mobile", "third_party":
		return true
	default:
		return false
	}
}

func validEnvironmentCode(value string) bool {
	switch value {
	case "dev", "test", "staging", "prod":
		return true
	default:
		return false
	}
}

func validCode(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (index > 0 && character >= '0' && character <= '9') ||
			(index > 0 && (character == '-' || character == '_' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func validOptionalCode(value *string, limit int) bool {
	return value == nil || validCode(*value, limit)
}

func validOptionalIdentifier(value *string) bool {
	return value == nil || (len(*value) == 26 && validIdentifier(*value))
}

func validIdentifier(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'A' && character <= 'Z')) {
			return false
		}
	}
	return true
}

func validManagementText(value string, limit int, allowEmpty bool) bool {
	return len(value) <= limit && (allowEmpty || value != "")
}

func validOptionalText(value *string, limit int) bool {
	return value == nil || validManagementText(*value, limit, true)
}

func validOptionalURL(value *string) bool {
	if value == nil {
		return true
	}
	if len(*value) > 512 {
		return false
	}
	parsed, err := url.ParseRequestURI(*value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// validOptionalBaseURL validates the public portal address used to compose browser redirects.
// Unlike a general homepage URL it must not contain credentials, a query or a fragment.
func validOptionalBaseURL(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	if len(*value) > 512 || strings.Contains(*value, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI(*value)
	return err == nil && parsed.Host != "" && parsed.User == nil && parsed.Opaque == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeOptionalBaseURL(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	normalized = strings.TrimRight(normalized, "/")
	return &normalized
}

// validOptionalUpstreamURL is the strictly-internal counterpart to validOptionalURL. It is used
// for the reverse-proxy target only and may point at private/loopback addresses, so it does not
// share the public-suffix concerns of the public BaseURL.
func validOptionalUpstreamURL(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	if len(*value) > 512 {
		return false
	}
	matches := safeUpstreamURLPattern.FindStringSubmatch(*value)
	if matches == nil {
		return false
	}
	if matches[3] != "" {
		port, err := strconv.Atoi(matches[3])
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	parsed, err := url.ParseRequestURI(*value)
	return err == nil && parsed.Host != "" && parsed.User == nil && parsed.Opaque == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeOptionalUpstreamURL(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	normalized = strings.TrimRight(normalized, "/")
	return &normalized
}

// validOptionalPathPrefix accepts an absolute path under the portal root. The trailing slash is
// optional; // (protocol-relative), \\ (windows) and any :// (scheme) are rejected.
func validOptionalPathPrefix(value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	// The portal root is reserved for the portal UI and cannot be assigned to a sub-system.
	return *value != "/" && validPortalPath(*value, 128)
}

// validPortalPath accepts one single-rooted, query-free path using the same conservative
// character set as the nginx gateway renderer. Encoded characters and traversal are rejected.
func validPortalPath(value string, limit int) bool {
	if value == "" || len(value) > limit || !strings.HasPrefix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	// Keep this character set aligned with scripts/portal-gateway.sh. Besides avoiding nginx
	// configuration metacharacters, rejecting percent-encoding prevents browser/nginx double-
	// decoding disagreements before a LoginTarget is joined to the public BaseURL.
	for _, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("/._~!+-", rune(character)) {
			continue
		}
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Path != "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeOptionalPathPrefix(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	normalized = strings.TrimRight(normalized, "/")
	if normalized == "" {
		return nil
	}
	return &normalized
}

// validGatewayTripleConsistent enforces a coarse invariant for the gateway fields:
//   - path_prefix only makes sense when paired with a BaseURL (relative LoginTarget.TargetURI is
//     resolved against BaseURL), so the presence of path_prefix implies BaseURL is set.
//   - upstream_url + path_prefix describe a single reverse-proxy entry, so they must be set
//     together (or both left empty when the environment is only used as a logical boundary).
func validGatewayTripleConsistent(baseURL, upstreamURL, pathPrefix *string) bool {
	hasBaseURL := baseURL != nil && *baseURL != ""
	hasUpstream := upstreamURL != nil && *upstreamURL != ""
	hasPrefix := pathPrefix != nil && *pathPrefix != ""
	if hasPrefix && !hasBaseURL {
		return false
	}
	if hasUpstream != hasPrefix {
		return false
	}
	return true
}

func validMetadata(value json.RawMessage) bool {
	if len(value) == 0 {
		return true
	}
	if len(value) > maxMetadataBytes || !json.Valid(value) {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return false
	}
	return !containsSensitiveMetadataKey(object)
}

func containsSensitiveMetadataKey(object map[string]json.RawMessage) bool {
	for key, value := range object {
		if sensitiveMetadataKey(key) {
			return true
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) == nil && nested != nil && containsSensitiveMetadataKey(nested) {
			return true
		}
	}
	return false
}

func sensitiveMetadataKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, fragment := range []string{"secret", "password", "private_key", "access_token", "refresh_token", "api_key"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func normalizeOptional(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	return &normalized
}
