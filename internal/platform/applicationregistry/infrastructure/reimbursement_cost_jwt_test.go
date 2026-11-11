package infrastructure

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"golang.org/x/crypto/bcrypt"
)

type costJWTRepository struct {
	client domain.OAuthClient
	hash   []byte
}

// FindForClientCredentials 返回独立合成用途的登记及密钥摘要。
// 参数：ctx为上下文，id为OAuth标签，now为签发时刻。
// 返回值：匹配的合成登记；错误标签返回认证失败。
func (r costJWTRepository) FindForClientCredentials(ctx context.Context, id string, now time.Time) (domain.OAuthClient, []domain.ClientCredential, error) {
	if id != r.client.ClientID {
		return domain.OAuthClient{}, nil, application.ErrUnauthenticated
	}
	return r.client, []domain.ClientCredential{{SecretHash: r.hash}}, nil
}

// FindActiveByID 返回注册主键对应的合成登记。
// 参数：ctx为上下文，id为注册主键，now为认证时刻。
// 返回值：登记或认证失败，不连接数据库。
func (r costJWTRepository) FindActiveByID(ctx context.Context, id string, now time.Time) (domain.OAuthClient, error) {
	if id != r.client.ID {
		return domain.OAuthClient{}, application.ErrUnauthenticated
	}
	return r.client, nil
}

// TestReimbursementCostActualJWTBinding 验证真实服务与签发器的字段形状对接接收配置。
// 参数：t为仅使用合成Ed25519密钥及secret的测试上下文。
// 返回值：无；字段反向、时效或JWT线格式漂移时失败，不输出令牌。
func TestReimbursementCostActualJWTBinding(t *testing.T) {
	input := costReceiverFixture()
	source := input.RelatedServiceClients[0]
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	privatePath, publicPath := filepath.Join(t.TempDir(), "private.pem"), filepath.Join(t.TempDir(), "public.pem")
	for path, block := range map[string]*pem.Block{privatePath: {Type: "PRIVATE KEY", Bytes: privDER}, publicPath: {Type: "PUBLIC KEY", Bytes: pubDER}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := security.LoadApplicationJWTManager("basic-platform", "basic-platform-application", privatePath, publicPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("synthetic-cost-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	repo := costJWTRepository{client: domain.OAuthClient{ID: source.Client.ID, ClientID: source.Client.ClientID,
		TenantID: input.TenantID, ApplicationID: source.ApplicationID, ApplicationCode: "settlement",
		EnvironmentID: source.EnvironmentID, EnvironmentCode: input.Environment, TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds: 900, GrantTypes: map[string]struct{}{"client_credentials": {}}, Scopes: map[string]struct{}{"reimbursement.cost_candidate.read": {}}}, hash: hash}
	svc, err := application.NewService(repo, manager, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.IssueClientCredentials(context.Background(), source.Client.ClientID, "synthetic-cost-secret", []string{"reimbursement.cost_candidate.read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(token.AccessToken, time.Now().UTC()); err != nil {
		t.Fatal("real signature verification failed")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token.AccessToken, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Subject  string   `json:"sub"`
		OAuthID  string   `json:"oauth_client_id"`
		Audience string   `json:"aud"`
		Scope    []string `json:"scope"`
		IAT      int64    `json:"iat"`
		NBF      int64    `json:"nbf"`
		EXP      int64    `json:"exp"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal("actual wire types differ")
	}
	values := map[string]string{}
	if err := reimbursementCostRuntimeValues(input, values); err != nil {
		t.Fatal(err)
	}
	if wire.Subject != values["REIMBURSEMENT_COST_MACHINE_CLIENT_ID"] || wire.OAuthID != values["REIMBURSEMENT_COST_MACHINE_OAUTH_CLIENT_ID"] {
		t.Fatal("actual signer identities do not match delivered receiver trust")
	}
	if wire.Audience != values["REIMBURSEMENT_COST_MACHINE_AUDIENCE"] || len(wire.Scope) != 1 || wire.Scope[0] != "reimbursement.cost_candidate.read" || wire.EXP-wire.IAT != 900 || wire.NBF != wire.IAT || token.ExpiresIn != 900 {
		t.Fatal("actual scope, audience or lifetime differs")
	}
}
