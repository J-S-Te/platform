package credentialhttp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/credentialcrypto"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type testRepo struct {
	challenges map[string]credentialcrypto.Challenge
	used       map[string]bool
}

func (r *testRepo) Create(_ context.Context, challenge credentialcrypto.Challenge) error {
	r.challenges[string(challenge.IDHash)] = challenge
	return nil
}
func (r *testRepo) Consume(_ context.Context, challenge credentialcrypto.Challenge) error {
	key := string(challenge.IDHash)
	stored, ok := r.challenges[key]
	if !ok || !bytes.Equal(stored.BindingHash, challenge.BindingHash) || stored.KID != challenge.KID || stored.Purpose != challenge.Purpose || stored.ResourceID != challenge.ResourceID || stored.RouteKey != challenge.RouteKey {
		return credentialcrypto.ErrChallengeInvalid
	}
	if r.used[key] {
		return credentialcrypto.ErrChallengeUsed
	}
	if !stored.ExpiresAt.After(time.Now().UTC()) {
		return credentialcrypto.ErrChallengeExpired
	}
	r.used[key] = true
	return nil
}
func (r *testRepo) DeleteExpired(context.Context, time.Time, int) (int64, error) { return 0, nil }

type noopAuth struct{}

func (noopAuth) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{}, context.Canceled
}
func (noopAuth) CookieName() string { return "bp_session" }

func newTestHandler(t *testing.T) (*Handler, *testRepo, *rsa.PrivateKey) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privDER, _ := x509.MarshalPKCS8PrivateKey(privateKey)
	pubDER, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	_ = os.WriteFile(filepath.Join(dir, "private.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "public.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)
	kid, _ := credentialcrypto.KIDForPublicKey(&privateKey.PublicKey)
	_ = os.WriteFile(filepath.Join(dir, "keyring.json"), []byte(`{"active_kid":"`+kid+`","keys":[{"kid":"`+kid+`","private_key_file":"private.pem","public_key_file":"public.pem"}]}`), 0o600)
	ring, err := credentialcrypto.LoadKeyRing(filepath.Join(dir, "keyring.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	repo := &testRepo{challenges: map[string]credentialcrypto.Challenge{}, used: map[string]bool{}}
	handler := &Handler{
		KeyRing: ring, Challenges: &credentialcrypto.ChallengeService{Repository: repo, TTL: 60 * time.Second},
		Mode: "compat", Authenticator: noopAuth{}, SessionCookie: "bp_session",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ResponseMaxBytes: 8 << 20,
	}
	return handler, repo, privateKey
}

func postJSON(t *testing.T, client *http.Client, url string, value any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(value)
	response, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response, body
}

func sealRequest(t *testing.T, pub *rsa.PublicKey, kid, challengeID, purpose, resourceID, contentType string, body []byte) credentialcrypto.RequestEnvelope {
	t.Helper()
	material := make([]byte, 64)
	_, _ = rand.Read(material)
	encrypted, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, material, []byte(credentialcrypto.OAEPLabel))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(material[:32])
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	aad, _ := credentialcrypto.AAD(credentialcrypto.DirectionRequest, kid, challengeID, purpose, resourceID, contentType)
	sealed := gcm.Seal(nil, iv, body, aad)
	return credentialcrypto.RequestEnvelope{
		Protocol: credentialcrypto.Protocol, KID: kid, ChallengeID: challengeID, Purpose: purpose,
		ResourceID: resourceID, ContentType: contentType,
		EncryptedKey: base64.RawURLEncoding.EncodeToString(encrypted),
		IV:           base64.RawURLEncoding.EncodeToString(iv),
		Ciphertext:   base64.RawURLEncoding.EncodeToString(sealed[:len(sealed)-16]),
		Tag:          base64.RawURLEncoding.EncodeToString(sealed[len(sealed)-16:]),
	}
}

func TestChallengeAndWrappedLogin(t *testing.T) {
	handler, _, privateKey := newTestHandler(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/credential-challenges", handler.IssueChallenge)
	mux.HandleFunc("/api/v1/auth/login", handler.WrapJSON(credentialcrypto.PurposeLogin, "auth.login", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte(`"password":"secret"`)) {
			t.Error("decrypted body not restored")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"OK","data":{"redirect_url":"/portal"}}`))
	}))
	server := httptest.NewServer(mux)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	response, body := postJSON(t, client, server.URL+"/api/v1/auth/credential-challenges", map[string]any{"purpose": "LOGIN", "resource_id": ""})
	if response.StatusCode != 200 {
		t.Fatalf("challenge status=%d body=%v", response.StatusCode, body)
	}
	data := body["data"].(map[string]any)
	kid, challengeID := data["kid"].(string), data["challenge_id"].(string)
	if kid == "" || challengeID == "" || data["public_key"] == "" {
		t.Fatal("challenge data missing")
	}
	envelope := sealRequest(t, &privateKey.PublicKey, kid, challengeID, "LOGIN", "", "application/json", []byte(`{"account":"localadmin","password":"secret"}`))
	response, body = postJSON(t, client, server.URL+"/api/v1/auth/login", map[string]any{"credential_envelope": envelope})
	if response.StatusCode != 200 {
		t.Fatalf("login status=%d body=%v", response.StatusCode, body)
	}
	encrypted := body["credential_envelope"].(map[string]any)
	if encrypted["protocol"] != credentialcrypto.Protocol {
		t.Fatal("response not encrypted")
	}
	// Reuse the same challenge must be rejected before the business handler.
	response, body = postJSON(t, client, server.URL+"/api/v1/auth/login", map[string]any{"credential_envelope": envelope})
	if response.StatusCode != 409 || body["code"] != "CREDENTIAL_CHALLENGE_USED" {
		t.Fatalf("replay status=%d body=%v", response.StatusCode, body)
	}
}

func TestMultipartEnvelopePreservesRawBodyAndContentType(t *testing.T) {
	handler, _, privateKey := newTestHandler(t)
	bindingValue := "session-token"
	tenantID := "01J00000000000000000000000"
	issued, err := handler.Challenges.Issue(context.Background(), handler.KeyRing.ActiveKID(), credentialcrypto.PurposeCreateEmployees, "", tenantID, credentialcrypto.HashBinding(bindingValue))
	if err != nil {
		t.Fatal(err)
	}
	rawBody := []byte("--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"users.csv\"\r\nContent-Type: text/csv\r\n\r\nname,account\n张三,zhangsan\n\r\n--boundary--\r\n")
	contentType := "multipart/form-data; boundary=boundary"
	envelope := sealRequest(t, &privateKey.PublicKey, handler.KeyRing.ActiveKID(), issued.Raw, credentialcrypto.PurposeCreateEmployees, "", contentType, rawBody)
	body, _ := json.Marshal(map[string]any{"credential_envelope": envelope})
	request := httptest.NewRequest(http.MethodPost, "http://platform.test/api/v1/employees/batch", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", "bp_session="+bindingValue)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: tenantID}, User: authctx.ReferenceName{ID: "operator-1"}}))
	handler.WrapJSONOrMultipart(credentialcrypto.PurposeCreateEmployees, "employee.batch", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=boundary") {
			t.Errorf("content type not restored: %q", r.Header.Get("Content-Type"))
		}
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, rawBody) {
			t.Errorf("raw multipart body changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"OK","data":{}}`))
	})(httptest.NewRecorder(), request)
}
