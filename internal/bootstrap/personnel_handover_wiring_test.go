package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"gorm.io/gorm"
)

// handoverTestKeys 是用临时目录里的 Ed25519 密钥模拟平台签名材料的测试夹具；
// 交接令牌由平台自签，测试通过公开验签器回读 claims，避免只断言"有字符串"。
type handoverTestKeys struct {
	issuer      string
	audience    string
	application string
	environment string
	privatePath string
	publicPath  string
}

func newHandoverTestKeys(t *testing.T) handoverTestKeys {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "jwt-ed25519.pem")
	publicPath := filepath.Join(directory, "jwt-ed25519-public.pem")
	encodedPrivate, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedPrivate}), 0o600); err != nil {
		t.Fatal(err)
	}
	encodedPublic, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encodedPublic}), 0o600); err != nil {
		t.Fatal(err)
	}
	return handoverTestKeys{
		issuer: "basic-platform", audience: "basic-platform-application", application: "platform", environment: "prod",
		privatePath: privatePath, publicPath: publicPath,
	}
}

func handoverConfigForTest(keys handoverTestKeys, enabled bool, urls string) config.Config {
	return config.Config{
		Auth: config.AuthConfig{
			JWTIssuer: keys.issuer, ApplicationJWTAudience: keys.audience,
			JWTPrivateKeyPath: keys.privatePath, JWTPublicKeyPath: keys.publicPath,
		},
		Handover: config.HandoverConfig{
			Enabled: enabled, Environment: "prod",
			MachineClientID: "platform-handover", MachineApplicationID: keys.application,
			MachineEnvironmentID: keys.environment, MachineScope: "handover.machine",
			SystemURLs: handoverSystemURLsForTest(urls), Timeout: 5 * time.Second,
		},
	}
}

type workerFinishBoundary struct{ calls int }

func (c *workerFinishBoundary) Finish(_ context.Context, batch, request, outcome string) error {
	if batch != "batch-1" || request != "request-1" || outcome != "COMPLETED" {
		return errors.New("unexpected finish identity")
	}
	c.calls++
	return nil
}

func TestPersonnelWorkerHandoverCannotStartCancelOrTransfer(t *testing.T) {
	ctx := context.Background()
	worker := personnelWorkerHandover{}
	if _, err := worker.Start(ctx, "tenant", "request", "user"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can start handover")
	}
	if err := worker.CancelByRequest(ctx, "tenant", "request"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can cancel handover")
	}
	boundary := &workerFinishBoundary{}
	client := personnelWorkerFinishClient{finisher: boundary}
	if _, err := client.Freeze(ctx, application.HandoverFreezeRequest{}); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can freeze")
	}
	if _, err := client.Responsibilities(ctx, "request", "user", "", 1); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can discover")
	}
	if _, err := client.Transfer(ctx, application.HandoverTransferRequest{}); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can transfer")
	}
	if _, err := client.Receipt(ctx, "item", "key"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can reconcile transfers")
	}
	if err := client.Finish(ctx, "batch-1", "request-1", "CANCELLED"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can release cancellation")
	}
	if boundary.calls != 0 {
		t.Fatal("denied operation reached machine client")
	}
	if err := client.Finish(ctx, "batch-1", "request-1", "COMPLETED"); err != nil || boundary.calls != 1 {
		t.Fatalf("finish unavailable: calls=%d err=%v", boundary.calls, err)
	}
}

// TestPersonnelWorkerCompletionRequiresSigningKeys 钉住 fail-closed：交接启用但平台签名
// 密钥不可用时，worker 必须在装配期失败，而不是运行期发不出令牌或退化成静态 Bearer。
func TestPersonnelWorkerCompletionRequiresSigningKeys(t *testing.T) {
	keys := newHandoverTestKeys(t)
	cfg := handoverConfigForTest(keys, true, "contract=https://contract.internal")
	cfg.Auth.JWTPrivateKeyPath = filepath.Join(t.TempDir(), "missing-private.pem")
	if _, err := buildPersonnelWorkerService(cfg, &gorm.DB{}); err == nil {
		t.Fatal("worker completion silently accepts missing signing keys")
	}
}

func handoverSystemURLsForTest(raw string) map[string]string {
	systems := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		code, target, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			continue
		}
		systems[code] = target
	}
	return systems
}

// TestHandoverTokenSourceBindsBatchTenantAndScope 校验平台自签的 application JWT：
// 客户端/应用/环境/scope 固定，租户来自批次，有效期不超过 5 分钟。
func TestHandoverTokenSourceBindsBatchTenantAndScope(t *testing.T) {
	keys := newHandoverTestKeys(t)
	cfg := handoverConfigForTest(keys, true, "contract=https://contract.internal")
	tokenSource, err := loadHandoverTokenSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := security.LoadApplicationJWTVerifier(keys.issuer, keys.audience, keys.publicPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"tenant-1", "tenant-2"} {
		token, err := tokenSource(tenant)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := verifier.Verify(token, time.Now().UTC())
		if err != nil {
			t.Fatalf("tenant %s token is not verifiable: %v", tenant, err)
		}
		if claims.TenantID != tenant || claims.ClientID != "platform-handover" || claims.OAuthClientID != "platform-handover" {
			t.Fatalf("tenant %s claims = %+v", tenant, claims)
		}
		if claims.ApplicationID != keys.application || claims.ApplicationCode != keys.application ||
			claims.EnvironmentID != keys.environment || claims.EnvironmentCode != "prod" {
			t.Fatalf("tenant %s application binding = %+v", tenant, claims)
		}
		if len(claims.Scopes) != 1 || claims.Scopes[0] != "handover.machine" {
			t.Fatalf("tenant %s scopes = %v", tenant, claims.Scopes)
		}
		if ttl := claims.ExpiresAt.Sub(claims.IssuedAt); ttl != handoverTokenTTL {
			t.Fatalf("tenant %s token ttl = %s, want %s", tenant, ttl, handoverTokenTTL)
		}
	}
	if _, err := tokenSource("  "); err == nil {
		t.Fatal("token source must reject an empty tenant")
	}
}

func TestBuildHandoverCoordinatorSkipsWhenDisabled(t *testing.T) {
	keys := newHandoverTestKeys(t)
	coordinator, err := buildHandoverCoordinator(handoverConfigForTest(keys, false, "contract=https://contract.internal"), &gorm.DB{}, inventoryRegistryFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator != nil {
		t.Fatal("disabled handover must not inject a coordinator")
	}
}

func TestBuildHandoverCoordinatorBuildsClientsPerSystem(t *testing.T) {
	keys := newHandoverTestKeys(t)
	tokenSource, err := loadHandoverTokenSource(handoverConfigForTest(keys, true, "Contract=https://contract.internal,crm=https://crm.internal"))
	if err != nil {
		t.Fatal(err)
	}
	clients, err := handoverClientsFromConfig(handoverConfigForTest(keys, true, "Contract=https://contract.internal,crm=https://crm.internal"), tokenSource, func() (string, error) { return "01REQUESTID0000000000000000", nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 || clients["contract"] == nil || clients["crm"] == nil {
		t.Fatalf("clients = %v", clients)
	}
}

func TestBuildHandoverCoordinatorFailsClosedWithoutTokenSource(t *testing.T) {
	keys := newHandoverTestKeys(t)
	if _, err := handoverClientsFromConfig(handoverConfigForTest(keys, true, "contract=https://contract.internal"), nil, func() (string, error) { return "01REQUESTID0000000000000000", nil }); err == nil {
		t.Fatal("missing application token source must fail closed even if config validation is bypassed")
	}
}

func TestBuildHandoverCoordinatorRequiresRegistry(t *testing.T) {
	keys := newHandoverTestKeys(t)
	if _, err := buildHandoverCoordinator(handoverConfigForTest(keys, true, "contract=https://contract.internal"), &gorm.DB{}, nil, nil); err == nil {
		t.Fatal("missing application registry must fail closed")
	}
}

func TestBuildHandoverCoordinatorAssemblesOrchestrator(t *testing.T) {
	keys := newHandoverTestKeys(t)
	coordinator, err := buildHandoverCoordinator(handoverConfigForTest(keys, true, "contract=https://contract.internal"), &gorm.DB{}, inventoryRegistryFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator == nil {
		t.Fatal("enabled handover must assemble an orchestrator")
	}
	// 未就绪的授权读取端必须报错（inventory 会归类为 UNREACHABLE），不得静默放行。
	if _, err := (lazyHandoverLicenseReader{}).Status(context.Background(), "contract_management"); err == nil {
		t.Fatal("unresolved license reader must return an error")
	}
}

func TestPersonnelWorkerWiringIncludesHandoverVerifierWhenEnabled(t *testing.T) {
	keys := newHandoverTestKeys(t)
	for _, enabled := range []bool{false, true} {
		cfg := handoverConfigForTest(keys, enabled, "contract=https://contract.internal")
		service, err := buildPersonnelWorkerService(cfg, &gorm.DB{})
		if err != nil {
			t.Fatal(err)
		}
		if service.HandoverOrchestrationEnabled() != enabled {
			t.Fatalf("worker cross-system verifier enabled=%v", enabled)
		}
	}
	if _, err := buildPersonnelWorkerService(config.Config{}, nil); err == nil {
		t.Fatal("worker missing handover checker must fail startup")
	}
}
