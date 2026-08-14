package security

import (
	"bytes"
	"encoding/base64"
	"testing"
)

const digestInputA = "customer\x00tenant\x00customer_portal\x00CRM-CUST-2001"
const digestInputB = "customer\x00tenant\x00customer_portal\x00CRM-CUST-2002"

func TestCustomerRefProtectorRoundTrip(t *testing.T) {
	encryptionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 32))
	digestKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x02}, 32))
	protector, err := NewCustomerRefProtector(encryptionKey, digestKey)
	if err != nil {
		t.Fatalf("NewCustomerRefProtector() error = %v", err)
	}
	ciphertext, err := protector.Encrypt("CRM-CUST-2001")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	plaintext, err := protector.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if plaintext != "CRM-CUST-2001" {
		t.Fatalf("Decrypt() = %q, want original value", plaintext)
	}
	digest, err := protector.Digest(digestInputA)
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	if len(digest) != 32 {
		t.Fatalf("Digest() length = %d, want 32", len(digest))
	}
	again, err := protector.Digest(digestInputA)
	if err != nil || !bytes.Equal(digest, again) {
		t.Fatalf("Digest() not deterministic: %v %v", digest, again)
	}
	other, err := protector.Digest(digestInputB)
	if err != nil || bytes.Equal(digest, other) {
		t.Fatalf("Digest() collision across distinct inputs")
	}
}

func TestCustomerRefProtectorTamperDetection(t *testing.T) {
	encryptionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x03}, 32))
	digestKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x04}, 32))
	protector, err := NewCustomerRefProtector(encryptionKey, digestKey)
	if err != nil {
		t.Fatalf("NewCustomerRefProtector() error = %v", err)
	}
	ciphertext, err := protector.Encrypt("CRM-CUST-2001")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := protector.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt() accepted tampered ciphertext")
	}
}

func TestCustomerRefProtectorFailsClosedWithoutKeys(t *testing.T) {
	protector, err := NewCustomerRefProtector("", "")
	if err != nil {
		t.Fatalf("NewCustomerRefProtector() error = %v", err)
	}
	if _, err := protector.Encrypt("x"); err == nil {
		t.Fatal("Encrypt() succeeded without encryption key")
	}
	if _, err := protector.Decrypt([]byte("x")); err == nil {
		t.Fatal("Decrypt() succeeded without encryption key")
	}
	if _, err := protector.Digest("x"); err == nil {
		t.Fatal("Digest() succeeded without digest key; must fail closed")
	}
}

func TestCustomerRefProtectorRejectsInvalidKeys(t *testing.T) {
	if _, err := NewCustomerRefProtector("not-base64!", ""); err == nil {
		t.Fatal("NewCustomerRefProtector() accepted invalid encryption key")
	}
	if _, err := NewCustomerRefProtector("", "not-base64!"); err == nil {
		t.Fatal("NewCustomerRefProtector() accepted invalid digest key")
	}
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := NewCustomerRefProtector(short, ""); err == nil {
		t.Fatal("NewCustomerRefProtector() accepted short encryption key")
	}
}
