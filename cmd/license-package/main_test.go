package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	core "github.com/J-S-Te/license-core"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerifyDelivery(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1800000000, 0)
	l := core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "delivery-test", Version: 1, CustomerID: "customer", ProductID: core.Product, Environment: "production", InstanceID: "instance", IssuedAt: now.Unix(), NotBefore: now.Unix(), Applications: []core.Application{{Code: "contract_management", Kind: "FULL", NotBefore: now.Unix(), ExpiresAt: now.Unix() + 86400}}}
	raw, err := core.Sign(l, "test", priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "license.jws")
	bytes := []byte(raw + "\n")
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"test": pub}
	out, err := verify(path, keys, now)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(bytes)
	if out.Digest != hex.EncodeToString(h[:]) || out.InstanceID != "instance" {
		t.Fatal("metadata mismatch")
	}
	if _, err := verify(path, map[string]ed25519.PublicKey{}, now); err == nil {
		t.Fatal("untrusted signature accepted")
	}
	if _, err := verify(path, keys, now.Add(24*time.Hour)); err == nil {
		t.Fatal("expired license accepted")
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(link, keys, now); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.WriteFile(path, []byte("forged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(path, keys, now); err == nil {
		t.Fatal("forgery accepted")
	}
}
