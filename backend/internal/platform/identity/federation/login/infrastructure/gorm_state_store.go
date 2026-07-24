// Package infrastructure provides production persistence adapters for external OIDC browser login.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	federatedLoginStateTable = "iam_federated_login_state"
	loginStateStatusActive   = "ACTIVE"
	loginStateStatusConsumed = "CONSUMED"
	loginStateStatusExpired  = "EXPIRED"
)

// StateProtector protects the private server-side authorization state before it is persisted.
// security.EnvelopeProtector implements this interface with AES-256-GCM. Keeping the interface
// narrow lets bootstrap supply a separately scoped encryption key without coupling this package to
// configuration details.
type StateProtector interface {
	Encrypt(context.Context, []byte) ([]byte, error)
	Decrypt(context.Context, []byte) ([]byte, error)
}

// StateIDGenerator creates ULID-compatible primary keys for durable authorization-state records.
type StateIDGenerator interface {
	New(time.Time) (string, error)
}

// GORMStateStore persists one-time external-login state in MySQL. It relies on the explicit
// iam_federated_login_state migration and deliberately never invokes GORM AutoMigrate.
type GORMStateStore struct {
	database    *gorm.DB
	protector   StateProtector
	idGenerator StateIDGenerator
}

// NewGORMStateStore constructs a durable state store. The protector must use an encryption key
// dedicated to external-login state; using a nil protector could otherwise silently persist data
// without confidentiality protection.
func NewGORMStateStore(database *gorm.DB, protector StateProtector, idGenerator StateIDGenerator) (*GORMStateStore, error) {
	if database == nil {
		return nil, errors.New("external login state database must not be nil")
	}
	if protector == nil {
		return nil, errors.New("external login state protector must not be nil")
	}
	if idGenerator == nil {
		return nil, errors.New("external login state ID generator must not be nil")
	}
	return &GORMStateStore{database: database, protector: protector, idGenerator: idGenerator}, nil
}

// NewDefaultGORMStateStore constructs the MySQL state store with the shared cryptographic ULID
// generator. Bootstrap may use this helper after creating the dedicated AES-GCM protector.
func NewDefaultGORMStateStore(database *gorm.DB, protector StateProtector) (*GORMStateStore, error) {
	return NewGORMStateStore(database, protector, ulid.Generator{})
}

type federatedLoginStateModel struct {
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

func (federatedLoginStateModel) TableName() string { return federatedLoginStateTable }

// persistedStatePayload contains every server-side value needed after the authorization redirect.
// It is serialized and encrypted as one authenticated payload; therefore the database cannot read
// or alter fields such as PKCEVerifier, nonce or callback routing data independently.
type persistedStatePayload struct {
	Issuer             string           `json:"issuer"`
	ClientID           string           `json:"client_id"`
	RedirectURI        string           `json:"redirect_uri"`
	Discovery          domain.Discovery `json:"discovery"`
	Nonce              string           `json:"nonce"`
	PKCEVerifier       string           `json:"pkce_verifier"`
	ReturnTo           string           `json:"return_to"`
	BrowserBindingHash []byte           `json:"browser_binding_hash"`
}

// Save stores a new encrypted, short-lived authorization attempt. The raw state is never accepted
// or stored here; StateHash is the only lookup material that reaches MySQL.
func (store *GORMStateStore) Save(ctx context.Context, state domain.State) error {
	if err := validateStateForPersistence(state); err != nil {
		return err
	}

	payload, err := json.Marshal(persistedStatePayload{
		Issuer: state.Issuer, ClientID: state.ClientID, RedirectURI: state.RedirectURI,
		Discovery: state.Discovery, Nonce: state.Nonce, PKCEVerifier: state.PKCEVerifier, ReturnTo: state.ReturnTo,
		BrowserBindingHash: append([]byte(nil), state.BrowserBindingHash[:]...),
	})
	if err != nil {
		return fmt.Errorf("serialize external login state: %w", err)
	}
	ciphertext, err := store.protector.Encrypt(ctx, payload)
	if err != nil {
		return fmt.Errorf("encrypt external login state: %w", err)
	}

	createdAt := state.CreatedAt.UTC()
	identifier, err := store.idGenerator.New(createdAt)
	if err != nil {
		return fmt.Errorf("generate external login state ID: %w", err)
	}
	row := federatedLoginStateModel{
		ID: identifier, StateHash: append([]byte(nil), state.StateHash[:]...), TenantID: state.TenantID,
		ProviderCode: state.ProviderCode, PayloadCiphertext: ciphertext, CreatedAt: createdAt,
		ExpiresAt: state.ExpiresAt.UTC(), Status: loginStateStatusActive,
	}
	if err := store.database.WithContext(ctx).Create(&row).Error; err != nil {
		// A duplicate digest is deliberately indistinguishable from every other unavailable state.
		// The application service maps Save failures to a generic provider-unavailable failure.
		return fmt.Errorf("persist external login state: %w", err)
	}
	return nil
}

// Consume uses an InnoDB transaction and SELECT ... FOR UPDATE to make a state single-use across
// API instances. The row is transitioned to a terminal status and committed before decryption, so
// a malformed ciphertext or later token/session failure can never make the authorization state
// usable again.
func (store *GORMStateStore) Consume(ctx context.Context, hash [32]byte, now time.Time) (domain.State, error) {
	now = now.UTC()
	var locked federatedLoginStateModel
	unavailable := false

	err := store.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("state_hash = ?", hash[:]).First(&locked)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			unavailable = true
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock external login state: %w", result.Error)
		}

		if locked.Status != loginStateStatusActive || !locked.ExpiresAt.After(now) {
			unavailable = true
			if locked.Status == loginStateStatusActive && !locked.ExpiresAt.After(now) {
				if err := markLoginStateConsumed(transaction, locked.ID, now, loginStateStatusExpired); err != nil {
					return err
				}
			}
			return nil
		}

		if err := markLoginStateConsumed(transaction, locked.ID, now, loginStateStatusConsumed); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.State{}, err
	}
	if unavailable {
		return domain.State{}, errStateUnavailable
	}

	state, err := store.decryptState(ctx, locked)
	if err != nil {
		// The transaction already committed the terminal state. Return no detail because callbacks
		// must not distinguish malformed, unknown, expired or previously consumed state values.
		return domain.State{}, errStateUnavailable
	}
	return state, nil
}

func markLoginStateConsumed(transaction *gorm.DB, identifier string, now time.Time, status string) error {
	result := transaction.Model(&federatedLoginStateModel{}).
		Where("id = ? AND status = ?", identifier, loginStateStatusActive).
		Updates(map[string]any{"status": status, "consumed_at": now})
	if result.Error != nil {
		return fmt.Errorf("consume external login state: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errStateUnavailable
	}
	return nil
}

func (store *GORMStateStore) decryptState(ctx context.Context, row federatedLoginStateModel) (domain.State, error) {
	payloadBytes, err := store.protector.Decrypt(ctx, row.PayloadCiphertext)
	if err != nil {
		return domain.State{}, err
	}
	var payload persistedStatePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return domain.State{}, fmt.Errorf("decode external login state: %w", err)
	}
	if len(row.StateHash) != len(domain.State{}.StateHash) {
		return domain.State{}, errors.New("external login state hash has invalid length")
	}
	if len(payload.BrowserBindingHash) != len(domain.State{}.BrowserBindingHash) {
		return domain.State{}, errors.New("external login browser binding hash has invalid length")
	}

	var stateHash [32]byte
	copy(stateHash[:], row.StateHash)
	var browserBindingHash [32]byte
	copy(browserBindingHash[:], payload.BrowserBindingHash)
	return domain.State{
		StateHash: stateHash, BrowserBindingHash: browserBindingHash, TenantID: row.TenantID, ProviderCode: row.ProviderCode,
		Issuer: payload.Issuer, ClientID: payload.ClientID, RedirectURI: payload.RedirectURI,
		Discovery: payload.Discovery, Nonce: payload.Nonce, PKCEVerifier: payload.PKCEVerifier,
		ReturnTo: payload.ReturnTo, CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC(),
	}, nil
}

func validateStateForPersistence(state domain.State) error {
	if strings.TrimSpace(state.TenantID) == "" || strings.TrimSpace(state.ProviderCode) == "" || state.CreatedAt.IsZero() || state.ExpiresAt.IsZero() || !state.ExpiresAt.After(state.CreatedAt) {
		return errors.New("external login state is invalid")
	}
	if state.BrowserBindingHash == ([32]byte{}) {
		return errors.New("external login browser binding hash is invalid")
	}
	return nil
}

var _ application.StateStore = (*GORMStateStore)(nil)
