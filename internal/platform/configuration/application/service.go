// Package application 编排配置命名空间、草稿与发布的用例，统一执行输入校验与生命周期规则。
package application

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/domain"
)

var (
	// ErrNotFound 表示请求的配置资源在租户范围内不可见，不区分“不存在”与“无权”。
	ErrNotFound = errors.New("configuration resource not found")
	// ErrConflict 表示同名的配置资源已存在。
	ErrConflict = errors.New("configuration resource conflict")
	// ErrVersionConflict 表示草稿自调用方读取后已被他人修改，需重新读取后再提交。
	ErrVersionConflict = errors.New("configuration version conflict")
	// ErrValidation 表示请求数据无法被安全存储，未触达存储层。
	ErrValidation = errors.New("configuration validation failed")
)

// IdentifierGenerator 提供 ULID 兼容的标识符生成能力，使服务不与具体基础设施耦合。
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock 抽象时间源，便于在测试中获得确定性的创建时间戳。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产环境使用的 UTC 时钟实现。
type SystemClock struct{}

// Now 返回当前 UTC 时间，所有创建时间戳一律以 UTC 落库。
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

// PageRequest 承载列表查询支持的分页与过滤条件。
type PageRequest struct {
	Page        int
	PageSize    int
	Keyword     string
	NamespaceID string
}

// PageResult 封装租户范围内的分页结果集。
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// NamespaceCreateInput 是创建应用配置命名空间所需的输入数据。
type NamespaceCreateInput struct {
	TenantID        string
	OperatorID      string
	ApplicationCode string
	Code            string
	Name            string
	Description     string
}

// ItemCreateInput 是创建配置草稿项所需的输入数据。
type ItemCreateInput struct {
	TenantID    string
	OperatorID  string
	NamespaceID string
	Key         string
	ValueType   string
	Value       any
	Secret      bool
}

// ItemUpdateInput 是整体替换一个配置草稿项所需的输入数据，Version 用于乐观锁。
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

// ReleaseCreateInput 描述从草稿项版本中选取的不可变发布快照。
type ReleaseCreateInput struct {
	TenantID     string
	OperatorID   string
	NamespaceID  string
	Comment      string
	ItemVersions []domain.VersionedItem
}

// Repository 持久化配置聚合。实现必须始终强制租户范围过滤，不得依赖调用方传参保证数据边界。
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

// Service 在数据交给仓储前统一执行配置生命周期规则与输入规范化。
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService 构造配置应用服务；任一依赖为 nil 时失败关闭。
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

// ListNamespaces 返回对指定租户可见的配置命名空间。
func (s *Service) ListNamespaces(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Namespace], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.Namespace]{}, ErrValidation
	}

	return s.repository.ListNamespaces(ctx, tenantID, normalizePage(query))
}

// CreateNamespace 只接受稳定编码，并把“应用必须存在且 dev 环境有效”的事实交给仓储从可信数据中确认，
// 避免调用方用名称或客户端提交的环境标识伪造配置归属。
func (s *Service) CreateNamespace(ctx context.Context, input NamespaceCreateInput) (domain.Namespace, error) {
	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OperatorID) == "" ||
		!validCode(input.ApplicationCode) ||
		!validCode(input.Code) ||
		strings.TrimSpace(input.Name) == "" ||
		len([]rune(strings.TrimSpace(input.Name))) > 128 ||
		len([]rune(strings.TrimSpace(input.Description))) > 1000 {
		return domain.Namespace{}, ErrValidation
	}

	now := s.clock.Now()
	id, err := s.ids.New(now)
	if err != nil {
		return domain.Namespace{}, err
	}

	return s.repository.CreateNamespace(ctx, normalizeNamespace(input), id, now)
}

// ListItems 返回对指定租户可见的配置草稿项。
func (s *Service) ListItems(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Item], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.Item]{}, ErrValidation
	}

	return s.repository.ListItems(ctx, tenantID, normalizePage(query))
}

// CreateItem 创建可编辑草稿。Secret=true 被有意拒绝：本模块没有密钥托管能力，不能把“敏感”标签
// 当成加密措施后继续保存明文。
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

// UpdateItem 在乐观锁版本仍然匹配时整体替换可编辑草稿项，版本不匹配时返回冲突。
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

// CreateRelease 要求调用方明确提交每个草稿的版本，并在进入仓储前去重。这样一次发布对应一个
// 可重复校验的输入集合，不能通过重复 item 放大计数或制造含义不清的快照。
func (s *Service) CreateRelease(ctx context.Context, input ReleaseCreateInput) (domain.Release, error) {
	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OperatorID) == "" ||
		strings.TrimSpace(input.NamespaceID) == "" ||
		len(input.ItemVersions) == 0 ||
		len(input.ItemVersions) > 500 {
		return domain.Release{}, ErrValidation
	}

	if len([]rune(strings.TrimSpace(input.Comment))) > 1000 {
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

// GetRelease 返回对租户可见的已发布版本快照。
func (s *Service) GetRelease(ctx context.Context, tenantID, releaseID string) (domain.Release, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(releaseID) == "" {
		return domain.Release{}, ErrValidation
	}

	return s.repository.GetRelease(ctx, tenantID, strings.TrimSpace(releaseID))
}

// GetPublished 返回当前已发布版本的配置值，供运行时消费方拉取；仅暴露发布态数据，
// 草稿永远不出现在该接口结果中。
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
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
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
	// 配置值来自 JSON 解码后的动态类型；这里先收紧类型，再由仓储按同一 ValueType 编码，
	// 防止“声明为数字、实际保存字符串”导致不同客户端解释不一致。
	if strings.TrimSpace(namespaceID) == "" || !validCode(key) || value == nil || secret {
		return ErrValidation
	}

	switch strings.ToUpper(strings.TrimSpace(valueType)) {
	case "STRING":
		text, ok := value.(string)
		if !ok || len([]rune(text)) > 20000 {
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
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > 20000 {
			return ErrValidation
		}
	default:
		return ErrValidation
	}

	return nil
}

func isJSONNumber(value any) bool {
	// JSON 不定义 NaN 和无穷大；即便 Go 浮点可以表达，也不能让它们进入可发布快照。
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
