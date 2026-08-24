// Package infrastructure persists configuration aggregates with GORM against migration-owned tables.
package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type namespaceModel struct {
	ID, TenantID, ApplicationID, EnvironmentID, Name, DisplayName, Description, Status string
	CurrentReleaseNo, Version                                                          uint64
	CreatedAt, UpdatedAt                                                               time.Time
	CreatedBy, UpdatedBy                                                               *string
}

func (namespaceModel) TableName() string { return "cfg_namespace" }

type applicationModel struct{ ID, Code, Name, Status string }

func (applicationModel) TableName() string { return "platform_application" }

type environmentModel struct{ ID, ApplicationID, Environment, Status string }

func (environmentModel) TableName() string { return "platform_application_environment" }

type itemModel struct {
	ID, NamespaceID, ConfigKey, ValueType string
	ValueText                             *string
	ValueJSON                             []byte
	SecretRef                             *string
	Sensitive                             bool
	Status                                string
	Version                               uint64
	CreatedAt, UpdatedAt                  time.Time
	CreatedBy, UpdatedBy                  *string
}

func (itemModel) TableName() string { return "cfg_item" }

type releaseModel struct {
	ID, NamespaceID, ReleaseStatus, ChangeSummary string
	ReleaseNo                                     uint64
	ItemCount                                     uint
	Checksum                                      []byte
	ReleasedBy                                    *string
	ReleasedAt                                    *time.Time
	CreatedAt                                     time.Time
}

func (releaseModel) TableName() string { return "cfg_release" }

type releaseItemModel struct {
	ReleaseID, ConfigKey, ValueType string
	ValueText                       *string
	ValueJSON                       []byte
	SecretRef                       *string
	Sensitive                       bool
}

func (releaseItemModel) TableName() string { return "cfg_release_item" }

// Repository 使用迁移维护的表执行显式查询，不调用 AutoMigrate。配置表结构和发布快照属于受控协议，
// 运行时自动改表会破坏历史版本的可重放性。
type Repository struct{ database *gorm.DB }

// NewRepository 创建配置仓储。
// database 不能为空；仓储不负责创建或修改数据库表结构。
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("configuration database must not be nil")
	}
	return &Repository{database: database}, nil
}

func (r *Repository) ListNamespaces(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Namespace], error) {
	db := r.database.WithContext(ctx).Table("cfg_namespace AS namespace").Joins("JOIN platform_application AS application ON application.id = namespace.application_id").Where("namespace.tenant_id = ?", tenantID)
	if query.Keyword != "" {
		db = db.Where("(namespace.name LIKE ? OR namespace.display_name LIKE ? OR namespace.description LIKE ?)", "%"+query.Keyword+"%", "%"+query.Keyword+"%", "%"+query.Keyword+"%")
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.Namespace]{}, err
	}
	var rows []struct {
		ID, Code, Name, Description, ApplicationID, ApplicationCode, ApplicationName string
		Version                                                                      uint64
	}
	err := db.Select("namespace.id, namespace.name AS code, namespace.display_name AS name, COALESCE(namespace.description, '') AS description, namespace.application_id, application.code AS application_code, application.name AS application_name, namespace.version").Order("namespace.updated_at DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error
	if err != nil {
		return application.PageResult[domain.Namespace]{}, err
	}
	items := make([]domain.Namespace, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.Namespace{ID: row.ID, Code: row.Code, Name: row.Name, Description: row.Description, Version: row.Version, Application: domain.Reference{ID: row.ApplicationID, Code: row.ApplicationCode, Name: row.ApplicationName}})
	}
	return application.PageResult[domain.Namespace]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}
func (r *Repository) CreateNamespace(ctx context.Context, input application.NamespaceCreateInput, id string, now time.Time) (domain.Namespace, error) {
	// 应用和环境归属始终从平台注册表反查；当前配置控制面只允许为 ACTIVE 的 dev 环境建命名空间。
	var app applicationModel
	if err := r.database.WithContext(ctx).Where("code = ? AND status = ?", input.ApplicationCode, "ACTIVE").Take(&app).Error; err != nil {
		return domain.Namespace{}, r.mapError(err)
	}
	var environment environmentModel
	if err := r.database.WithContext(ctx).Where("application_id = ? AND environment = ? AND status = ?", app.ID, "dev", "ACTIVE").Take(&environment).Error; err != nil {
		return domain.Namespace{}, r.mapError(err)
	}
	model := namespaceModel{ID: id, TenantID: input.TenantID, ApplicationID: app.ID, EnvironmentID: environment.ID, Name: input.Code, DisplayName: input.Name, Description: input.Description, Status: "ACTIVE", Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), CreatedBy: pointer(input.OperatorID), UpdatedBy: pointer(input.OperatorID)}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.Namespace{}, r.mapError(err)
	}
	return domain.Namespace{ID: id, Application: domain.Reference{ID: app.ID, Code: app.Code, Name: app.Name}, Code: input.Code, Name: input.Name, Description: input.Description, Version: 1}, nil
}
func (r *Repository) ListItems(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Item], error) {
	db := r.database.WithContext(ctx).Table("cfg_item AS item").Joins("JOIN cfg_namespace AS namespace ON namespace.id = item.namespace_id").Where("namespace.tenant_id = ?", tenantID)
	if query.NamespaceID != "" {
		db = db.Where("item.namespace_id = ?", query.NamespaceID)
	}
	if query.Keyword != "" {
		db = db.Where("item.config_key LIKE ?", "%"+query.Keyword+"%")
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.Item]{}, err
	}
	var rows []struct {
		ID, NamespaceID, NamespaceCode, NamespaceName, ConfigKey, ValueType string
		ValueText                                                           *string
		ValueJSON                                                           []byte
		Sensitive                                                           bool
		Version                                                             uint64
		UpdatedAt                                                           time.Time
	}
	err := db.Select("item.id, item.namespace_id, namespace.name AS namespace_code, namespace.display_name AS namespace_name, item.config_key, item.value_type, item.value_text, item.value_json, item.`sensitive`, item.version, item.updated_at").Order("item.updated_at DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error
	if err != nil {
		return application.PageResult[domain.Item]{}, err
	}
	items := make([]domain.Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, itemToDomain(row.ID, row.NamespaceID, row.NamespaceCode, row.NamespaceName, row.ConfigKey, row.ValueType, row.ValueText, row.ValueJSON, row.Sensitive, row.Version, row.UpdatedAt))
	}
	return application.PageResult[domain.Item]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}
func (r *Repository) CreateItem(ctx context.Context, input application.ItemCreateInput, id string, now time.Time) (domain.Item, error) {
	var namespace namespaceModel
	if err := r.database.WithContext(ctx).Where("id = ? AND tenant_id = ? AND status = ?", input.NamespaceID, input.TenantID, "ACTIVE").Take(&namespace).Error; err != nil {
		return domain.Item{}, r.mapError(err)
	}
	text, raw, err := encodeValue(input.ValueType, input.Value)
	if err != nil {
		return domain.Item{}, application.ErrValidation
	}
	model := itemModel{ID: id, NamespaceID: namespace.ID, ConfigKey: input.Key, ValueType: input.ValueType, ValueText: text, ValueJSON: raw, Sensitive: false, Status: "DRAFT", Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), CreatedBy: pointer(input.OperatorID), UpdatedBy: pointer(input.OperatorID)}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.Item{}, r.mapError(err)
	}
	return domain.Item{ID: id, Namespace: domain.Reference{ID: namespace.ID, Code: namespace.Name, Name: namespace.DisplayName}, Key: input.Key, ValueType: input.ValueType, Value: input.Value, Secret: false, Version: 1, UpdatedAt: now.UTC()}, nil
}
func (r *Repository) UpdateItem(ctx context.Context, input application.ItemUpdateInput, now time.Time) (domain.Item, error) {
	text, raw, err := encodeValue(input.ValueType, input.Value)
	if err != nil {
		return domain.Item{}, application.ErrValidation
	}

	err = r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先在租户连接下读取当前项，再校验目标命名空间；两次检查都在事务中，防止跨租户移动配置。
		var current itemModel
		if err := tx.Table("cfg_item AS item").
			Joins("JOIN cfg_namespace AS namespace ON namespace.id = item.namespace_id").
			Where("item.id = ? AND namespace.tenant_id = ?", input.ItemID, input.TenantID).
			Select("item.*").
			Take(&current).Error; err != nil {
			return r.mapError(err)
		}
		if current.Version != input.Version {
			return application.ErrVersionConflict
		}

		var target namespaceModel
		if err := tx.Where("id = ? AND tenant_id = ? AND status = ?", input.NamespaceID, input.TenantID, "ACTIVE").Take(&target).Error; err != nil {
			return r.mapError(err)
		}

		// version 条件把并发覆盖变成显式冲突；零行更新不能被当成成功，否则旧页面会静默覆盖新草稿。
		result := tx.Model(&itemModel{}).
			Where("id = ? AND version = ?", input.ItemID, input.Version).
			Updates(map[string]any{
				"namespace_id": input.NamespaceID,
				"config_key":   input.Key,
				"value_type":   input.ValueType,
				"value_text":   text,
				"value_json":   raw,
				"sensitive":    false,
				"updated_at":   now.UTC(),
				"updated_by":   input.OperatorID,
				"version":      gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return r.mapError(result.Error)
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}

		return nil
	})
	if err != nil {
		return domain.Item{}, err
	}

	return r.itemByID(ctx, input.TenantID, input.ItemID)
}
func (r *Repository) CreateRelease(ctx context.Context, input application.ReleaseCreateInput, id string, now time.Time) (domain.Release, error) {
	var result domain.Release
	// 锁定命名空间后，在同一事务中校验草稿版本、生成快照并推进当前发布号；任一步失败都不留下半成品发布。
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var namespace namespaceModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ? AND status = ?", input.NamespaceID, input.TenantID, "ACTIVE").Take(&namespace).Error; err != nil {
			return r.mapError(err)
		}
		ids := make([]string, 0, len(input.ItemVersions))
		versions := make(map[string]uint64, len(input.ItemVersions))
		for _, selected := range input.ItemVersions {
			ids = append(ids, selected.ItemID)
			versions[selected.ItemID] = selected.Version
		}
		// config_key 排序让快照内容与校验和不依赖数据库偶然的返回顺序。
		var items []itemModel
		if err := tx.Where("namespace_id = ? AND id IN ?", namespace.ID, ids).Order("config_key ASC").Find(&items).Error; err != nil {
			return err
		}
		if len(items) != len(ids) {
			return application.ErrConflict
		}
		for _, item := range items {
			if item.Version != versions[item.ID] || item.Status != "DRAFT" {
				return application.ErrConflict
			}
		}
		releaseNo := namespace.CurrentReleaseNo + 1
		snapshots := make([]releaseItemModel, 0, len(items))
		checksumSource := make([]map[string]any, 0, len(items))
		for _, item := range items {
			snapshots = append(snapshots, releaseItemModel{ReleaseID: id, ConfigKey: item.ConfigKey, ValueType: item.ValueType, ValueText: item.ValueText, ValueJSON: item.ValueJSON, SecretRef: item.SecretRef, Sensitive: item.Sensitive})
			checksumSource = append(checksumSource, map[string]any{"key": item.ConfigKey, "type": item.ValueType, "text": item.ValueText, "json": json.RawMessage(item.ValueJSON), "sensitive": item.Sensitive})
		}
		raw, err := json.Marshal(checksumSource)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		publishedAt := now.UTC()
		release := releaseModel{ID: id, NamespaceID: namespace.ID, ReleaseNo: releaseNo, ReleaseStatus: "PUBLISHED", ItemCount: uint(len(items)), Checksum: digest[:], ChangeSummary: strings.TrimSpace(input.Comment), ReleasedBy: pointer(input.OperatorID), ReleasedAt: &publishedAt, CreatedAt: publishedAt}
		if err := tx.Create(&release).Error; err != nil {
			return r.mapError(err)
		}
		if err := tx.Create(&snapshots).Error; err != nil {
			return r.mapError(err)
		}
		namespaceUpdate := tx.Model(&namespaceModel{}).
			Where("id = ? AND version = ?", namespace.ID, namespace.Version).
			Updates(map[string]any{
				"current_release_no": releaseNo,
				"version":            gorm.Expr("version + 1"),
				"updated_at":         publishedAt,
				"updated_by":         input.OperatorID,
			})
		if namespaceUpdate.Error != nil {
			return namespaceUpdate.Error
		}
		if namespaceUpdate.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		result = domain.Release{ID: id, Namespace: domain.Reference{ID: namespace.ID, Code: namespace.Name, Name: namespace.Name}, VersionNo: releaseNo, Status: "PUBLISHED", Comment: release.ChangeSummary, CreatedAt: publishedAt, PublishedAt: &publishedAt}
		return nil
	})
	return result, err
}
func (r *Repository) GetRelease(ctx context.Context, tenantID, releaseID string) (domain.Release, error) {
	var row struct {
		ID, NamespaceID, NamespaceCode, NamespaceName, ReleaseStatus, ChangeSummary string
		ReleaseNo                                                                   uint64
		CreatedAt                                                                   time.Time
		ReleasedAt                                                                  *time.Time
	}
	err := r.database.WithContext(ctx).Table("cfg_release AS release").Joins("JOIN cfg_namespace AS namespace ON namespace.id = release.namespace_id").Where("release.id = ? AND namespace.tenant_id = ?", releaseID, tenantID).Select("release.id, release.namespace_id, namespace.name AS namespace_code, namespace.display_name AS namespace_name, release.release_no, release.release_status, COALESCE(release.change_summary, '') AS change_summary, release.created_at, release.released_at").Scan(&row).Error
	if err != nil {
		return domain.Release{}, r.mapError(err)
	}
	if row.ID == "" {
		return domain.Release{}, application.ErrNotFound
	}
	return domain.Release{ID: row.ID, Namespace: domain.Reference{ID: row.NamespaceID, Code: row.NamespaceCode, Name: row.NamespaceName}, VersionNo: row.ReleaseNo, Status: row.ReleaseStatus, Comment: row.ChangeSummary, CreatedAt: row.CreatedAt, PublishedAt: row.ReleasedAt}, nil
}
func (r *Repository) GetPublished(ctx context.Context, tenantID, applicationCode, namespaceCode string) (domain.PublishedConfig, error) {
	// 运行时只解析命名空间指向的当前 PUBLISHED 快照，绝不回退读取仍可变化的 DRAFT 项。
	var release struct {
		ID        string
		ReleaseNo uint64
	}
	err := r.database.WithContext(ctx).Table("cfg_release AS release").Joins("JOIN cfg_namespace AS namespace ON namespace.id = release.namespace_id").Joins("JOIN platform_application AS application ON application.id = namespace.application_id").Where("namespace.tenant_id = ? AND application.code = ? AND namespace.name = ? AND release.release_no = namespace.current_release_no AND release.release_status = ?", tenantID, applicationCode, namespaceCode, "PUBLISHED").Select("release.id, release.release_no").Scan(&release).Error
	if err != nil {
		return domain.PublishedConfig{}, r.mapError(err)
	}
	if release.ID == "" {
		return domain.PublishedConfig{}, application.ErrNotFound
	}
	// 即使历史数据意外包含敏感项，也在查询层再次排除，避免运行时接口返回密钥材料。
	var items []releaseItemModel
	if err := r.database.WithContext(ctx).Where("release_id = ? AND `sensitive` = ?", release.ID, false).Find(&items).Error; err != nil {
		return domain.PublishedConfig{}, err
	}
	values := make(map[string]any, len(items))
	for _, item := range items {
		value, err := decodeValue(item.ValueType, item.ValueText, item.ValueJSON)
		if err != nil {
			return domain.PublishedConfig{}, fmt.Errorf("decode published configuration value: %w", err)
		}
		values[item.ConfigKey] = value
	}
	return domain.PublishedConfig{ApplicationCode: applicationCode, NamespaceCode: namespaceCode, ReleaseVersion: release.ReleaseNo, Values: values}, nil
}
func (r *Repository) itemByID(ctx context.Context, tenantID, itemID string) (domain.Item, error) {
	var row struct {
		ID, NamespaceID, NamespaceCode, NamespaceName, ConfigKey, ValueType string
		ValueText                                                           *string
		ValueJSON                                                           []byte
		Sensitive                                                           bool
		Version                                                             uint64
		UpdatedAt                                                           time.Time
	}
	err := r.database.WithContext(ctx).Table("cfg_item AS item").Joins("JOIN cfg_namespace AS namespace ON namespace.id = item.namespace_id").Where("item.id = ? AND namespace.tenant_id = ?", itemID, tenantID).Select("item.id, item.namespace_id, namespace.name AS namespace_code, namespace.display_name AS namespace_name, item.config_key, item.value_type, item.value_text, item.value_json, item.`sensitive`, item.version, item.updated_at").Scan(&row).Error
	if err != nil {
		return domain.Item{}, r.mapError(err)
	}
	if row.ID == "" {
		return domain.Item{}, application.ErrNotFound
	}
	return itemToDomain(row.ID, row.NamespaceID, row.NamespaceCode, row.NamespaceName, row.ConfigKey, row.ValueType, row.ValueText, row.ValueJSON, row.Sensitive, row.Version, row.UpdatedAt), nil
}
func itemToDomain(id, namespaceID, namespaceCode, namespaceName, key, valueType string, text *string, raw []byte, sensitive bool, version uint64, updatedAt time.Time) domain.Item {
	value := any(nil)
	if !sensitive {
		value, _ = decodeValue(valueType, text, raw)
	}
	return domain.Item{ID: id, Namespace: domain.Reference{ID: namespaceID, Code: namespaceCode, Name: namespaceName}, Key: key, ValueType: valueType, Value: value, Secret: sensitive, Version: version, UpdatedAt: updatedAt}
}
func encodeValue(valueType string, value any) (*string, []byte, error) {
	switch valueType {
	case "STRING":
		text := value.(string)
		return &text, nil, nil
	case "NUMBER", "BOOLEAN", "JSON":
		raw, err := json.Marshal(value)
		return nil, raw, err
	default:
		return nil, nil, application.ErrValidation
	}
}
func decodeValue(valueType string, text *string, raw []byte) (any, error) {
	switch valueType {
	case "STRING":
		if text == nil {
			return "", nil
		}
		return *text, nil
	case "NUMBER":
		var value json.Number
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	case "BOOLEAN":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return value, nil
	case "JSON":
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, application.ErrValidation
	}
}
func (r *Repository) mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		return application.ErrConflict
	}
	return err
}
func pointer(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
