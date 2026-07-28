package sm3

import (
	"encoding/hex"
	"testing"
)

func TestStandardVector(t *testing.T) {
	got := Sum([]byte("abc"))
	const want = "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"
	if encoded := hex.EncodeToString(got[:]); encoded != want {
		t.Fatalf("SM3(abc) = %s, want %s", encoded, want)
	}
}
