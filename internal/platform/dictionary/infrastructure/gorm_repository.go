// Package infrastructure provides GORM persistence for business dictionaries.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dictionaryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/application"
	dictionarydomain "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository persists dictionaries and dictionary items through GORM.
type Repository struct {
	database *gorm.DB
}

// NewRepository constructs a dictionary repository.
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("dictionary database must not be nil")
	}

	return &Repository{database: database}, nil
}

type dictionaryModel struct {
	ID          string    `gorm:"column:id;primaryKey"`
	TenantID    string    `gorm:"column:tenant_id"`
	Code        string    `gorm:"column:code"`
	Name        string    `gorm:"column:name"`
	Description string    `gorm:"column:description"`
	Status      string    `gorm:"column:status"`
	Version     uint64    `gorm:"column:version"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	CreatedBy   *string   `gorm:"column:created_by"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	UpdatedBy   *string   `gorm:"column:updated_by"`
}

func (dictionaryModel) TableName() string {
	return "dict_dictionary"
}

type itemModel struct {
	ID           string    `gorm:"column:id;primaryKey"`
	TenantID     string    `gorm:"column:tenant_id"`
	DictionaryID string    `gorm:"column:dictionary_id"`
	Code         string    `gorm:"column:code"`
	Label        string    `gorm:"column:label"`
	Value        string    `gorm:"column:item_value"`
	SortOrder    uint      `gorm:"column:sort_order"`
	Status       string    `gorm:"column:status"`
	Version      uint64    `gorm:"column:version"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	CreatedBy    *string   `gorm:"column:created_by"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
	UpdatedBy    *string   `gorm:"column:updated_by"`
}

func (itemModel) TableName() string {
	return "dict_item"
}

type dictionaryProjection struct {
	dictionaryModel
	ItemCount int64 `gorm:"column:item_count"`
}

// ListDictionaries lists tenant dictionaries with their aggregate item counts.
func (repository *Repository) ListDictionaries(
	ctx context.Context,
	tenantID string,
	query dictionaryapplication.PageRequest,
) (dictionaryapplication.PageResult[dictionarydomain.Dictionary], error) {
	base := repository.database.WithContext(ctx).
		Table("dict_dictionary AS dictionary").
		Where("dictionary.tenant_id = ?", tenantID)
	if query.Status != "" {
		base = base.Where("dictionary.status = ?", query.Status)
	}
	if query.Keyword != "" {
		keyword := "%" + query.Keyword + "%"
		base = base.Where("dictionary.code LIKE ? OR dictionary.name LIKE ?", keyword, keyword)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return dictionaryapplication.PageResult[dictionarydomain.Dictionary]{}, fmt.Errorf("count dictionaries: %w", err)
	}

	var rows []dictionaryProjection
	if err := base.
		Select("dictionary.*, COUNT(item.id) AS item_count").
		Joins("LEFT JOIN dict_item AS item ON item.dictionary_id = dictionary.id AND item.tenant_id = dictionary.tenant_id").
		Group("dictionary.id").
		Order("dictionary.code ASC").
		Offset((query.Page - 1) * query.PageSize).
		Limit(query.PageSize).
		Scan(&rows).Error; err != nil {
		return dictionaryapplication.PageResult[dictionarydomain.Dictionary]{}, fmt.Errorf("list dictionaries: %w", err)
	}

	items := make([]dictionarydomain.Dictionary, 0, len(rows))
	for _, row := range rows {
		items = append(items, dictionaryToDomain(row.dictionaryModel, row.ItemCount))
	}

	return dictionaryapplication.PageResult[dictionarydomain.Dictionary]{
		Items:    items,
		Page:     query.Page,
		PageSize: query.PageSize,
		Total:    total,
	}, nil
}

// CreateDictionary inserts a tenant-owned dictionary.
func (repository *Repository) CreateDictionary(
	ctx context.Context,
	input dictionaryapplication.DictionaryCreateInput,
	dictionaryID string,
	now time.Time,
) (dictionarydomain.Dictionary, error) {
	operatorID := input.OperatorID
	row := dictionaryModel{
		ID:          dictionaryID,
		TenantID:    input.TenantID,
		Code:        input.Code,
		Name:        input.Name,
		Description: input.Description,
		Status:      string(input.Status),
		Version:     1,
		CreatedAt:   now,
		CreatedBy:   &operatorID,
		UpdatedAt:   now,
		UpdatedBy:   &operatorID,
	}
	if err := repository.database.WithContext(ctx).Create(&row).Error; err != nil {
		return dictionarydomain.Dictionary{}, mapError(err)
	}

	return dictionaryToDomain(row, 0), nil
}

// GetDictionary returns one dictionary after applying its tenant boundary.
func (repository *Repository) GetDictionary(
	ctx context.Context,
	tenantID string,
	dictionaryID string,
) (dictionarydomain.Dictionary, error) {
	var row dictionaryProjection
	err := repository.database.WithContext(ctx).
		Table("dict_dictionary AS dictionary").
		Select("dictionary.*, COUNT(item.id) AS item_count").
		Joins("LEFT JOIN dict_item AS item ON item.dictionary_id = dictionary.id AND item.tenant_id = dictionary.tenant_id").
		Where("dictionary.id = ? AND dictionary.tenant_id = ?", dictionaryID, tenantID).
		Group("dictionary.id").
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return dictionarydomain.Dictionary{}, dictionaryapplication.ErrNotFound
	}
	if err != nil {
		return dictionarydomain.Dictionary{}, fmt.Errorf("get dictionary: %w", err)
	}

	return dictionaryToDomain(row.dictionaryModel, row.ItemCount), nil
}

// UpdateDictionary 先按租户锁定目标行，再核对版本；锁负责串行化并发提交，Version 负责识别
// 管理端基于旧快照发起的覆盖写。
func (repository *Repository) UpdateDictionary(
	ctx context.Context,
	input dictionaryapplication.DictionaryUpdateInput,
	now time.Time,
) (dictionarydomain.Dictionary, error) {
	var updated dictionarydomain.Dictionary
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row dictionaryModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", input.DictionaryID, input.TenantID).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dictionaryapplication.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock dictionary: %w", err)
		}
		if row.Version != input.Version {
			return dictionaryapplication.ErrVersionConflict
		}

		operatorID := input.OperatorID
		row.Code = input.Code
		row.Name = input.Name
		row.Description = input.Description
		row.Status = string(input.Status)
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &operatorID
		if err := transaction.Model(&dictionaryModel{}).
			Where("id = ? AND tenant_id = ?", row.ID, input.TenantID).
			Select("code", "name", "description", "status", "version", "updated_at", "updated_by").
			Updates(&row).Error; err != nil {
			return mapError(err)
		}

		itemCount, err := countItems(transaction, input.TenantID, input.DictionaryID)
		if err != nil {
			return fmt.Errorf("count dictionary items: %w", err)
		}

		updated = dictionaryToDomain(row, itemCount)
		return nil
	})
	if err != nil {
		return dictionarydomain.Dictionary{}, err
	}

	return updated, nil
}

// GetDictionaryByCode retrieves a dictionary by its tenant-scoped code.
func (repository *Repository) GetDictionaryByCode(
	ctx context.Context,
	tenantID string,
	code string,
) (dictionarydomain.Dictionary, error) {
	var row dictionaryModel
	err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND code = ?", tenantID, code).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return dictionarydomain.Dictionary{}, dictionaryapplication.ErrNotFound
	}
	if err != nil {
		return dictionarydomain.Dictionary{}, fmt.Errorf("get dictionary by code: %w", err)
	}

	return dictionaryToDomain(row, 0), nil
}

// ListItems 先确认父字典属于当前租户，再读取子项；仅凭 dictionary_id 不能跨过租户边界。
func (repository *Repository) ListItems(
	ctx context.Context,
	tenantID string,
	dictionaryID string,
	query dictionaryapplication.PageRequest,
	activeOnly bool,
) (dictionaryapplication.PageResult[dictionarydomain.Item], error) {
	if err := repository.ensureDictionaryExists(ctx, tenantID, dictionaryID); err != nil {
		return dictionaryapplication.PageResult[dictionarydomain.Item]{}, err
	}

	base := repository.database.WithContext(ctx).
		Model(&itemModel{}).
		Where("tenant_id = ? AND dictionary_id = ?", tenantID, dictionaryID)
	if activeOnly {
		base = base.Where("status = ?", dictionarydomain.StatusActive)
	} else if query.Status != "" {
		base = base.Where("status = ?", query.Status)
	}
	if query.Keyword != "" {
		keyword := "%" + query.Keyword + "%"
		base = base.Where("code LIKE ? OR label LIKE ? OR item_value LIKE ?", keyword, keyword, keyword)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return dictionaryapplication.PageResult[dictionarydomain.Item]{}, fmt.Errorf("count dictionary items: %w", err)
	}

	var rows []itemModel
	if err := base.
		Order("sort_order ASC, code ASC").
		Offset((query.Page - 1) * query.PageSize).
		Limit(query.PageSize).
		Find(&rows).Error; err != nil {
		return dictionaryapplication.PageResult[dictionarydomain.Item]{}, fmt.Errorf("list dictionary items: %w", err)
	}

	items := make([]dictionarydomain.Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, itemToDomain(row))
	}

	return dictionaryapplication.PageResult[dictionarydomain.Item]{
		Items:    items,
		Page:     query.Page,
		PageSize: query.PageSize,
		Total:    total,
	}, nil
}

// CreateItem 在同一事务确认父字典租户归属并插入子项，避免客户端提交其他租户的 dictionary_id。
func (repository *Repository) CreateItem(
	ctx context.Context,
	input dictionaryapplication.ItemCreateInput,
	itemID string,
	now time.Time,
) (dictionarydomain.Item, error) {
	var created dictionarydomain.Item
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var parent dictionaryModel
		err := transaction.
			Where("id = ? AND tenant_id = ?", input.DictionaryID, input.TenantID).
			Take(&parent).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dictionaryapplication.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("find item dictionary: %w", err)
		}

		operatorID := input.OperatorID
		row := itemModel{
			ID:           itemID,
			TenantID:     input.TenantID,
			DictionaryID: input.DictionaryID,
			Code:         input.Code,
			Label:        input.Label,
			Value:        input.Value,
			SortOrder:    input.SortOrder,
			Status:       string(input.Status),
			Version:      1,
			CreatedAt:    now,
			CreatedBy:    &operatorID,
			UpdatedAt:    now,
			UpdatedBy:    &operatorID,
		}
		if err := transaction.Create(&row).Error; err != nil {
			return mapError(err)
		}

		created = itemToDomain(row)
		return nil
	})
	if err != nil {
		return dictionarydomain.Item{}, err
	}

	return created, nil
}

// UpdateItem 的锁定条件同时包含 item、dictionary 与 tenant，防止移动子项或通过全局 ID 跨租户更新。
func (repository *Repository) UpdateItem(
	ctx context.Context,
	input dictionaryapplication.ItemUpdateInput,
	now time.Time,
) (dictionarydomain.Item, error) {
	var updated dictionarydomain.Item
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row itemModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND dictionary_id = ? AND tenant_id = ?", input.ItemID, input.DictionaryID, input.TenantID).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dictionaryapplication.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock dictionary item: %w", err)
		}
		if row.Version != input.Version {
			return dictionaryapplication.ErrVersionConflict
		}

		operatorID := input.OperatorID
		row.Code = input.Code
		row.Label = input.Label
		row.Value = input.Value
		row.SortOrder = input.SortOrder
		row.Status = string(input.Status)
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &operatorID
		if err := transaction.Model(&itemModel{}).
			Where("id = ? AND dictionary_id = ? AND tenant_id = ?", row.ID, input.DictionaryID, input.TenantID).
			Select("code", "label", "item_value", "sort_order", "status", "version", "updated_at", "updated_by").
			Updates(&row).Error; err != nil {
			return mapError(err)
		}

		updated = itemToDomain(row)
		return nil
	})
	if err != nil {
		return dictionarydomain.Item{}, err
	}

	return updated, nil
}

func (repository *Repository) ensureDictionaryExists(ctx context.Context, tenantID, dictionaryID string) error {
	var count int64
	if err := repository.database.WithContext(ctx).
		Model(&dictionaryModel{}).
		Where("id = ? AND tenant_id = ?", dictionaryID, tenantID).
		Count(&count).Error; err != nil {
		return fmt.Errorf("verify dictionary: %w", err)
	}
	if count == 0 {
		return dictionaryapplication.ErrNotFound
	}

	return nil
}

func countItems(database *gorm.DB, tenantID, dictionaryID string) (int64, error) {
	var count int64
	if err := database.Model(&itemModel{}).
		Where("tenant_id = ? AND dictionary_id = ?", tenantID, dictionaryID).
		Count(&count).Error; err != nil {
		return 0, err
	}

	return count, nil
}

func dictionaryToDomain(row dictionaryModel, itemCount int64) dictionarydomain.Dictionary {
	return dictionarydomain.Dictionary{
		ID:          row.ID,
		TenantID:    row.TenantID,
		Code:        row.Code,
		Name:        row.Name,
		Description: row.Description,
		Status:      dictionarydomain.Status(row.Status),
		ItemCount:   itemCount,
		Version:     row.Version,
		UpdatedAt:   row.UpdatedAt,
	}
}

func itemToDomain(row itemModel) dictionarydomain.Item {
	return dictionarydomain.Item{
		ID:           row.ID,
		TenantID:     row.TenantID,
		DictionaryID: row.DictionaryID,
		Code:         row.Code,
		Label:        row.Label,
		Value:        row.Value,
		SortOrder:    row.SortOrder,
		Status:       dictionarydomain.Status(row.Status),
		Version:      row.Version,
		UpdatedAt:    row.UpdatedAt,
	}
}

func mapError(err error) error {
	if isDuplicateKey(err) {
		return dictionaryapplication.ErrConflict
	}

	return err
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()
	return strings.Contains(message, "Duplicate entry") || strings.Contains(message, "duplicate key")
}
