// Package infrastructure provides MySQL and HTTP adapters for DingTalk QR login.
package infrastructure

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	dingTalkQRLoginStateTable = "iam_dingtalk_qr_login_state"
	qrStateStatusActive       = "ACTIVE"
	qrStateStatusConsumed     = "CONSUMED"
	qrStateStatusExpired      = "EXPIRED"
	sha256Size                = 32
)

// StateProtector protects the complete server-side DingTalk QR state before persistence. The
// existing IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY EnvelopeProtector satisfies this interface.
type StateProtector interface {
	Encrypt(context.Context, []byte) ([]byte, error)
	Decrypt(context.Context, []byte) ([]byte, error)
}

// StateIDGenerator creates ULID-compatible keys for state rows.
type StateIDGenerator interface {
	New(time.Time) (string, error)
}

// GORMStateStore persists state in the explicitly migrated MySQL table. It never invokes
// AutoMigrate because authorization-state schema changes require reviewable SQL migrations.
type GORMStateStore struct {
	database    *gorm.DB
	protector   StateProtector
	idGenerator StateIDGenerator
}

var _ application.StateStore = (*GORMStateStore)(nil)

// NewGORMStateStore creates the encrypted MySQL state adapter.
func NewGORMStateStore(database *gorm.DB, protector StateProtector, idGenerator StateIDGenerator) (*GORMStateStore, error) {
	if database == nil || protector == nil || idGenerator == nil {
		return nil, errors.New("dingtalk QR login state dependencies must not be nil")
	}
	return &GORMStateStore{database: database, protector: protector, idGenerator: idGenerator}, nil
}

// NewDefaultGORMStateStore uses the shared cryptographic ULID generator.
func NewDefaultGORMStateStore(database *gorm.DB, protector StateProtector) (*GORMStateStore, error) {
	return NewGORMStateStore(database, protector, ulid.Generator{})
}

type dingTalkQRLoginStateModel struct {
	ID                string     `gorm:"column:id"`
	StateHash         []byte     `gorm:"column:state_hash"`
	TenantID          string     `gorm:"column:tenant_id"`
	ProviderCode      string     `gorm:"column:provider_code"`
	PayloadCiphertext []byte     `gorm:"column:payload_ciphertext"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	ExpiresAt         time.Time  `gorm:"column:expires_at"`
	ConsumedAt        *time.Time `gorm:"column:consumed_at"`
	Status            string     `gorm:"column:status"`
}

func (dingTalkQRLoginStateModel) TableName() string { return dingTalkQRLoginStateTable }

// encryptedStatePayload contains all callback data as one authenticated encrypted payload. It
// deliberately contains hashes instead of raw state/browser-binding values.
type encryptedStatePayload struct {
	BrowserBindingHash []byte `json:"browser_binding_hash"`
	SessionID          string `json:"session_id"`
	ProviderID         string `json:"provider_id"`
	AppKey             string `json:"app_key"`
	RedirectURI        string `json:"redirect_uri"`
	ReturnTo           string `json:"return_to"`
}

// Save encrypts the private payload before creating the durable state row.
func (store *GORMStateStore) Save(ctx context.Context, state domain.State) error {
	if !validStateForPersistence(state) {
		return application.ErrInvalidState
	}
	payload, err := json.Marshal(encryptedStatePayload{
		BrowserBindingHash: append([]byte(nil), state.BrowserBindingHash[:]...), SessionID: state.SessionID,
		ProviderID: state.ProviderID, AppKey: state.AppKey, RedirectURI: state.RedirectURI, ReturnTo: state.ReturnTo,
	})
	if err != nil {
		return application.ErrInvalidState
	}
	ciphertext, err := store.protector.Encrypt(ctx, payload)
	if err != nil || len(ciphertext) == 0 {
		return application.ErrInvalidState
	}
	id, err := store.idGenerator.New(state.CreatedAt.UTC())
	if err != nil || strings.TrimSpace(id) == "" {
		return application.ErrInvalidState
	}
	model := dingTalkQRLoginStateModel{
		ID: id, StateHash: append([]byte(nil), state.StateHash[:]...), TenantID: state.TenantID,
		ProviderCode: state.ProviderCode, PayloadCiphertext: ciphertext, CreatedAt: state.CreatedAt.UTC(),
		ExpiresAt: state.ExpiresAt.UTC(), Status: qrStateStatusActive,
	}
	if err := store.database.WithContext(ctx).Create(&model).Error; err != nil {
		return application.ErrInvalidState
	}
	return nil
}

// Consume atomically verifies the initiating browser and then consumes the one-time state. A
// browser-binding mismatch does not invalidate another browser's valid QR session. After a valid
// browser consumes the state, downstream protocol or binding failures cannot make it reusable.
func (store *GORMStateStore) Consume(ctx context.Context, stateHash, browserBindingHash [32]byte, now time.Time) (domain.State, error) {
	var model dingTalkQRLoginStateModel
	var state domain.State
	var expired bool
	now = now.UTC()
	err := store.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("state_hash = ?", stateHash[:]).Take(&model).Error; err != nil {
			return application.ErrInvalidState
		}
		if model.Status != qrStateStatusActive {
			return application.ErrInvalidState
		}
		if !model.ExpiresAt.After(now) {
			if err := transaction.Model(&dingTalkQRLoginStateModel{}).Where("id = ? AND status = ?", model.ID, qrStateStatusActive).
				Updates(map[string]any{"status": qrStateStatusExpired, "consumed_at": now}).Error; err != nil {
				return application.ErrInvalidState
			}
			expired = true
			return nil
		}

		payloadBytes, err := store.protector.Decrypt(ctx, append([]byte(nil), model.PayloadCiphertext...))
		if err != nil {
			return application.ErrInvalidState
		}
		var payload encryptedStatePayload
		if err := json.Unmarshal(payloadBytes, &payload); err != nil || len(payload.BrowserBindingHash) != sha256Size {
			return application.ErrInvalidState
		}
		if subtle.ConstantTimeCompare(payload.BrowserBindingHash, browserBindingHash[:]) != 1 {
			return application.ErrInvalidState
		}
		var storedBindingHash [sha256Size]byte
		copy(storedBindingHash[:], payload.BrowserBindingHash)
		state = domain.State{
			StateHash: stateHash, BrowserBindingHash: storedBindingHash, SessionID: payload.SessionID,
			TenantID: model.TenantID, ProviderCode: model.ProviderCode, ProviderID: payload.ProviderID,
			AppKey: payload.AppKey, RedirectURI: payload.RedirectURI, ReturnTo: payload.ReturnTo,
			CreatedAt: model.CreatedAt.UTC(), ExpiresAt: model.ExpiresAt.UTC(),
		}
		if !validStateForPersistence(state) {
			return application.ErrInvalidState
		}

		result := transaction.Model(&dingTalkQRLoginStateModel{}).Where("id = ? AND status = ?", model.ID, qrStateStatusActive).
			Updates(map[string]any{"status": qrStateStatusConsumed, "consumed_at": now})
		if result.Error != nil || result.RowsAffected != 1 {
			return application.ErrInvalidState
		}
		return nil
	})
	if err != nil {
		return domain.State{}, application.ErrInvalidState
	}
	if expired {
		return domain.State{}, application.ErrInvalidState
	}
	return state, nil
}

func validStateForPersistence(state domain.State) bool {
	return strings.TrimSpace(state.SessionID) != "" && strings.TrimSpace(state.TenantID) != "" &&
		strings.TrimSpace(state.ProviderCode) != "" && strings.TrimSpace(state.ProviderID) != "" &&
		strings.TrimSpace(state.AppKey) != "" && strings.TrimSpace(state.RedirectURI) != "" &&
		strings.TrimSpace(state.ReturnTo) != "" && !state.CreatedAt.IsZero() && state.ExpiresAt.After(state.CreatedAt)
}
