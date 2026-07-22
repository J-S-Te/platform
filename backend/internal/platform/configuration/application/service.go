// Package application coordinates configuration namespace, draft, and release use cases.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/domain"
)

var (
	// ErrNotFound indicates that the requested configuration resource is not visible to the tenant.
	ErrNotFound = errors.New("configuration resource not found")
	// ErrConflict indicates that a unique configuration resource already exists.
	ErrConflict = errors.New("configuration resource conflict")
	// ErrVersionConflict indicates that a draft has changed since the caller read it.
	ErrVersionConflict = errors.New("configuration version conflict")
	// ErrValidation indicates that request data cannot be stored safely.
	ErrValidation = errors.New("configuration validation failed")
)

// IdentifierGenerator provides ULID-compatible identifiers without coupling the service to infrastructure.
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock makes creation timestamps deterministic in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production UTC clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

// PageRequest contains supported list filters and paging information.
type PageRequest struct {
	Page        int
	PageSize    int
	Keyword     string
	NamespaceID string
}

// PageResult wraps a tenant-scoped paginated result set.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// NamespaceCreateInput is the data needed to create an application configuration namespace.
type NamespaceCreateInput struct {
	TenantID        string
	OperatorID      string
	ApplicationCode string
	Code            string
	Name            string
	Description     string
}

// ItemCreateInput is the data needed to create a draft configuration item.
type ItemCreateInput struct {
	TenantID    string
	OperatorID  string
	NamespaceID string
	Key         string
	ValueType   string
	Value       any
	Secret      bool
}

// ItemUpdateInput is the data needed to replace one draft configuration item.
type ItemUpdateInput struct {
	TenantID    string
	OperatorID  string
	ItemID      string
	NamespaceID string
	Key         string
	ValueType   string
	Value       any
	Secret      bool
	Version     uint64
}

// ReleaseCreateInput describes an immutable release snapshot selected from draft item versions.
type ReleaseCreateInput struct {
	TenantID     string
	OperatorID   string
	NamespaceID  string
	Comment      string
	ItemVersions []domain.VersionedItem
}

// Repository persists configuration aggregates. Implementations must always enforce tenant scope.
type Repository interface {
	ListNamespaces(context.Context, string, PageRequest) (PageResult[domain.Namespace], error)
	CreateNamespace(context.Context, NamespaceCreateInput, string, time.Time) (domain.Namespace, error)
	ListItems(context.Context, string, PageRequest) (PageResult[domain.Item], error)
	CreateItem(context.Context, ItemCreateInput, string, time.Time) (domain.Item, error)
	UpdateItem(context.Context, ItemUpdateInput, time.Time) (domain.Item, error)
	CreateRelease(context.Context, ReleaseCreateInput, string, time.Time) (domain.Release, error)
	GetRelease(context.Context, string, string) (domain.Release, error)
	GetPublished(context.Context, string, string, string) (domain.PublishedConfig, error)
}

// Service applies configuration lifecycle rules before handing data to a repository.
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService constructs a configuration application service.
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("configuration service dependencies must not be nil")
	}

	return &Service{
		repository: repository,
		ids:        ids,
		clock:      clock,
	}, nil
}

// ListNamespaces returns configuration namespaces visible to one tenant.
func (s *Service) ListNamespaces(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Namespace], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.Namespace]{}, ErrValidation
	}

	return s.repository.ListNamespaces(ctx, tenantID, normalizePage(query))
}

// CreateNamespace creates an active namespace for an active application in the development environment.
func (s *Service) CreateNamespace(ctx context.Context, input NamespaceCreateInput) (domain.Namespace, error) {
	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OperatorID) == "" ||
		!validCode(input.ApplicationCode) ||
		!validCode(input.Code) ||
		strings.TrimSpace(input.Name) == "" {
		return domain.Namespace{}, ErrValidation
	}

	now := s.clock.Now()
	id, err := s.ids.New(now)
	if err != nil {
		return domain.Namespace{}, err
	}

	return s.repository.CreateNamespace(ctx, normalizeNamespace(input), id, now)
}

// ListItems returns draft configuration items visible to one tenant.
func (s *Service) ListItems(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Item], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.Item]{}, ErrValidation
	}

	return s.repository.ListItems(ctx, tenantID, normalizePage(query))
}

// CreateItem creates an editable draft item. Plaintext secrets are deliberately unsupported.
func (s *Service) CreateItem(ctx context.Context, input ItemCreateInput) (domain.Item, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" {
		return domain.Item{}, ErrValidation
	}
	if err := validateItem(input.NamespaceID, input.Key, input.ValueType, input.Value, input.Secret); err != nil {
		return domain.Item{}, ErrValidation
	}

	now := s.clock.Now()
	id, err := s.ids.New(now)
	if err != nil {
		return domain.Item{}, err
	}

	return s.repository.CreateItem(ctx, normalizeCreateItem(input), id, now)
}

// UpdateItem replaces an editable draft item when its optimistic-lock version still matches.
func (s *Service) UpdateItem(ctx context.Context, input ItemUpdateInput) (domain.Item, error) {
	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OperatorID) == "" ||
		strings.TrimSpace(input.ItemID) == "" ||
		input.Version == 0 {
		return domain.Item{}, ErrValidation
	}
	if err := validateItem(input.NamespaceID, input.Key, input.ValueType, input.Value, input.Secret); err != nil {
		return domain.Item{}, ErrValidation
	}

	return s.repository.UpdateItem(ctx, normalizeUpdateItem(input), s.clock.Now())
}

// CreateRelease creates an immutable published snapshot of the selected draft versions.
func (s *Service) CreateRelease(ctx context.Context, input ReleaseCreateInput) (domain.Release, error) {
	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OperatorID) == "" ||
		strings.TrimSpace(input.NamespaceID) == "" ||
		len(input.ItemVersions) == 0 ||
		len(input.ItemVersions) > 500 {
		return domain.Release{}, ErrValidation
	}

	seen := make(map[string]struct{}, len(input.ItemVersions))
	for _, item := range input.ItemVersions {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" || item.Version == 0 {
			return domain.Release{}, ErrValidation
		}
		if _, exists := seen[itemID]; exists {
			return domain.Release{}, ErrValidation
		}
		seen[itemID] = struct{}{}
	}

	now := s.clock.Now()
	id, err := s.ids.New(now)
	if err != nil {
		return domain.Release{}, err
	}

	input.Comment = strings.TrimSpace(input.Comment)
	return s.repository.CreateRelease(ctx, input, id, now)
}

// GetRelease returns a published release visible to the tenant.
func (s *Service) GetRelease(ctx context.Context, tenantID, releaseID string) (domain.Release, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(releaseID) == "" {
		return domain.Release{}, ErrValidation
	}

	return s.repository.GetRelease(ctx, tenantID, strings.TrimSpace(releaseID))
}

// GetPublished returns the current published release values for runtime consumption.
func (s *Service) GetPublished(ctx context.Context, tenantID, applicationCode, namespaceCode string) (domain.PublishedConfig, error) {
	if strings.TrimSpace(tenantID) == "" || !validCode(applicationCode) || !validCode(namespaceCode) {
		return domain.PublishedConfig{}, ErrValidation
	}

	return s.repository.GetPublished(
		ctx,
		tenantID,
		strings.TrimSpace(applicationCode),
		strings.TrimSpace(namespaceCode),
	)
}

func normalizePage(query PageRequest) PageRequest {
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
	query.NamespaceID = strings.TrimSpace(query.NamespaceID)
	return query
}

func normalizeNamespace(input NamespaceCreateInput) NamespaceCreateInput {
	input.ApplicationCode = strings.TrimSpace(input.ApplicationCode)
	input.Code = strings.TrimSpace(input.Code)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	return input
}

func normalizeCreateItem(input ItemCreateInput) ItemCreateInput {
	input.NamespaceID = strings.TrimSpace(input.NamespaceID)
	input.Key = strings.TrimSpace(input.Key)
	input.ValueType = strings.ToUpper(strings.TrimSpace(input.ValueType))
	return input
}

func normalizeUpdateItem(input ItemUpdateInput) ItemUpdateInput {
	input.NamespaceID = strings.TrimSpace(input.NamespaceID)
	input.Key = strings.TrimSpace(input.Key)
	input.ValueType = strings.ToUpper(strings.TrimSpace(input.ValueType))
	return input
}

func validCode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}

	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}

	return true
}

func validateItem(namespaceID, key, valueType string, value any, secret bool) error {
	if strings.TrimSpace(namespaceID) == "" || !validCode(key) || value == nil || secret {
		return ErrValidation
	}

	switch strings.ToUpper(strings.TrimSpace(valueType)) {
	case "STRING":
		if _, ok := value.(string); !ok {
			return ErrValidation
		}
	case "NUMBER":
		if !isJSONNumber(value) {
			return ErrValidation
		}
	case "BOOLEAN":
		if _, ok := value.(bool); !ok {
			return ErrValidation
		}
	case "JSON":
		if _, err := json.Marshal(value); err != nil {
			return ErrValidation
		}
	default:
		return ErrValidation
	}

	return nil
}

func isJSONNumber(value any) bool {
	switch number := value.(type) {
	case json.Number:
		_, err := number.Float64()
		return err == nil
	case float64:
		return !math.IsNaN(number) && !math.IsInf(number, 0)
	case float32:
		return !math.IsNaN(float64(number)) && !math.IsInf(float64(number), 0)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}
