package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ClientMappingStore struct{ database *gorm.DB }

func NewClientMappingStore(database *gorm.DB) (*ClientMappingStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak Client mapping database must not be nil")
	}
	return &ClientMappingStore{database: database}, nil
}

func (store *ClientMappingStore) SaveKeycloakClientMapping(ctx context.Context, tenantID, applicationID, environmentID, realm, clientID string) error {
	now := time.Now().UTC()
	return store.database.WithContext(ctx).Table("keycloak_application_client_mapping").Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}, {Name: "environment_id"}},
		DoUpdates: clause.Assignments(map[string]any{"realm": strings.TrimSpace(realm), "keycloak_client_id": strings.TrimSpace(clientID), "status": "SYNCED", "last_synced_at": now, "updated_at": now}),
	}).Create(map[string]any{"tenant_id": strings.TrimSpace(tenantID), "application_id": strings.TrimSpace(applicationID), "environment_id": strings.TrimSpace(environmentID), "realm": strings.TrimSpace(realm), "keycloak_client_id": strings.TrimSpace(clientID), "status": "SYNCED", "last_synced_at": now, "created_at": now, "updated_at": now}).Error
}

// GetKeycloakClientMapping exposes a safe persisted mapping summary for the
// application authentication UI.  Client secrets are never stored in this
// table and are never part of this return value.
func (store *ClientMappingStore) GetKeycloakClientMapping(ctx context.Context, tenantID, applicationID, environmentID string) (registryhttp.KeycloakClientMapping, error) {
	var row struct {
		Realm        string     `gorm:"column:realm"`
		ClientID     string     `gorm:"column:keycloak_client_id"`
		Status       string     `gorm:"column:status"`
		LastSyncedAt *time.Time `gorm:"column:last_synced_at"`
	}
	err := store.database.WithContext(ctx).Table("keycloak_application_client_mapping").
		Select("realm, keycloak_client_id, status, last_synced_at").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID)).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return registryhttp.KeycloakClientMapping{Status: "NOT_CONFIGURED"}, nil
	}
	if err != nil {
		return registryhttp.KeycloakClientMapping{}, err
	}
	return registryhttp.KeycloakClientMapping{Realm: strings.TrimSpace(row.Realm), ClientID: strings.TrimSpace(row.ClientID), Status: strings.TrimSpace(row.Status), LastSyncedAt: row.LastSyncedAt, Exists: true}, nil
}

// ResolveEffectiveKeycloakClient keeps existing subsystem configuration working
// when the platform environment label has been normalized. A persisted mapping
// is authoritative, except that one explicitly registered legacy browser OAuth
// client on the same application/environment is preserved so a running subsystem
// is not silently forced to change its client_id. Service clients are excluded.
// No prod/dev string substitution or cross-environment lookup is performed.
func (store *ClientMappingStore) ResolveEffectiveKeycloakClient(ctx context.Context, tenantID, applicationID, environmentID, canonicalClientID string) (registryhttp.KeycloakClientResolution, error) {
	canonicalClientID = strings.TrimSpace(canonicalClientID)
	resolution := registryhttp.KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}

	mapping, err := store.GetKeycloakClientMapping(ctx, tenantID, applicationID, environmentID)
	if err != nil {
		return resolution, err
	}
	mappedClientID := strings.TrimSpace(mapping.ClientID)

	var clients []struct {
		ClientID   string `gorm:"column:client_id"`
		ClientType string `gorm:"column:client_type"`
		Status     string `gorm:"column:status"`
	}
	err = store.database.WithContext(ctx).Table("platform_oauth_client").
		Select("client_id, client_type, status").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ? AND status = ? AND client_type IN ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID), "ACTIVE", []string{"public", "confidential"}).
		Find(&clients).Error
	if err != nil {
		return resolution, err
	}

	legacy := make([]string, 0, len(clients))
	for _, client := range clients {
		id := strings.TrimSpace(client.ClientID)
		if id == "" || strings.EqualFold(id, canonicalClientID) || isKeycloakServiceClientID(id) {
			continue
		}
		legacy = append(legacy, id)
	}
	if len(legacy) > 1 {
		return resolution, errors.New("multiple legacy browser OAuth clients are registered for the application environment")
	}
	if len(legacy) == 1 {
		return registryhttp.KeycloakClientResolution{ClientID: legacy[0], CanonicalClientID: canonicalClientID, Source: "legacy_oauth_client", LegacyCompatibility: true}, nil
	}
	if mappedClientID != "" && strings.EqualFold(mapping.Status, "SYNCED") {
		return registryhttp.KeycloakClientResolution{ClientID: mappedClientID, CanonicalClientID: canonicalClientID, Source: "persisted_mapping", LegacyCompatibility: !strings.EqualFold(mappedClientID, canonicalClientID)}, nil
	}
	return resolution, nil
}

func isKeycloakServiceClientID(clientID string) bool {
	clientID = strings.ToLower(strings.TrimSpace(clientID))
	for _, suffix := range []string{"-catalog-publisher", "-audit-publisher", "-summary-read", "-opportunity-intake"} {
		if strings.HasSuffix(clientID, suffix) {
			return true
		}
	}
	return false
}
