// Package application 实现租户级业务字典的用例层：字典与字典项的增删改查、
// 乐观锁更新与运行时启用项查询。租户边界由调用方传入的 tenantID 与仓储层
// 查询条件共同保证。
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
	// ErrNotFound 表示请求的字典资源在当前租户内不存在。
	ErrNotFound = errors.New("dictionary resource not found")
	// ErrConflict 表示编码唯一约束冲突或生命周期冲突（如重复编码）。
	ErrConflict = errors.New("dictionary resource conflict")
	// ErrVersionConflict 表示乐观锁版本已过期：数据被其他管理员先修改。
	ErrVersionConflict = errors.New("dictionary resource version conflict")
	// ErrValidation 表示输入不满足业务约束；消息为可安全展示给管理界面的静态原因。
	ErrValidation = errors.New("invalid dictionary input")
)

// IdentifierGenerator 生成全局唯一、按时间可排序的记录标识（ULID），
// 用于新建字典与字典项的主键。
type IdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// Clock 提供当前时间；测试注入确定性时钟使乐观锁与有效期断言可复现。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产环境时钟。
type SystemClock struct{}

// Now 返回当前墙钟时间（本地时区，由仓储层统一转 UTC 落库）。
func (SystemClock) Now() time.Time {
	return time.Now()
}

// PageRequest 是字典/字典项列表的分页与过滤参数；零值由服务层归一化为
// 第 1 页、每页 20 条（上限 100）。
type PageRequest struct {
	Page     int
	PageSize int
	// Keyword 模糊匹配字典编码/名称或字典项编码/名称/值；空串表示不过滤。
	Keyword string
	// Status 过滤启用/停用（ACTIVE/DISABLED）；空串表示全部。
	Status string
}

// PageResult 是可复用的分页响应值。
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// DictionaryCreateInput 创建一个租户级字典类型。编码在租户内唯一，
// 是业务表单按编码消费启用项的稳定契约。
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

// ItemCreateInput 在指定字典下创建一个字典项。value 是业务数据实际保存的值，
// label 仅为显示名；编码在字典内唯一。
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

// ItemUpdateInput 以乐观锁更新字典项；Version 过期返回 ErrVersionConflict，
// 防止旧页面静默覆盖其他管理员的修改。
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

// DictionaryDeleteInput 删除一个字典及其名下全部字典项（物理删除、单事务级联）。
type DictionaryDeleteInput struct {
	TenantID     string
	DictionaryID string
	OperatorID   string
}

// ItemDeleteInput 删除租户自有字典下的单个字典项（物理删除）。
type ItemDeleteInput struct {
	TenantID     string
	DictionaryID string
	ItemID       string
	OperatorID   string
}

// Repository 持久化字典与字典项。实现必须保证：所有查询以 tenantID 为边界；
// 删除字典前先删除其字典项（外键为 RESTRICT）；唯一冲突映射为 ErrConflict。
type Repository interface {
	ListDictionaries(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Dictionary], error)
	CreateDictionary(ctx context.Context, input DictionaryCreateInput, dictionaryID string, now time.Time) (domain.Dictionary, error)
	GetDictionary(ctx context.Context, tenantID, dictionaryID string) (domain.Dictionary, error)
	UpdateDictionary(ctx context.Context, input DictionaryUpdateInput, now time.Time) (domain.Dictionary, error)
	DeleteDictionary(ctx context.Context, input DictionaryDeleteInput) (int64, error)
	ListItems(ctx context.Context, tenantID, dictionaryID string, query PageRequest, activeOnly bool) (PageResult[domain.Item], error)
	CreateItem(ctx context.Context, input ItemCreateInput, itemID string, now time.Time) (domain.Item, error)
	UpdateItem(ctx context.Context, input ItemUpdateInput, now time.Time) (domain.Item, error)
	DeleteItem(ctx context.Context, input ItemDeleteInput) error
	GetDictionaryByCode(ctx context.Context, tenantID, code string) (domain.Dictionary, error)
}

// Service 实现租户隔离的字典操作：所有查询与写入以入参 tenantID 为边界，
// 输入先归一化（trim、大小写）再校验。
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService 构造字典应用服务；任一依赖为 nil 时返回错误，不允许半初始化实例。
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("dictionary service dependencies must not be nil")
	}

	return &Service{repository: repository, ids: ids, clock: clock}, nil
}

// ListDictionaries 分页列出当前租户可见的字典（含各字典的字典项计数）。
func (service *Service) ListDictionaries(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Dictionary], error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return PageResult[domain.Dictionary]{}, ErrValidation
	}

	return service.repository.ListDictionaries(ctx, tenantID, normalizePage(query))
}

// CreateDictionary 创建一个租户内编码唯一的字典；编码冲突映射为 ErrConflict。
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

// GetDictionary 返回租户内单个字典（含字典项计数）；不存在返回 ErrNotFound。
func (service *Service) GetDictionary(ctx context.Context, tenantID, dictionaryID string) (domain.Dictionary, error) {
	tenantID = strings.TrimSpace(tenantID)
	dictionaryID = strings.TrimSpace(dictionaryID)
	if tenantID == "" || dictionaryID == "" {
		return domain.Dictionary{}, ErrValidation
	}

	return service.repository.GetDictionary(ctx, tenantID, dictionaryID)
}

// UpdateDictionary 以乐观锁更新字典；Version 为 0 视为未基于读取快照提交，
// 直接拒绝（ErrValidation），过期版本返回 ErrVersionConflict。
func (service *Service) UpdateDictionary(ctx context.Context, input DictionaryUpdateInput) (domain.Dictionary, error) {
	input = normalizeUpdateDictionary(input)
	if input.TenantID == "" || input.DictionaryID == "" || input.OperatorID == "" || input.Version == 0 ||
		!validDictionary(input.Code, input.Name, input.Description, input.Status) {
		return domain.Dictionary{}, ErrValidation
	}

	return service.repository.UpdateDictionary(ctx, input, service.clock.Now().UTC())
}

// ListItems 分页列出指定字典下全部状态的字典项（含停用项）：管理端需要看到
// 停用项以便审计与恢复；业务侧应使用 ListActiveItemsByCode。
func (service *Service) ListItems(ctx context.Context, tenantID, dictionaryID string, query PageRequest) (PageResult[domain.Item], error) {
	tenantID = strings.TrimSpace(tenantID)
	dictionaryID = strings.TrimSpace(dictionaryID)
	if tenantID == "" || dictionaryID == "" {
		return PageResult[domain.Item]{}, ErrValidation
	}

	return service.repository.ListItems(ctx, tenantID, dictionaryID, normalizePage(query), false)
}

// CreateItem 在指定字典下创建编码唯一的字典项；父字典归属在仓储层事务内核验，
// 防止跨租户的 dictionary_id 注入。
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

// UpdateItem 以乐观锁更新字典项；Version 过期返回 ErrVersionConflict。
// 锁定条件同时包含项、字典与租户，防止移动子项或跨租户更新。
func (service *Service) UpdateItem(ctx context.Context, input ItemUpdateInput) (domain.Item, error) {
	input = normalizeUpdateItem(input)
	if input.TenantID == "" || input.DictionaryID == "" || input.ItemID == "" || input.OperatorID == "" || input.Version == 0 ||
		!validItem(input.Code, input.Label, input.Value, input.Status) {
		return domain.Item{}, ErrValidation
	}

	return service.repository.UpdateItem(ctx, input, service.clock.Now().UTC())
}

// DeleteDictionary 删除租户自有字典并级联删除其全部字典项，单事务完成，
// 返回级联删除的字典项数量。操作物理且不可恢复；已发布到业务的取值应以停用替代。
func (service *Service) DeleteDictionary(ctx context.Context, input DictionaryDeleteInput) (int64, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.DictionaryID = strings.TrimSpace(input.DictionaryID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.DictionaryID == "" || input.OperatorID == "" {
		return 0, ErrValidation
	}

	return service.repository.DeleteDictionary(ctx, input)
}

// DeleteItem 删除租户自有字典下的单个字典项；已发布到业务的取值建议停用而非删除。
func (service *Service) DeleteItem(ctx context.Context, input ItemDeleteInput) error {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.DictionaryID = strings.TrimSpace(input.DictionaryID)
	input.ItemID = strings.TrimSpace(input.ItemID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.DictionaryID == "" || input.ItemID == "" || input.OperatorID == "" {
		return ErrValidation
	}

	return service.repository.DeleteItem(ctx, input)
}

// ListActiveItemsByCode 按字典编码返回启用字典的启用字典项，供业务表单的只读
// 选择控件消费。父字典或字典项任一停用即不返回；管理端列表（ListItems）不受此限，
// 保留停用项以便审计与恢复。
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
