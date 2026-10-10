package trust

import (
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"testing"
)

func TestReviewedVendorKeyAndIsolation(t *testing.T) {
	a := Keys()
	key := a["vendor-v1"]
	raw, e := x509.MarshalPKIXPublicKey(key)
	if e != nil {
		t.Fatal(e)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "fcb4c05eea187300eb2c27c23fc1cab2b446a266038fda3c997fbe4d2ba365bd" {
		t.Fatal("vendor key fingerprint changed")
	}
	key[0] ^= 255
	if Keys()["vendor-v1"][0] == key[0] {
		t.Fatal("mutable public key map leaked")
	}
}
