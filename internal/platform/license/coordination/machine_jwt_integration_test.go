package coordination_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registrydomain "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	licensehttp "github.com/J-S-Te/Basic-Platform/internal/platform/license/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/migrations"
	runtime "github.com/J-S-Te/license-core/runtime"
	driver "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type machineRegistry struct {
	client registrydomain.OAuthClient
	hash   []byte
}

func (r *machineRegistry) FindForClientCredentials(_ context.Context, id string, _ time.Time) (registrydomain.OAuthClient, []registrydomain.ClientCredential, error) {
	if id != r.client.ClientID {
		return registrydomain.OAuthClient{}, nil, registry.ErrUnauthenticated
	}
	return r.client, []registrydomain.ClientCredential{{SecretHash: r.hash}}, nil
}
func (r *machineRegistry) FindActiveByID(_ context.Context, id string, _ time.Time) (registrydomain.OAuthClient, error) {
	if id != r.client.ID {
		return registrydomain.OAuthClient{}, registry.ErrUnauthenticated
	}
	return r.client, nil
}

type machineClock struct{ now time.Time }

func (c machineClock) Now() time.Time { return c.now }

type machineLicenseReader struct{ state domain.State }

func (r machineLicenseReader) Read(context.Context) (domain.State, error) { return r.state, nil }

type unusedMachineSigner struct{}

func (unusedMachineSigner) Sign(runtime.PlatformSnapshot) (string, error) {
	return "", errors.New("not used by ready")
}
func (unusedMachineSigner) Verify(string, runtime.Binding) (runtime.PlatformSnapshot, error) {
	return runtime.PlatformSnapshot{}, errors.New("not used by ready")
}

func machineTokenService(t *testing.T, now time.Time) (*registry.Service, *machineRegistry, *security.ApplicationJWTManager) {
	t.Helper()
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
	dir := t.TempDir()
	privatePath, publicPath := filepath.Join(dir, "private.pem"), filepath.Join(dir, "public.pem")
	if err = os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := security.LoadApplicationJWTManager("test-issuer", "test-audience", privatePath, publicPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("test-only-runtime-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	repo := &machineRegistry{hash: hash, client: registrydomain.OAuthClient{ID: "database-pk", ClientID: "jwt-runtime-external", TenantID: "jwt-tenant", ApplicationID: "jwt-app", ApplicationCode: "contract_management", EnvironmentID: "jwt-env", EnvironmentCode: "prod", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 300, GrantTypes: map[string]struct{}{"client_credentials": {}}, Scopes: map[string]struct{}{"license.runtime": {}}}}
	svc, err := registry.NewService(repo, manager, machineClock{now})
	if err != nil {
		t.Fatal(err)
	}
	return svc, repo, manager
}

// This native test uses the actual issuance and current-registration authentication,
// not a fabricated Principal. SQL integration below then passes it to the real handler.
func TestRuntimeMachineJWTSeparatesDatabaseAndExternalIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, repo, manager := machineTokenService(t, now)
	token, err := svc.IssueClientCredentials(context.Background(), repo.client.ClientID, "test-only-runtime-secret", []string{"license.runtime"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Authenticate(context.Background(), token.AccessToken)
	if err != nil || p.OAuthClientID != repo.client.ID || p.ClientID != repo.client.ClientID || p.ClientID == p.OAuthClientID {
		t.Fatal("real JWT identity semantics lost", err)
	}
	claims, err := manager.Verify(token.AccessToken, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"wrong-pk", "wrong-external", "wrong-app", "wrong-env", "wrong-scope", "wrong-tenant"} {
		t.Run(kind, func(t *testing.T) {
			c := claims
			switch kind {
			case "wrong-pk":
				c.OAuthClientID = "other-pk"
			case "wrong-external":
				c.ClientID = "other-client"
			case "wrong-app":
				c.ApplicationCode = "customer_portal"
			case "wrong-env":
				c.EnvironmentCode = "test"
			case "wrong-scope":
				c.Scopes = []string{"unknown.scope"}
			case "wrong-tenant":
				c.TenantID = "other-tenant"
			}
			raw, err := manager.Issue(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Authenticate(context.Background(), raw); !errors.Is(err, registry.ErrUnauthenticated) {
				t.Fatal("mismatched signed binding accepted", kind, err)
			}
		})
	}
}

func TestMySQLRealMachineJWTToRuntimeHandler(t *testing.T) {
	dsn := os.Getenv("LICENSE_TEST_DSN")
	if dsn == "" {
		t.Skip("LICENSE_TEST_DSN is not configured")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid dedicated test DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || (host != "127.0.0.1" && host != "localhost") || cfg.DBName != "platform_license_test" {
		t.Fatal("isolated loopback platform_license_test required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	// Every dedicated SQL test bootstraps its own schema, including fresh DSNs;
	// Go test ordering must not provide an implicit migration dependency.
	if _, err = migration.Run(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	// Fixture changes are rolled back, including ready_at; no shared license identity is replaced.
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() {
		if err := tx.Rollback().Error; err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC().Truncate(time.Second)
	var previous domain.Deployment
	if err = tx.First(&previous, "id = ?", 1).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	d := domain.Deployment{ID: 1, InstanceID: "jwt-instance", CustomerID: "jwt-customer", Environment: "production", Revision: 1, HighestObservedAt: now.Unix()}
	if err = tx.Save(&d).Error; err != nil {
		t.Fatal(err)
	}
	m := coordination.Member{ServiceID: "jwt-test-contract-api", Application: "contract_management", Environment: "prod", OAuthClientID: "jwt-runtime-external", CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64), CreatedAt: now}
	if err = tx.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	reader := machineLicenseReader{domain.State{InstanceID: d.InstanceID, CustomerID: d.CustomerID, Environment: d.Environment, Revision: d.Revision}}
	coordinator, err := coordination.NewService(tx, reader, nil, unusedMachineSigner{}, func() time.Time { return now }, coordination.WithApplicationEnvironment("prod"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := licensehttp.NewRuntimeHandler(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	svc, repo, _ := machineTokenService(t, now)
	for _, kind := range []string{"valid", "wrong-external", "pk-only", "wrong-app", "wrong-env", "missing-scope"} {
		t.Run(kind, func(t *testing.T) {
			client := repo.client
			switch kind {
			case "wrong-external":
				client.ClientID = "other-external"
			case "pk-only":
				client.ID = m.OAuthClientID
				client.ClientID = "other-external"
			case "wrong-app":
				client.ApplicationCode = "customer_portal"
			case "wrong-env":
				client.EnvironmentCode = "test"
			case "missing-scope":
				client.Scopes = map[string]struct{}{"other.scope": {}}
			}
			original := repo.client
			repo.client = client
			defer func() { repo.client = original }()
			token, err := svc.IssueClientCredentials(context.Background(), client.ClientID, "test-only-runtime-secret", nil)
			if err != nil {
				t.Fatal(err)
			}
			p, err := svc.Authenticate(context.Background(), token.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/api/v1/internal/licenses/runtime/"+m.ServiceID+"/ready", strings.NewReader(`{"protocol":1,"coverage_digest":"`+m.CoverageDigest+`","image_digest":"`+m.ImageDigest+`"}`))
			r = r.WithContext(appctx.WithPrincipal(r.Context(), p))
			w := httptest.NewRecorder()
			handler.Ready(w, r)
			want := 403
			if kind == "valid" {
				want = 200
			}
			if w.Code != want {
				t.Fatal("real authenticated handler boundary mismatch", kind, w.Code, w.Body.String())
			}
		})
	}
	var result coordination.Member
	if err = tx.First(&result, "service_id = ?", m.ServiceID).Error; err != nil || result.ReadyAt == nil {
		t.Fatal("real authorized Ready did not persist", err)
	}
}
