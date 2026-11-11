//go:build license_closure

package coordination_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registryinfra "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	licenseapplication "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	licenseinfra "github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	licensehttp "github.com/J-S-Te/Basic-Platform/internal/platform/license/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/migrations"
	core "github.com/J-S-Te/license-core"
	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
	"github.com/J-S-Te/license-core/syncclient"
	driver "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const closureTenant = "01CLOSURE00000000000000001"

type closureSigner struct {
	private ed25519.PrivateKey
	public  map[string]ed25519.PublicKey
	vendor  map[string]ed25519.PublicKey
}

// Sign 使用本轮独立平台密钥签署协调器实际生成的快照。
// 参数：snapshot 为真实协调状态机提供的完整快照。
// 返回值：真实 Ed25519 JWS；格式或签名失败返回错误。
func (s closureSigner) Sign(snapshot runtime.PlatformSnapshot) (string, error) {
	return runtime.SignSnapshot(snapshot, "closure-platform", s.private)
}

// Verify 同时校验临时平台签名、临时厂商签名及固定消费方绑定。
// 参数：raw 为原始签名快照，binding 为预登记的消费方坐标。
// 返回值：已验签快照；篡改、未知信任或绑定错误时拒绝。
func (s closureSigner) Verify(raw string, binding runtime.Binding) (runtime.PlatformSnapshot, error) {
	return runtime.VerifySnapshot(raw, binding, s.public, s.vendor)
}

type closureConsumer struct {
	spec   coordination.ServiceSpec
	path   string
	local  *runtime.FileRuntime
	client *syncclient.Client
	token  syncclient.TokenSource
}

type closureFixture struct {
	db          *gorm.DB
	license     *licenseapplication.Service
	coordinator *coordination.Service
	machines    *registry.Service
	signer      closureSigner
	instance    string
	expires     time.Time
	consumers   []closureConsumer
}

// TestReimbursementMySQLLicenseClosure 验证报销双消费方的真实许可协议闭环。
// 参数：t 为测试上下文；数据库仅来自本轮新容器的严格隔离环境变量。
// 返回值：无；任何身份、持久化、签名或自然状态推进不符合预期即失败。
func TestReimbursementMySQLLicenseClosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := closureDatabase(t, ctx)
	f := newClosureFixture(t, ctx, db)
	server := closureHTTPServer(t, f.coordinator, f.machines)
	defer server.Close()
	f.consumers = closureConsumers(t, ctx, f, server.URL)
	for _, consumer := range f.consumers {
		if err := consumer.local.Evaluate(ctx, core.MUTATE_BUSINESS, time.Now()); !errors.Is(err, runtime.ErrState) {
			t.Fatalf("未收到签名快照的 %s 未失败关闭", consumer.spec.ServiceID)
		}
	}
	closureRejectForeignMember(t, ctx, f, server.URL)
	closureSyncRound(t, ctx, f.consumers)
	ready := closureStatus(t, ctx, f)
	if ready.State != runtime.Pending || ready.MigrationEligible {
		t.Fatal("新安装尚未激活却取得强制执行或迁移资格")
	}
	for _, member := range ready.Members {
		if member.ReadyAt == nil {
			t.Fatal("真实同步未产生 READY")
		}
	}
	actor := domain.Actor{TenantID: closureTenant, UserID: "closure-operator"}
	if err := f.coordinator.BeginActivation(ctx, "reimbursement", ready.Revision, actor); err != nil {
		t.Fatalf("显式激活失败：%v", err)
	}
	if applying := closureStatus(t, ctx, f); applying.State != runtime.Applying {
		t.Fatal("激活未自然进入 APPLYING")
	}
	closureSyncRound(t, ctx, f.consumers)
	if enforced := closureStatus(t, ctx, f); enforced.State != runtime.Enforced {
		t.Fatal("双成员 ACK 未自然进入 ENFORCED")
	}
	// ENFORCED 会产生新修订；再次真实拉取、落盘和 ACK，不能沿用 APPLYING 的确认。
	closureSyncRound(t, ctx, f.consumers)
	final := closureStatus(t, ctx, f)
	closureAssertCurrent(t, ctx, f, final)
	closureAssertExpiryAndRestart(t, ctx, f, final.Revision)
	t.Logf("协议闭环通过：成员数=%d，当前修订=%d，状态=%s，快照当前=%t",
		len(final.Members), final.Revision, final.State, final.SnapshotCurrent)
}

// closureDSN 在任何网络访问前校验专用库、数字回环地址与本轮容器声明。
// 参数：t 为测试上下文，dsn 为调用方显式提供的隔离测试 DSN。
// 返回值：已校验的驱动配置；不输出 DSN、用户名、口令或运行凭据。
func closureDSN(t *testing.T, dsn string) *driver.Config {
	t.Helper()
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("隔离测试 DSN 格式无效")
	}
	host, rawPort, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(rawPort)
	if err != nil || portErr != nil || config.Net != "tcp" ||
		(host != "127.0.0.1" && host != "::1") || port < 1024 || port == 3306 ||
		config.DBName != "platform_license_test" || !config.ParseTime || config.Loc != time.UTC {
		t.Fatal("要求数字回环高位端口、platform_license_test、parseTime=true 和 loc=UTC")
	}
	run := os.Getenv("LICENSE_CLOSURE_RUN_ID")
	container := os.Getenv("LICENSE_CLOSURE_CONTAINER_ID")
	uuid := os.Getenv("LICENSE_CLOSURE_SERVER_UUID")
	if !regexp.MustCompile(`^reimbursement-license-closure-20261110-[a-z0-9-]{8,64}$`).MatchString(run) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(container) ||
		!regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`).MatchString(uuid) {
		t.Fatal("缺少本轮新容器的独立运行号、完整容器 ID 或 MySQL UUID 声明")
	}
	return config
}

// closureDatabase 只在身份匹配且零表的全新隔离库上运行真实迁移。
// 参数：t 为测试上下文，ctx 为本轮有界请求上下文。
// 返回值：已迁移数据库；不创建容器、不清库，失败数据交主任务受控核验和清理。
func closureDatabase(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LICENSE_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置专用 LICENSE_TEST_DSN，未执行真实协议集成")
	}
	closureDSN(t, dsn)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("无法打开本轮专用测试库；未输出连接材料")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal("无法取得隔离测试连接池")
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error("关闭测试连接池失败")
		}
	})
	db = db.WithContext(ctx)
	var identity struct{ Hostname, ServerUUID, SchemaName string }
	if err := db.Raw("SELECT @@hostname AS hostname, @@server_uuid AS server_uuid, DATABASE() AS schema_name").Scan(&identity).Error; err != nil {
		t.Fatal("无法只读核对隔离数据库身份")
	}
	if identity.Hostname != os.Getenv("LICENSE_CLOSURE_CONTAINER_ID")[:12] ||
		identity.ServerUUID != os.Getenv("LICENSE_CLOSURE_SERVER_UUID") || identity.SchemaName != "platform_license_test" {
		t.Fatal("数据库与本轮新容器声明不一致，拒绝迁移或写入")
	}
	var tables int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables).Error; err != nil || tables != 0 {
		t.Fatal("必须使用零表新库；不会清理或复用既有安装")
	}
	if _, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatalf("真实迁移失败：%v", err)
	}
	return db
}

// newClosureFixture 通过真实仓储导入临时厂商签名许可证并构造协调器。
// 参数：t、ctx 为测试上下文，db 为已核验的全新迁移库。
// 返回值：仅携带临时信任和真实服务的夹具，不直接写许可或协调状态。
func newClosureFixture(t *testing.T, ctx context.Context, db *gorm.DB) *closureFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	platformPublic, platformPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vendor := map[string]ed25519.PublicKey{"closure-vendor": public}
	repository, err := licenseinfra.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := licenseapplication.NewService(repository, vendor, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f := &closureFixture{db: db, license: service, instance: "reimbursement-closure-instance"}
	f.expires = time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	closureImport(t, ctx, f, private)
	f.signer = closureSigner{platformPrivate, map[string]ed25519.PublicKey{"closure-platform": platformPublic}, vendor}
	f.coordinator, err = coordination.NewService(db, service, vendor, f.signer, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	closureRegistryParents(t, ctx, db)
	machineRepo, err := registryinfra.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	f.machines, err = registry.NewService(machineRepo, closureJWTManager(t), registry.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// closureImport 使用 ImportDelivery 在空安装上持久化临时真实签名授权。
// 参数：t、ctx 为测试上下文，f 为真实服务，private 为本轮厂商测试私钥。
// 返回值：无；导入失败、错误身份或非当前许可证均使测试失败。
func closureImport(t *testing.T, ctx context.Context, f *closureFixture, private ed25519.PrivateKey) {
	t.Helper()
	now := time.Now().UTC().Unix()
	raw, err := core.Sign(core.License{
		ProtocolVersion: 1, Issuer: core.Issuer, ProductID: core.Product,
		ID: "reimbursement-closure-license", CustomerID: "closure-customer",
		Environment: "production", InstanceID: f.instance, Version: 1,
		IssuedAt: now - 30, NotBefore: now - 30,
		Applications: []core.Application{{Code: "reimbursement", NotBefore: now - 30,
			ExpiresAt: f.expires.Unix(), Kind: "FULL"}},
	}, "closure-vendor", private)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{TenantID: closureTenant, UserID: "closure-operator"}
	imported, err := f.license.ImportDelivery(ctx, raw, "production", actor)
	if err != nil || imported.Status != "IMPORTED" || imported.State.InstanceID != f.instance || imported.State.CurrentVersion != 1 {
		t.Fatalf("真实交付导入未完成：%v", err)
	}
}

// closureJWTManager 生成独立机器 JWT 身份并由正式安全组件装载。
// 参数：t 为测试上下文，临时密钥仅位于本轮私有临时目录。
// 返回值：真实 JWT 签发校验器；不修改任何正式信任或厂商密钥。
func closureJWTManager(t *testing.T) *security.ApplicationJWTManager {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath, publicPath := filepath.Join(dir, "machine-private.pem"), filepath.Join(dir, "machine-public.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := security.LoadApplicationJWTManager("closure-issuer", "closure-runtime", privatePath, publicPath)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// closureRegistryParents 创建夹具租户及真实应用环境登记。
// 参数：t、ctx 为测试上下文，db 为零安装迁移后的专用数据库。
// 返回值：无；只写夹具身份，通过原管理仓储创建应用与环境。
func closureRegistryParents(t *testing.T, ctx context.Context, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.WithContext(ctx).Table("iam_tenant").Create(map[string]any{
		"id": closureTenant, "code": "closure-tenant", "name": "隔离协议验收租户",
		"timezone": "Asia/Shanghai", "locale": "zh-CN", "status": "ACTIVE",
		"version": 1, "created_at": now, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	repo, err := registryinfra.NewManagementRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	appID, envID := strings.Repeat("a", 26), strings.Repeat("e", 26)
	if _, err := repo.CreateApplication(ctx, registry.ApplicationCreateInput{
		TenantID: closureTenant, Code: "reimbursement", Name: "隔离报销协议验收",
		ApplicationType: "BUSINESS", Status: "ACTIVE",
	}, appID, now); err != nil {
		t.Fatal(err)
	}
	issuerAlias := "platform"
	if _, err := repo.CreateEnvironment(ctx, registry.EnvironmentCreateInput{
		TenantID: closureTenant, ApplicationID: appID, Environment: "production", Status: "ACTIVE",
		IssuerAlias: &issuerAlias,
	}, envID, now); err != nil {
		t.Fatal(err)
	}
}

// closureRegisterMachine 通过受控管理仓储创建成员专属凭据并保留内外身份区别。
// 参数：t、ctx 为测试上下文，db 为专用库，spec 为固定目录服务，index 区分数据库主键。
// 返回值：只供内存中真实签发使用的临时随机 secret；不输出或落盘明文。
func closureRegisterMachine(t *testing.T, ctx context.Context, db *gorm.DB, spec coordination.ServiceSpec, index int) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(raw)
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(secret))
	repo, err := registryinfra.NewOAuthClientManagementRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateOAuthClient(ctx, registry.OAuthClientCreateInput{
		TenantID: closureTenant, ApplicationID: strings.Repeat("a", 26), EnvironmentID: strings.Repeat("e", 26),
		ClientID: spec.OAuthClientID, ClientName: spec.ServiceID, ClientType: "SERVICE",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 300,
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"license.runtime"},
	}, strings.Repeat(strconv.Itoa(index+1), 26), &registry.SecretWrite{
		CredentialID: strings.Repeat(strconv.Itoa(index+4), 26), SecretHash: hash,
		Fingerprint: hex.EncodeToString(digest[:]),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// closureHTTPServer 将真实机器认证串到原运行时 HTTP 处理器。
// 参数：t 为测试上下文，coordinator 为真实状态机，machines 为真实 SQL/JWT 服务。
// 返回值：仅绑定回环的临时 HTTP 服务；无固定成功响应或伪造主体。
func closureHTTPServer(t *testing.T, coordinator *coordination.Service, machines *registry.Service) *httptest.Server {
	t.Helper()
	handler, err := licensehttp.NewRuntimeHandler(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/internal/licenses/runtime/{service}/ready", handler.Ready)
	mux.HandleFunc("GET /api/v1/internal/licenses/runtime/{service}/snapshot", handler.Snapshot)
	mux.HandleFunc("POST /api/v1/internal/licenses/runtime/{service}/ack", handler.Ack)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		principal, err := machines.Authenticate(r.Context(), strings.TrimPrefix(authorization, "Bearer "))
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r.WithContext(appctx.WithPrincipal(r.Context(), principal)))
	}))
}

// closureConsumers 以真实目录登记两个成员并构造独立持久化运行时与同步客户端。
// 参数：t、ctx 为测试上下文，f 为服务夹具，base 为临时原处理器地址。
// 返回值：两个未收到任何快照的消费方；READY 与 ACK 只能由 SyncOnce 产生。
func closureConsumers(t *testing.T, ctx context.Context, f *closureFixture, base string) []closureConsumer {
	t.Helper()
	components, err := catalog.Components("reimbursement")
	if err != nil || len(components) != 2 {
		t.Fatal("报销目录必须精确包含两个消费方")
	}
	result := make([]closureConsumer, 0, 2)
	for index, component := range components {
		spec := coordination.ServiceSpec{
			Application: component.Application, Environment: "production", ServiceID: component.ServiceID,
			OAuthClientID:        catalog.ClientIDV2(f.instance, component.Application, "production", component.ServiceID, "closure-1"),
			RegistrationProtocol: component.RegistrationProtocol, CatalogVersion: catalog.Version, CredentialGeneration: "closure-1",
		}
		if err := f.coordinator.Register(ctx, spec); err != nil {
			t.Fatal(err)
		}
		secret := closureRegisterMachine(t, ctx, f.db, spec, index)
		path := filepath.Join(t.TempDir(), "runtime-state.json")
		local := closureOpenRuntime(t, f, spec, path)
		tokenSource := func(ctx context.Context) (string, error) {
			token, err := f.machines.IssueClientCredentials(ctx, spec.OAuthClientID, secret, []string{"license.runtime"})
			return token.AccessToken, err
		}
		client, err := syncclient.New(syncclient.Options{
			BaseURL: base, ServiceID: spec.ServiceID, RegistrationProtocol: spec.RegistrationProtocol,
			CatalogVersion: spec.CatalogVersion, AllowHTTP: true, Runtime: local,
			Token: tokenSource,
		})
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, closureConsumer{spec, path, local, client, tokenSource})
	}
	return result
}

// closureOpenRuntime 以独立状态文件及原信任配置重新构造消费端。
// 参数：t 为测试上下文，f 为临时信任，spec 为目录成员，path 为私有状态文件。
// 返回值：原 FileRuntime；不会创建或伪造已授权状态。
func closureOpenRuntime(t *testing.T, f *closureFixture, spec coordination.ServiceSpec, path string) *runtime.FileRuntime {
	t.Helper()
	local, err := runtime.NewFileRuntime(path, runtime.Binding{
		InstanceID: f.instance, Environment: "production", Application: "reimbursement", ServiceID: spec.ServiceID,
	}, f.signer.public, f.signer.vendor)
	if err != nil {
		t.Fatal(err)
	}
	return local
}

// closureRejectForeignMember 证明有效机器令牌不能替另一个已登记成员报 READY。
// 参数：t、ctx 为测试上下文，f 为两个未就绪成员，base 为真实处理器地址。
// 返回值：无；若错误绑定被接受或污染目标 READY 则失败。
func closureRejectForeignMember(t *testing.T, ctx context.Context, f *closureFixture, base string) {
	t.Helper()
	token, err := f.consumers[0].token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	url := base + "/api/v1/internal/licenses/runtime/" + f.consumers[1].spec.ServiceID + "/ready"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		strings.NewReader(`{"protocol":2,"catalog_version":`+strconv.Itoa(catalog.Version)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("错误成员绑定状态=%d", response.StatusCode)
	}
	for _, member := range closureStatus(t, ctx, f).Members {
		if member.ReadyAt != nil || member.AckAt != nil {
			t.Fatal("被拒请求污染了成员状态")
		}
	}
}

// closureSyncRound 按有限顺序让每个独立消费方走真实 READY、验签落盘及 ACK。
// 参数：t、ctx 为测试上下文，consumers 为两个独立消费端。
// 返回值：无；任何协议或持久化错误立即失败，不通过睡眠或直接改状态恢复。
func closureSyncRound(t *testing.T, ctx context.Context, consumers []closureConsumer) {
	t.Helper()
	for _, consumer := range consumers {
		if err := consumer.client.SyncOnce(ctx); err != nil {
			t.Fatalf("%s 真实同步失败：%v", consumer.spec.ServiceID, err)
		}
	}
}

// closureStatus 从原协调器读取持久化状态并确认成员集合未缩小。
// 参数：t、ctx 为测试上下文，f 为真实协调器夹具。
// 返回值：当前应用状态；成员数或查询错误使测试失败。
func closureStatus(t *testing.T, ctx context.Context, f *closureFixture) coordination.Status {
	t.Helper()
	status, err := f.coordinator.Status(ctx, "reimbursement")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Members) != 2 {
		t.Fatal("协调器未保留两个报销成员")
	}
	return status
}

// closureAssertCurrent 验证最终同修订签发、确认与两份独立已验签状态。
// 参数：t、ctx 为测试上下文，f 为真实夹具，status 为协调器读取结果。
// 返回值：无；不把 APPLYING 旧 ACK 或单一消费端成功冒充闭环。
func closureAssertCurrent(t *testing.T, ctx context.Context, f *closureFixture, status coordination.Status) {
	t.Helper()
	if status.State != runtime.Enforced || !status.SnapshotCurrent || status.RegistrationMode != catalog.BuiltinComponentV2 {
		t.Fatal("最终强制执行、当前快照或 V2 内置模式未完成")
	}
	for _, member := range status.Members {
		if member.IssuedRevision != status.Revision || member.AckRevision != status.Revision || member.AckAt == nil {
			t.Fatal("仍有成员未确认最终修订")
		}
	}
	for _, consumer := range f.consumers {
		state, err := consumer.local.ReadState(ctx)
		if err != nil || state.ControlRevision != status.Revision || !state.Enforced || state.HighestLicenseVersion != 1 {
			t.Fatal("消费端未持久化最终修订与真实授权")
		}
		verified, err := f.signer.Verify(state.SnapshotJWS, runtime.Binding{
			InstanceID: f.instance, Environment: "production", Application: "reimbursement", ServiceID: consumer.spec.ServiceID,
		})
		if err != nil || verified.EnforcementState != runtime.Enforced {
			t.Fatal("落盘快照验签失败")
		}
		if err := consumer.local.Evaluate(ctx, core.MUTATE_BUSINESS, time.Now()); err != nil {
			t.Fatal("激活后消费端未允许有效业务写")
		}
	}
}

// closureAssertExpiryAndRestart 用未来判定时间验证固定到期，并重开独立状态文件。
// 参数：t、ctx 为测试上下文，f 为真实信任夹具，revision 为最终确认修订。
// 返回值：无；到期写放行、历史/导出/基础服务阻断或修订丢失都失败。
func closureAssertExpiryAndRestart(t *testing.T, ctx context.Context, f *closureFixture, revision uint64) {
	t.Helper()
	for _, consumer := range f.consumers {
		reopened := closureOpenRuntime(t, f, consumer.spec, consumer.path)
		before, err := reopened.ReadState(ctx)
		if err != nil || before.ControlRevision != revision || !before.Enforced {
			t.Fatal("重新打开状态文件丢失修订")
		}
		if err := reopened.Evaluate(ctx, core.MUTATE_BUSINESS, time.Now()); err != nil {
			t.Fatal("重开后有效写被拒")
		}
		if err := reopened.Evaluate(ctx, core.MUTATE_BUSINESS, f.expires); !errors.Is(err, core.ErrDenied) {
			t.Fatal("固定到期边界未拒绝业务写")
		}
		for _, operation := range []core.Operation{core.READ_HISTORY, core.EXPORT_HISTORY, core.ESSENTIAL_SERVICE} {
			if err := reopened.Evaluate(ctx, operation, f.expires.Add(time.Second)); err != nil {
				t.Fatalf("到期后 %s 不可用：%v", operation, err)
			}
		}
		after, err := closureOpenRuntime(t, f, consumer.spec, consumer.path).ReadState(ctx)
		if err != nil || after.ControlRevision != revision || after.SnapshotJWS != before.SnapshotJWS || after.ClockHighWater < f.expires.Unix() {
			t.Fatal("到期判定或再次重开改变签名修订或丢失时间高水位")
		}
	}
}
