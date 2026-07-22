// Package infrastructure contains the GORM implementation of local federation persistence.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	providerTable = "iam_federated_identity_provider"
	bindingTable  = "iam_federated_identity_binding"
)

// Repository uses migration-owned tables only. It deliberately has no AutoMigrate behavior.
type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("federation database must not be nil")
	}
	return &Repository{db: db}, nil
}

func (repository *Repository) CreateProvider(ctx context.Context, provider domain.Provider) error {
	err := repository.db.WithContext(ctx).Table(providerTable).Create(map[string]any{
		"id": provider.ID, "tenant_id": provider.TenantID, "provider_code": provider.Code,
		"provider_type": provider.Type, "issuer": nullableString(provider.Issuer), "client_id": provider.ClientID,
		"callback_uri": provider.CallbackURI, "authorization_scopes": marshalScopes(provider.AuthorizationScopes),
		"client_secret_ciphertext": cloneBytes(provider.ClientSecretCiphertext),
		"client_secret_updated_at": provider.ClientSecretUpdatedAt, "display_name": provider.DisplayName,
		"status": provider.Status, "created_at": provider.CreatedAt, "updated_at": provider.UpdatedAt,
		"version": provider.Version,
	}).Error
	return mapWriteError(err, "create federated identity provider")
}

func (repository *Repository) ListProviders(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Provider], error) {
	database := repository.providerQuery(ctx).Where("tenant_id = ?", tenantID)
	if query.Keyword != "" {
		keyword := "%" + escapeLike(query.Keyword) + "%"
		database = database.Where("provider_code LIKE ? ESCAPE '\\\\' OR display_name LIKE ? ESCAPE '\\\\'", keyword, keyword)
	}
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}

	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[domain.Provider]{}, fmt.Errorf("count federated identity providers: %w", err)
	}
	var rows []providerRow
	if err := database.Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Provider]{}, fmt.Errorf("list federated identity providers: %w", err)
	}
	items := make([]domain.Provider, 0, len(rows))
	for _, row := range rows {
		provider, err := providerFromRow(row)
		if err != nil {
			return application.PageResult[domain.Provider]{}, err
		}
		items = append(items, provider)
	}
	return application.PageResult[domain.Provider]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *Repository) FindProviderByID(ctx context.Context, tenantID, providerID string) (domain.Provider, error) {
	var row providerRow
	err := repository.providerQuery(ctx).Where("tenant_id = ? AND id = ?", tenantID, providerID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Provider{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Provider{}, fmt.Errorf("find federated identity provider: %w", err)
	}
	return providerFromRow(row)
}

func (repository *Repository) FindProviderByCode(ctx context.Context, tenantID, code string) (domain.Provider, error) {
	var row providerRow
	err := repository.providerQuery(ctx).Where("tenant_id = ? AND provider_code = ?", tenantID, code).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Provider{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Provider{}, fmt.Errorf("find federated identity provider by code: %w", err)
	}
	return providerFromRow(row)
}

func (repository *Repository) UpdateProvider(ctx context.Context, input application.ProviderPersistenceUpdate, updatedAt time.Time) (domain.Provider, error) {
	updates := map[string]any{
		"display_name": input.DisplayName,
		"status":       input.Status,
		"updated_at":   updatedAt,
		"version":      gorm.Expr("version + 1"),
	}
	if input.ClientID != nil {
		updates["client_id"] = *input.ClientID
	}
	if input.CallbackURI != nil {
		updates["callback_uri"] = *input.CallbackURI
	}
	if input.AuthorizationScopes != nil {
		updates["authorization_scopes"] = marshalScopes(*input.AuthorizationScopes)
	}
	if input.ClientSecretCiphertext != nil {
		updates["client_secret_ciphertext"] = cloneBytes(*input.ClientSecretCiphertext)
		updates["client_secret_updated_at"] = input.ClientSecretUpdatedAt
	}
	result := repository.db.WithContext(ctx).Table(providerTable).
		Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.ProviderID, input.Version).
		Updates(updates)
	if result.Error != nil {
		return domain.Provider{}, mapWriteError(result.Error, "update federated identity provider")
	}
	if result.RowsAffected == 0 {
		return domain.Provider{}, repository.versionedProviderError(ctx, input.TenantID, input.ProviderID)
	}
	return repository.FindProviderByID(ctx, input.TenantID, input.ProviderID)
}

func (repository *Repository) UserExists(ctx context.Context, tenantID, userID string) (bool, error) {
	var total int64
	err := repository.db.WithContext(ctx).Table("iam_user").
		Where("tenant_id = ? AND id = ?", tenantID, userID).Count(&total).Error
	if err != nil {
		return false, fmt.Errorf("check federated binding user: %w", err)
	}
	return total == 1, nil
}

func (repository *Repository) Bind(ctx context.Context, binding domain.Binding) (domain.Binding, error) {
	var persisted domain.Binding
	err := repository.db.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing bindingRow
		err := transaction.Table(bindingTable).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND provider_id = ? AND subject_hash = ?", binding.TenantID, binding.ProviderID, binding.SubjectHash[:]).
			Take(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := transaction.Table(bindingTable).Create(map[string]any{
				"id": binding.ID, "tenant_id": binding.TenantID, "provider_id": binding.ProviderID,
				"user_id": binding.UserID, "subject_hash": binding.SubjectHash[:], "bound_at": binding.BoundAt,
				"status": binding.Status, "version": binding.Version,
			}).Error; err != nil {
				return mapWriteError(err, "create federated identity binding")
			}
			persisted = binding
			return nil
		}
		if err != nil {
			return fmt.Errorf("find federated identity binding while binding: %w", err)
		}
		if existing.Status != domain.BindingStatusUnbound {
			return application.ErrConflict
		}

		result := transaction.Table(bindingTable).Where("id = ? AND version = ?", existing.ID, existing.Version).Updates(map[string]any{
			"user_id": binding.UserID, "bound_at": binding.BoundAt, "unbound_at": nil,
			"status": domain.BindingStatusActive, "version": gorm.Expr("version + 1"),
		})
		if result.Error != nil {
			return mapWriteError(result.Error, "rebind federated identity")
		}
		if result.RowsAffected == 0 {
			return application.ErrConflict
		}
		existing.UserID = binding.UserID
		existing.BoundAt = binding.BoundAt
		existing.UnboundAt = nil
		existing.Status = domain.BindingStatusActive
		existing.Version++
		var conversionErr error
		persisted, conversionErr = bindingFromRow(existing)
		return conversionErr
	})
	if err != nil {
		return domain.Binding{}, err
	}
	return persisted, nil
}

func (repository *Repository) ListBindings(ctx context.Context, tenantID, userID string) ([]domain.Binding, error) {
	var rows []bindingRow
	err := repository.bindingQuery(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Order("bound_at DESC, id DESC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list federated identity bindings: %w", err)
	}
	bindings := make([]domain.Binding, 0, len(rows))
	for _, row := range rows {
		binding, err := bindingFromRow(row)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func (repository *Repository) FindBindingByID(ctx context.Context, tenantID, bindingID string) (domain.Binding, error) {
	var row bindingRow
	err := repository.bindingQuery(ctx).Where("tenant_id = ? AND id = ?", tenantID, bindingID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Binding{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Binding{}, fmt.Errorf("find federated identity binding: %w", err)
	}
	return bindingFromRow(row)
}

func (repository *Repository) UnbindBinding(ctx context.Context, tenantID, userID, bindingID string, version uint64, unboundAt time.Time) (domain.Binding, error) {
	result := repository.db.WithContext(ctx).Table(bindingTable).
		Where("tenant_id = ? AND user_id = ? AND id = ? AND status = ? AND version = ?", tenantID, userID, bindingID, domain.BindingStatusActive, version).
		Updates(map[string]any{
			"status":     domain.BindingStatusUnbound,
			"unbound_at": unboundAt,
			"version":    gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return domain.Binding{}, mapWriteError(result.Error, "unbind federated identity")
	}
	if result.RowsAffected == 0 {
		return domain.Binding{}, repository.versionedBindingError(ctx, tenantID, userID, bindingID)
	}
	return repository.FindBindingByID(ctx, tenantID, bindingID)
}

func (repository *Repository) ResolveActiveBinding(ctx context.Context, tenantID, providerID string, subjectHash [32]byte) (domain.Binding, error) {
	var row bindingRow
	err := repository.bindingQuery(ctx).
		Where("tenant_id = ? AND provider_id = ? AND subject_hash = ? AND status = ?", tenantID, providerID, subjectHash[:], domain.BindingStatusActive).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Binding{}, application.ErrNotFound
	}
	if err != nil {
		return domain.Binding{}, fmt.Errorf("resolve federated identity binding: %w", err)
	}
	return bindingFromRow(row)
}

func (repository *Repository) providerQuery(ctx context.Context) *gorm.DB {
	return repository.db.WithContext(ctx).Table(providerTable).Select(
		"id", "tenant_id", "provider_code", "provider_type", "issuer", "client_id", "callback_uri", "authorization_scopes", "client_secret_ciphertext", "client_secret_updated_at", "display_name", "status", "created_at", "updated_at", "version",
	)
}

func (repository *Repository) bindingQuery(ctx context.Context) *gorm.DB {
	return repository.db.WithContext(ctx).Table(bindingTable).Select(
		"id", "tenant_id", "provider_id", "user_id", "subject_hash", "bound_at", "unbound_at", "status", "version",
	)
}

func (repository *Repository) versionedProviderError(ctx context.Context, tenantID, providerID string) error {
	var total int64
	if err := repository.db.WithContext(ctx).Table(providerTable).Where("tenant_id = ? AND id = ?", tenantID, providerID).Count(&total).Error; err != nil {
		return fmt.Errorf("check federated provider after update: %w", err)
	}
	if total == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func (repository *Repository) versionedBindingError(ctx context.Context, tenantID, userID, bindingID string) error {
	var total int64
	if err := repository.db.WithContext(ctx).Table(bindingTable).
		Where("tenant_id = ? AND user_id = ? AND id = ?", tenantID, userID, bindingID).Count(&total).Error; err != nil {
		return fmt.Errorf("check federated binding after unbind: %w", err)
	}
	if total == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func mapWriteError(err error, operation string) error {
	if err == nil {
		return nil
	}
	if isDuplicateError(err) {
		return application.ErrConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func isDuplicateError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(strings.ToLower(err.Error()), "1062")
}

func pageOffset(query application.PageRequest) int { return (query.Page - 1) * query.PageSize }

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}

type providerRow struct {
	ID                     string     `gorm:"column:id"`
	TenantID               string     `gorm:"column:tenant_id"`
	Code                   string     `gorm:"column:provider_code"`
	Type                   string     `gorm:"column:provider_type"`
	Issuer                 *string    `gorm:"column:issuer"`
	ClientID               string     `gorm:"column:client_id"`
	CallbackURI            string     `gorm:"column:callback_uri"`
	AuthorizationScopes    []byte     `gorm:"column:authorization_scopes"`
	ClientSecretCiphertext []byte     `gorm:"column:client_secret_ciphertext"`
	ClientSecretUpdatedAt  *time.Time `gorm:"column:client_secret_updated_at"`
	DisplayName            string     `gorm:"column:display_name"`
	Status                 string     `gorm:"column:status"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
	Version                uint64     `gorm:"column:version"`
}

func (providerRow) TableName() string { return providerTable }

type bindingRow struct {
	ID          string     `gorm:"column:id"`
	TenantID    string     `gorm:"column:tenant_id"`
	ProviderID  string     `gorm:"column:provider_id"`
	UserID      string     `gorm:"column:user_id"`
	SubjectHash []byte     `gorm:"column:subject_hash"`
	BoundAt     time.Time  `gorm:"column:bound_at"`
	UnboundAt   *time.Time `gorm:"column:unbound_at"`
	Status      string     `gorm:"column:status"`
	Version     uint64     `gorm:"column:version"`
}

func (bindingRow) TableName() string { return bindingTable }

func providerFromRow(row providerRow) (domain.Provider, error) {
	scopes, err := unmarshalScopes(row.AuthorizationScopes)
	if err != nil {
		return domain.Provider{}, err
	}
	issuer := ""
	if row.Issuer != nil {
		issuer = *row.Issuer
	}
	return domain.Provider{
		ID: row.ID, TenantID: row.TenantID, Code: row.Code, Type: row.Type, Issuer: issuer,
		ClientID: row.ClientID, CallbackURI: row.CallbackURI, AuthorizationScopes: scopes,
		ClientSecretCiphertext: cloneBytes(row.ClientSecretCiphertext), ClientSecretUpdatedAt: copyTime(row.ClientSecretUpdatedAt),
		DisplayName: row.DisplayName, Status: row.Status, CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: row.UpdatedAt.UTC(), Version: row.Version,
	}, nil
}

// nullableString preserves a NULL issuer for non-OIDC providers. A nullable value allows the
// existing tenant-and-issuer unique key to continue enforcing OIDC issuer uniqueness without
// inventing a synthetic issuer for DingTalk QR providers.
func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func marshalScopes(scopes []string) []byte {
	encoded, err := json.Marshal(scopes)
	if err != nil {
		return nil
	}
	return encoded
}

func unmarshalScopes(value []byte) ([]string, error) {
	if len(value) == 0 {
		return nil, nil
	}
	var scopes []string
	if err := json.Unmarshal(value, &scopes); err != nil {
		return nil, fmt.Errorf("decode persisted federated provider scopes: %w", err)
	}
	return append([]string(nil), scopes...), nil
}

func cloneBytes(value []byte) []byte { return append([]byte(nil), value...) }

func bindingFromRow(row bindingRow) (domain.Binding, error) {
	if len(row.SubjectHash) != 32 {
		return domain.Binding{}, fmt.Errorf("invalid persisted federated subject hash length: %d", len(row.SubjectHash))
	}
	var subjectHash [32]byte
	copy(subjectHash[:], row.SubjectHash)
	return domain.Binding{
		ID: row.ID, TenantID: row.TenantID, ProviderID: row.ProviderID, UserID: row.UserID,
		SubjectHash: subjectHash, BoundAt: row.BoundAt.UTC(), UnboundAt: copyTime(row.UnboundAt),
		Status: row.Status, Version: row.Version,
	}, nil
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

var _ application.Repository = (*Repository)(nil)
