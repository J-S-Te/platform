package credentialcrypto

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type keyMetadata struct {
	KID            string     `json:"kid"`
	PrivateKeyFile string     `json:"private_key_file"`
	PublicKeyFile  string     `json:"public_key_file"`
	NotBefore      *time.Time `json:"not_before,omitempty"`
	NotAfter       *time.Time `json:"not_after,omitempty"`
}

type keyringMetadata struct {
	ActiveKID string        `json:"active_kid"`
	Keys      []keyMetadata `json:"keys"`
}

type Key struct {
	KID          string
	PrivateKey   *rsa.PrivateKey
	PublicKey    *rsa.PublicKey
	PublicKeyPEM string
	NotBefore    *time.Time
	NotAfter     *time.Time
}

type KeyRing struct {
	activeKID string
	keys      map[string]*Key
}

// KIDForPublicKey returns the stable lowercase SHA-256 fingerprint of SPKI DER.
func KIDForPublicKey(publicKey *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

func parsePrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key is not PKCS#8 or PKCS#1 RSA")
	}
	return key, nil
}

func parsePublicKey(raw []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("public key is not SPKI")
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not RSA")
	}
	return rsaKey, nil
}

// LoadKeyRing reads metadata and referenced PEM files. Paths are relative to the
// metadata file; absolute paths are rejected to keep deployments explicit.
func LoadKeyRing(metadataPath string, now time.Time) (*KeyRing, error) {
	metadataPath = strings.TrimSpace(metadataPath)
	if metadataPath == "" {
		return nil, fmt.Errorf("credential keyring path is required")
	}
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("read credential keyring: %w", err)
	}
	var metadata keyringMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode credential keyring: %w", err)
	}
	metadata.ActiveKID = strings.TrimSpace(metadata.ActiveKID)
	if metadata.ActiveKID == "" || len(metadata.Keys) == 0 {
		return nil, fmt.Errorf("credential keyring must declare active_kid and at least one key")
	}
	base := filepath.Dir(metadataPath)
	ring := &KeyRing{activeKID: metadata.ActiveKID, keys: make(map[string]*Key, len(metadata.Keys))}
	for _, item := range metadata.Keys {
		item.KID = strings.TrimSpace(item.KID)
		if item.KID == "" || ring.keys[item.KID] != nil {
			return nil, fmt.Errorf("credential keyring has empty or duplicate kid")
		}
		if item.NotBefore != nil && now.Before(*item.NotBefore) {
			continue
		}
		if item.NotAfter != nil && !now.Before(*item.NotAfter) {
			continue
		}
		paths := []string{item.PrivateKeyFile, item.PublicKeyFile}
		for _, value := range paths {
			if value == "" || filepath.IsAbs(value) {
				return nil, fmt.Errorf("credential key files must be relative to keyring metadata")
			}
		}
		privateRaw, err := os.ReadFile(filepath.Join(base, item.PrivateKeyFile))
		if err != nil {
			return nil, fmt.Errorf("read credential private key %s: %w", item.KID, err)
		}
		publicRaw, err := os.ReadFile(filepath.Join(base, item.PublicKeyFile))
		if err != nil {
			return nil, fmt.Errorf("read credential public key %s: %w", item.KID, err)
		}
		privateKey, err := parsePrivateKey(privateRaw)
		if err != nil {
			return nil, fmt.Errorf("parse credential private key %s: %w", item.KID, err)
		}
		publicKey, err := parsePublicKey(publicRaw)
		if err != nil {
			return nil, fmt.Errorf("parse credential public key %s: %w", item.KID, err)
		}
		if privateKey.N.Cmp(publicKey.N) != 0 || privateKey.E != publicKey.E {
			return nil, fmt.Errorf("credential keypair %s does not match", item.KID)
		}
		kid, err := KIDForPublicKey(publicKey)
		if err != nil {
			return nil, fmt.Errorf("derive credential kid: %w", err)
		}
		if kid != strings.ToLower(item.KID) {
			return nil, fmt.Errorf("credential key %s does not match SPKI fingerprint %s", item.KID, kid)
		}
		ring.keys[item.KID] = &Key{
			KID: item.KID, PrivateKey: privateKey, PublicKey: publicKey,
			PublicKeyPEM: string(publicRaw), NotBefore: item.NotBefore, NotAfter: item.NotAfter,
		}
	}
	if ring.keys[ring.activeKID] == nil {
		return nil, fmt.Errorf("credential keyring active kid is unavailable")
	}
	if ring.keys[ring.activeKID].PublicKeyPEM == "" {
		return nil, fmt.Errorf("credential keyring active public key is empty")
	}
	return ring, nil
}

func (ring *KeyRing) ActiveKID() string { return ring.activeKID }

func (ring *KeyRing) Key(kid string) (*Key, bool) {
	key, ok := ring.keys[kid]
	return key, ok
}

func (ring *KeyRing) ActiveKey() *Key { return ring.keys[ring.activeKID] }
