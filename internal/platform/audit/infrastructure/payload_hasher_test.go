package infrastructure

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// testAuditDatabase 与本包其他测试一致：DryRun 连接，不触达真实数据库。
func testAuditDatabase() *gorm.DB {
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		panic(err)
	}
	return db
}

func TestPayloadHasherWithoutKeyFallsBackToSHA256(t *testing.T) {
	hasher := payloadHasher{}
	payload := []byte(`{"EventID":"evt-1"}`)
	digest := hasher.Sum(payload)
	if len(digest) != 32 {
		t.Fatalf("digest length = %d, want 32", len(digest))
	}
	direct := sha256.Sum256(payload)
	if !bytes.Equal(digest, direct[:]) {
		t.Fatal("fallback digest must equal plain SHA-256")
	}
	if hasher.Algorithm() != PayloadHashAlgorithmSHA256 {
		t.Fatalf("algorithm = %q, want %q", hasher.Algorithm(), PayloadHashAlgorithmSHA256)
	}
	computed, matched, err := hasher.Verify(PayloadHashAlgorithmSHA256, payload, digest)
	if err != nil || !matched || !bytes.Equal(computed, digest) {
		t.Fatalf("verify sha256: matched=%v err=%v", matched, err)
	}
}

func TestPayloadHasherWithKeyUsesHMACAndNeverFallsBack(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	hasher := payloadHasher{key: key}
	payload := []byte(`{"EventID":"evt-1"}`)
	digest := hasher.Sum(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !bytes.Equal(digest, mac.Sum(nil)) {
		t.Fatal("keyed digest must equal HMAC-SHA256")
	}
	plain := sha256.Sum256(payload)
	if bytes.Equal(digest, plain[:]) {
		t.Fatal("keyed digest must differ from unkeyed SHA-256")
	}
	if hasher.Algorithm() != PayloadHashAlgorithmHMACSHA256 {
		t.Fatalf("algorithm = %q, want %q", hasher.Algorithm(), PayloadHashAlgorithmHMACSHA256)
	}
	if _, matched, err := hasher.Verify(PayloadHashAlgorithmHMACSHA256, []byte(`tampered`), digest); err != nil || matched {
		t.Fatalf("tampered payload must not match: matched=%v err=%v", matched, err)
	}
	if _, _, err := hasher.Verify(PayloadHashAlgorithmSHA256, payload, plain[:]); err != nil {
		t.Fatalf("legacy sha256 rows must stay verifiable: %v", err)
	}
}

func TestPayloadHasherVerifyRejectsUnknownAlgorithm(t *testing.T) {
	hasher := payloadHasher{}
	if _, _, err := hasher.Verify("MD5", []byte(`x`), nil); err == nil {
		t.Fatal("unknown algorithm must be rejected")
	}
}

func TestPayloadHasherVerifyHMACRowWithoutKeyFailsClosed(t *testing.T) {
	hasher := payloadHasher{}
	if _, _, err := hasher.Verify(PayloadHashAlgorithmHMACSHA256, []byte(`x`), []byte(bytes.Repeat([]byte{1}, 32))); !errors.Is(err, ErrPayloadHashKeyUnavailable) {
		t.Fatalf("err = %v, want ErrPayloadHashKeyUnavailable", err)
	}
}

func TestParsePayloadHashKeyAcceptsBase64AndRejectsShortOrInvalid(t *testing.T) {
	if key, err := parsePayloadHashKey(""); err != nil || key != nil {
		t.Fatalf("empty key = %v, %v", key, err)
	}
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	if key, err := parsePayloadHashKey(valid); err != nil || len(key) != 32 {
		t.Fatalf("valid key = %v, %v", key, err)
	}
	rawNoPadding := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	if key, err := parsePayloadHashKey(rawNoPadding); err != nil || len(key) != 32 {
		t.Fatalf("raw base64 key = %v, %v", key, err)
	}
	if _, err := parsePayloadHashKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("short key must be rejected")
	}
	if _, err := parsePayloadHashKey("!!!not base64!!!"); err == nil {
		t.Fatal("invalid base64 must be rejected")
	}
}

func TestPayloadHasherFromEnvFailsClosedInRequiredModeWithoutKey(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	t.Setenv(PayloadHashKeyEnv, "")
	if _, err := payloadHasherFromEnv(); !errors.Is(err, ErrPayloadHashKeyRequired) {
		t.Fatalf("err = %v, want ErrPayloadHashKeyRequired", err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	t.Setenv(PayloadHashKeyEnv, key)
	hasher, err := payloadHasherFromEnv()
	if err != nil || hasher.Algorithm() != PayloadHashAlgorithmHMACSHA256 {
		t.Fatalf("hasher = %+v err = %v", hasher, err)
	}
}

func TestPayloadHasherFromEnvAllowsSHA256FallbackOutsideRequiredMode(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	t.Setenv("APP_ENV", "dev")
	t.Setenv(PayloadHashKeyEnv, "")
	hasher, err := payloadHasherFromEnv()
	if err != nil || hasher.Algorithm() != PayloadHashAlgorithmSHA256 {
		t.Fatalf("hasher = %+v err = %v", hasher, err)
	}
}

func TestNewRepositoryFailsClosedWhenRequiredAndKeyMissing(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	t.Setenv(PayloadHashKeyEnv, "")
	if _, err := NewRepository(testAuditDatabase()); !errors.Is(err, ErrPayloadHashKeyRequired) {
		t.Fatalf("err = %v, want ErrPayloadHashKeyRequired", err)
	}
}

// 与传输层同语义：显式键可解析时用解析值，空值回退 APP_ENV=production，
// 显式值非法（如 "notabool"）按失败关闭返回 true。
func TestAuditRequiredFromEnvFailsClosedOnInvalidExplicitValue(t *testing.T) {
	cases := []struct {
		name         string
		explicit     string
		appEnv       string
		wantRequired bool
	}{
		{name: "非法显式值按失败关闭", explicit: "notabool", appEnv: "dev", wantRequired: true},
		{name: "空值回退 production 判定", explicit: "", appEnv: "production", wantRequired: true},
		{name: "空值非 production 不要求必开", explicit: "", appEnv: "dev", wantRequired: false},
		{name: "显式 false 在 production 下关闭必开", explicit: "false", appEnv: "production", wantRequired: false},
		{name: "显式 true 在非 production 下要求必开", explicit: "true", appEnv: "dev", wantRequired: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("PLATFORM_AUDIT_REQUIRED", testCase.explicit)
			t.Setenv("APP_ENV", testCase.appEnv)
			if required := auditRequiredFromEnv(); required != testCase.wantRequired {
				t.Fatalf("auditRequiredFromEnv() = %v, want %v (PLATFORM_AUDIT_REQUIRED=%q, APP_ENV=%q)",
					required, testCase.wantRequired, testCase.explicit, testCase.appEnv)
			}
		})
	}
}

// 非法显式值 + 未配置 payload hash key ⇒ 必开模式生效，构造函数失败关闭，
// 不再静默退回无键 SHA-256。这是本修复对 payloadHasherFromEnv 的预期影响。
func TestPayloadHasherFromEnvFailsClosedOnInvalidExplicitValue(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "notabool")
	t.Setenv("APP_ENV", "dev")
	t.Setenv(PayloadHashKeyEnv, "")
	if _, err := payloadHasherFromEnv(); !errors.Is(err, ErrPayloadHashKeyRequired) {
		t.Fatalf("err = %v, want ErrPayloadHashKeyRequired", err)
	}
}
