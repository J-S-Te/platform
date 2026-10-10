// Package trust holds vendor keys baked into the reviewed customer release.
// Neither an imported license nor mutable runtime settings may add trusted keys.
package trust

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
)

// Vendor public key supplied by the owner. kid matches the issuer tool default.
// SPKI SHA256: fcb4c05eea187300eb2c27c23fc1cab2b446a266038fda3c997fbe4d2ba365bd.
const vendorPublicKey = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAaKc7dKpa6IPeM3+dRf9fai2jD8LvwKssmm68Up++un8=
-----END PUBLIC KEY-----`

// Keys returns a defensive copy. The vendor public key must be reviewed and
// changed through reviewed code, never through mutable runtime configuration.
// No test signing key is trusted.
func Keys() map[string]ed25519.PublicKey {
	block, _ := pem.Decode([]byte(vendorPublicKey))
	if block == nil {
		panic("invalid compiled vendor public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("invalid compiled vendor public key")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		panic("compiled vendor key is not Ed25519")
	}
	return map[string]ed25519.PublicKey{"vendor-v1": append(ed25519.PublicKey(nil), key...)}
}
