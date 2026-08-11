package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

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
