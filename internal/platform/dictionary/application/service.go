// Package application provides business dictionary use cases.
package application

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/domain"
)

var (
	// ErrNotFound indicates that the requested dictionary resource does not exist in the tenant.
	ErrNotFound = errors.New("dictionary resource not found")
	// ErrConflict indicates a unique-code or lifecycle conflict.
	ErrConflict = errors.New("dictionary resource conflict")
	// ErrVersionConflict indicates that an optimistic-lock version is stale.
	ErrVersionConflict = errors.New("dictionary resource version conflict")
	// ErrValidation indicates invalid application input.
	ErrValidation = errors.New("invalid dictionary input")
)

// IdentifierGenerator supplies sortable aggregate identifiers.
type IdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// Clock supplies the current time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time {
	return time.Now()
}

// PageRequest defines dictionary list pagination and filters.
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
	Status   string
}

// PageResult is a reusable paginated response value.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// DictionaryCreateInput creates one dictionary type.
type DictionaryCreateInput struct {
	TenantID    string
	OperatorID  string
	Code        string
	Name        string
	Description string
	Status      domain.Status
}

// DictionaryUpdateInput 通过 Version 更新字典；调用方必须基于刚读取的版本提交，旧页面不能静默覆盖新值。
type DictionaryUpdateInput struct {
	TenantID     string
	DictionaryID string
	OperatorID   string
	Code         string
	Name         string
	Description  string
	Status       domain.Status
	Version      uint64
}

// ItemCreateInput creates a dictionary item under a dictionary type.
type ItemCreateInput struct {
	TenantID     string
	DictionaryID string
	OperatorID   string
	Code         string
	Label        string
	Value        string
	SortOrder    uint
	Status       domain.Status
}

// ItemUpdateInput updates a dictionary item under optimistic locking.
type ItemUpdateInput struct {
	TenantID     string
	DictionaryID string
	ItemID       string
	OperatorID   string
	Code         string
	Label        string
	Value        string
	SortOrder    uint
	Status       domain.Status
	Version      uint64
}

// Repository persists dictionaries and their items.
type Repository interface {
	ListDictionaries(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Dictionary], error)
	CreateDictionary(ctx context.Context, input DictionaryCreateInput, dictionaryID string, now time.Time) (domain.Dictionary, error)
	GetDictionary(ctx context.Context, tenantID, dictionaryID string) (domain.Dictionary, error)
	UpdateDictionary(ctx context.Context, input DictionaryUpdateInput, now time.Time) (domain.Dictionary, error)
	ListItems(ctx context.Context, tenantID, dictionaryID string, query PageRequest, activeOnly bool) (PageResult[domain.Item], error)
	CreateItem(ctx context.Context, input ItemCreateInput, itemID string, now time.Time) (domain.Item, error)
	UpdateItem(ctx context.Context, input ItemUpdateInput, now time.Time) (domain.Item, error)
	GetDictionaryByCode(ctx context.Context, tenantID, code string) (domain.Dictionary, error)
}

// Service implements tenant-isolated dictionary operations.
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService creates a dictionary application service.
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("dictionary service dependencies must not be nil")
	}

	return &Service{repository: repository, ids: ids, clock: clock}, nil
}

// ListDictionaries lists dictionaries visible to one tenant.
func (service *Service) ListDictionaries(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Dictionary], error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return PageResult[domain.Dictionary]{}, ErrValidation
	}

	return service.repository.ListDictionaries(ctx, tenantID, normalizePage(query))
}

// CreateDictionary creates a dictionary code unique in the current tenant.
func (service *Service) CreateDictionary(ctx context.Context, input DictionaryCreateInput) (domain.Dictionary, error) {
	input = normalizeCreateDictionary(input)
	if input.TenantID == "" || input.OperatorID == "" || !validDictionary(input.Code, input.Name, input.Description, input.Status) {
		return domain.Dictionary{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	dictionaryID, err := service.ids.New(now)
	if err != nil {
		return domain.Dictionary{}, err
	}

	return service.repository.CreateDictionary(ctx, input, dictionaryID, now)
}

// GetDictionary returns a single tenant-owned dictionary.
func (service *Service) GetDictionary(ctx context.Context, tenantID, dictionaryID string) (domain.Dictionary, error) {
	tenantID = strings.TrimSpace(tenantID)
	dictionaryID = strings.TrimSpace(dictionaryID)
	if tenantID == "" || dictionaryID == "" {
		return domain.Dictionary{}, ErrValidation
	}

	return service.repository.GetDictionary(ctx, tenantID, dictionaryID)
}

// UpdateDictionary updates one dictionary with optimistic locking.
func (service *Service) UpdateDictionary(ctx context.Context, input DictionaryUpdateInput) (domain.Dictionary, error) {
	input = normalizeUpdateDictionary(input)
	if input.TenantID == "" || input.DictionaryID == "" || input.OperatorID == "" || input.Version == 0 ||
		!validDictionary(input.Code, input.Name, input.Description, input.Status) {
		return domain.Dictionary{}, ErrValidation
	}

	return service.repository.UpdateDictionary(ctx, input, service.clock.Now().UTC())
}

// ListItems lists tenant-owned items under an existing dictionary.
func (service *Service) ListItems(ctx context.Context, tenantID, dictionaryID string, query PageRequest) (PageResult[domain.Item], error) {
	tenantID = strings.TrimSpace(tenantID)
	dictionaryID = strings.TrimSpace(dictionaryID)
	if tenantID == "" || dictionaryID == "" {
		return PageResult[domain.Item]{}, ErrValidation
	}

	return service.repository.ListItems(ctx, tenantID, dictionaryID, normalizePage(query), false)
}

// CreateItem creates a dictionary item whose code is unique inside the dictionary.
func (service *Service) CreateItem(ctx context.Context, input ItemCreateInput) (domain.Item, error) {
	input = normalizeCreateItem(input)
	if input.TenantID == "" || input.DictionaryID == "" || input.OperatorID == "" ||
		!validItem(input.Code, input.Label, input.Value, input.Status) {
		return domain.Item{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	itemID, err := service.ids.New(now)
	if err != nil {
		return domain.Item{}, err
	}

	return service.repository.CreateItem(ctx, input, itemID, now)
}

// UpdateItem updates a dictionary item with optimistic locking.
func (service *Service) UpdateItem(ctx context.Context, input ItemUpdateInput) (domain.Item, error) {
	input = normalizeUpdateItem(input)
	if input.TenantID == "" || input.DictionaryID == "" || input.ItemID == "" || input.OperatorID == "" || input.Version == 0 ||
		!validItem(input.Code, input.Label, input.Value, input.Status) {
		return domain.Item{}, ErrValidation
	}

	return service.repository.UpdateItem(ctx, input, service.clock.Now().UTC())
}

// ListActiveItemsByCode returns only active items for an active dictionary. It is suitable for
// read-only business selection controls and intentionally does not expose disabled values.
func (service *Service) ListActiveItemsByCode(ctx context.Context, tenantID, code string, query PageRequest) (PageResult[domain.Item], error) {
	// 运行时选择接口同时要求父字典和子项为 ACTIVE；管理端列表则保留禁用项以便审计和恢复。
	tenantID = strings.TrimSpace(tenantID)
	code = strings.TrimSpace(code)
	if tenantID == "" || !validCode(code) {
		return PageResult[domain.Item]{}, ErrValidation
	}

	normalizedQuery := normalizePage(query)
	dictionary, err := service.repository.GetDictionaryByCode(ctx, tenantID, code)
	if err != nil {
		return PageResult[domain.Item]{}, err
	}
	if dictionary.Status != domain.StatusActive {
		return PageResult[domain.Item]{
			Items:    []domain.Item{},
			Page:     normalizedQuery.Page,
			PageSize: normalizedQuery.PageSize,
		}, nil
	}

	return service.repository.ListItems(ctx, tenantID, dictionary.ID, normalizedQuery, true)
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
	query.Status = strings.ToUpper(strings.TrimSpace(query.Status))

	return query
}

func normalizeCreateDictionary(input DictionaryCreateInput) DictionaryCreateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.TrimSpace(input.Code)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Status = normalizeStatus(input.Status)

	return input
}

func normalizeUpdateDictionary(input DictionaryUpdateInput) DictionaryUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.DictionaryID = strings.TrimSpace(input.DictionaryID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.TrimSpace(input.Code)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Status = normalizeStatus(input.Status)

	return input
}

func normalizeCreateItem(input ItemCreateInput) ItemCreateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.DictionaryID = strings.TrimSpace(input.DictionaryID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.TrimSpace(input.Code)
	input.Label = strings.TrimSpace(input.Label)
	input.Value = strings.TrimSpace(input.Value)
	input.Status = normalizeStatus(input.Status)

	return input
}

func normalizeUpdateItem(input ItemUpdateInput) ItemUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.DictionaryID = strings.TrimSpace(input.DictionaryID)
	input.ItemID = strings.TrimSpace(input.ItemID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.TrimSpace(input.Code)
	input.Label = strings.TrimSpace(input.Label)
	input.Value = strings.TrimSpace(input.Value)
	input.Status = normalizeStatus(input.Status)

	return input
}

func normalizeStatus(status domain.Status) domain.Status {
	return domain.Status(strings.ToUpper(strings.TrimSpace(string(status))))
}

func validDictionary(code, name, description string, status domain.Status) bool {
	return validCode(code) && name != "" && len(name) <= 100 && len(description) <= 500 && validStatus(status)
}

func validItem(code, label, value string, status domain.Status) bool {
	return validCode(code) && label != "" && len(label) <= 100 && value != "" && len(value) <= 255 && validStatus(status)
}

func validStatus(status domain.Status) bool {
	return status == domain.StatusActive || status == domain.StatusDisabled
}

func validCode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}

	for _, character := range value {
		if unicode.IsLower(character) || unicode.IsUpper(character) || unicode.IsDigit(character) ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}

	return true
}
