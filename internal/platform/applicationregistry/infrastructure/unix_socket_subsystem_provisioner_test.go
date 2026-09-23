package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

func TestUnixSocketSubsystemProvisionerExchangesOnlySupportedOperations(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{}
	serverContext, cancelServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- RunSubsystemProvisioningServer(serverContext, socketPath, executor)
	}()
	waitForProvisioningSocket(t, socketPath)

	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("construct socket client: %v", err)
	}
	preflight := application.SubsystemPreflightInput{TenantID: "tenant-1", ApplicationCode: "contract_management", Environment: "dev"}
	const requestID = "01KZ42MPYY9168FKFPBXVTX677"
	preflightContext := requestctx.WithRequestID(context.Background(), requestID)
	if err := client.Preflight(preflightContext, preflight); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if got := executor.requestIDSnapshot(); got != requestID {
		t.Fatalf("executor request id = %q, want %q", got, requestID)
	}
	input := application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationCode: "contract_management", Environment: "dev",
		Issuer: "http://localhost:8081", ClientID: "contract_management-dev-web",
		ClientSecret: "one-time-secret", RedirectURI: "http://localhost:8081/contract_management/auth/callback",
		PublicURL: "http://localhost:8081/contract_management/", PathPrefix: "/contract_management",
		UpstreamURL: "http://contract-api:8081",
	}
	if err := client.Provision(context.Background(), input); err != nil {
		t.Fatalf("provision: %v", err)
	}

	code, received := executor.snapshot()
	if code != "contract_management" {
		t.Fatalf("preflight code = %q", code)
	}
	if !reflect.DeepEqual(received, input) {
		t.Fatalf("provision input = %#v, want %#v", received, input)
	}

	if err := client.Update(context.Background(), application.SubsystemProvisioningInput{
		ApplicationCode: "contract_management", Environment: "dev",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := executor.updateInputSnapshot(); got.ApplicationCode != "contract_management" || got.Environment != "dev" {
		t.Fatalf("update input = %#v", got)
	}

	if err := client.Teardown(context.Background(), "tenant-1", "contract_management", "dev"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if gotCode, gotEnv := executor.teardownSnapshot(); gotCode != "contract_management" || gotEnv != "dev" {
		t.Fatalf("teardown (%q, %q)", gotCode, gotEnv)
	}

	cancelServer()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provisioning server did not stop")
	}
}

func TestUnixSocketSubsystemProvisionerDisabled(t *testing.T) {
	t.Parallel()
	client, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second)
	if err != nil {
		t.Fatalf("construct disabled client: %v", err)
	}
	if err := client.Preflight(context.Background(), application.SubsystemPreflightInput{ApplicationCode: "contract_management"}); !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("disabled preflight error = %v", err)
	}
}

func TestNormalizedProvisioningRequestIDRejectsLogInjection(t *testing.T) {
	t.Parallel()
	if got := normalizedProvisioningRequestID(" 01kz42mpyy9168fkfpbxvtx677 "); got != "01KZ42MPYY9168FKFPBXVTX677" {
		t.Fatalf("normalized request id = %q", got)
	}
	for _, value := range []string{"", "01KZ42MPYY9168FKFPBXVTX67", "01KZ42MPYY9168FKFPBXVTX67I", "01KZ42MPYY9168FKFPBXVTX6\n"} {
		if got := normalizedProvisioningRequestID(value); got != "" {
			t.Fatalf("unsafe request id %q normalized to %q", value, got)
		}
	}
}

func TestUnixSocketSubsystemProvisionerReportsProductionCapabilities(t *testing.T) {
	t.Parallel()
	client, err := NewUnixSocketSubsystemProvisioner(true, "/tmp/subsystem-provisioner.sock", time.Second, application.SubsystemProvisioningCapabilities{
		Mode: " production ", SupportedApplicationCodes: []string{" billing_management ", "billing_management"},
		SupportedEnvironments: []string{" dev "}, DefaultApplicationCode: " billing_management ",
		DefaultEnvironment: " dev ", DefaultPathPrefix: " /billing/ ",
	})
	if err != nil {
		t.Fatalf("construct production client: %v", err)
	}

	capabilities := client.Capabilities()
	if !capabilities.Enabled || capabilities.Mode != "production" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if !reflect.DeepEqual(capabilities.SupportedApplicationCodes, []string{"billing_management"}) {
		t.Fatalf("supported application codes = %#v", capabilities.SupportedApplicationCodes)
	}
	if !reflect.DeepEqual(capabilities.SupportedEnvironments, []string{"dev"}) {
		t.Fatalf("supported environments = %#v", capabilities.SupportedEnvironments)
	}
	if capabilities.DefaultApplicationCode != "billing_management" || capabilities.DefaultEnvironment != "dev" || capabilities.DefaultPathPrefix != "/billing" {
		t.Fatalf("normalized defaults = %#v", capabilities)
	}
}

func TestUnixSocketSubsystemProvisionerRejectsInvalidMode(t *testing.T) {
	t.Parallel()

	invalidMode := application.SubsystemProvisioningCapabilities{Mode: "remote"}
	if client, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second, invalidMode); err == nil || client != nil {
		t.Fatalf("invalid mode returned client=%#v err=%v", client, err)
	}
	if client, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second, invalidMode, invalidMode); err == nil || client != nil {
		t.Fatalf("multiple policies returned client=%#v err=%v", client, err)
	}
}

func TestUnixSocketSubsystemProvisionerReturnsSafeActionableExecutorMessage(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{preflightErr: provisioningError("subsystem Compose file is unavailable")}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Preflight(context.Background(), application.SubsystemPreflightInput{ApplicationCode: "customer_and_opportunity"})
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) || !strings.Contains(err.Error(), "Compose file is unavailable") {
		t.Fatalf("preflight error = %v", err)
	}
}

func TestUnixSocketSubsystemProvisionerCarriesMultiLineExecutorDetail(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-detail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{preflightErr: provisioningError(
		"start production subsystem services: CRM startup failed: authorization catalog token returned HTTP 401\ncontainer exited before health check\n",
	)}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Preflight(context.Background(), application.SubsystemPreflightInput{ApplicationCode: "customer_and_opportunity"})
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("preflight error = %v", err)
	}
	for _, expected := range []string{"start production subsystem services", "authorization catalog token returned HTTP 401", "container exited before health check"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("preflight error missing %q: %v", expected, err)
		}
	}
}

func waitForProvisioningSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("provisioning socket %q was not created", socketPath)
}

type recordingSubsystemProvisioner struct {
	mutex        sync.Mutex
	code         string
	requestID    string
	input        application.SubsystemProvisioningInput
	teardownCode string
	teardownEnv  string
	updateInput  application.SubsystemProvisioningInput
	preflightErr error
	// mode mirrors the privileged Agent's deployment mode so tests can exercise the
	// production manifest-checksum guard without starting a real production executor.
	mode string
	// accessOrigin records the origin forwarded by the server for SEC-F1 injection tests.
	accessOrigin string
}

func (provisioner *recordingSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	return application.SubsystemProvisioningCapabilities{Mode: provisioner.mode}
}

func (provisioner *recordingSubsystemProvisioner) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.code = input.ApplicationCode
	provisioner.requestID = requestctx.RequestID(ctx)
	return provisioner.preflightErr
}

func (provisioner *recordingSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.input = input
	return nil
}

func (provisioner *recordingSubsystemProvisioner) Update(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.updateInput = input
	return nil
}

func (provisioner *recordingSubsystemProvisioner) Teardown(_ context.Context, _ string, code, environment string) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.teardownCode = code
	provisioner.teardownEnv = environment
	return nil
}

func (provisioner *recordingSubsystemProvisioner) ApplyAccess(_ context.Context, input settingsapplication.AccessApplyInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.accessOrigin = input.PublicOrigin
	return nil
}

func (provisioner *recordingSubsystemProvisioner) accessOriginSnapshot() string {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.accessOrigin
}

func (provisioner *recordingSubsystemProvisioner) snapshot() (string, application.SubsystemProvisioningInput) {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.code, provisioner.input
}

func (provisioner *recordingSubsystemProvisioner) requestIDSnapshot() string {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.requestID
}

func (provisioner *recordingSubsystemProvisioner) updateInputSnapshot() application.SubsystemProvisioningInput {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.updateInput
}

func (provisioner *recordingSubsystemProvisioner) teardownSnapshot() (string, string) {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.teardownCode, provisioner.teardownEnv
}

func TestProvisioningRejectionErrorDeduplicatesIdenticalSummaryAndDetail(t *testing.T) {
	t.Parallel()
	err := provisioningRejectionError("subsystem provisioning unavailable", "subsystem provisioning unavailable")
	if got := err.Error(); got != application.ErrSubsystemProvisioningUnavailable.Error() {
		t.Fatalf("duplicated rejection = %q, want a single sentinel", got)
	}
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("deduplicated rejection lost the sentinel: %v", err)
	}
	if strings.Count(err.Error(), application.ErrSubsystemProvisioningUnavailable.Error()) != 1 {
		t.Fatalf("sentinel repeated in %q", err.Error())
	}

	distinct := provisioningRejectionError("start production subsystem services", "container exited before health check")
	if !strings.Contains(distinct.Error(), "start production subsystem services") ||
		!strings.Contains(distinct.Error(), "container exited before health check") {
		t.Fatalf("distinct summary/detail was dropped: %v", distinct)
	}
}

func TestUnixSocketSubsystemProvisionerReportsActionableProductionManifestRejection(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-manifest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{mode: "production"}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Production Agent requires an approved manifest checksum; an empty checksum must be
	// rejected with a concrete reason instead of the bare sentinel that used to render as
	// "subsystem provisioning unavailable: subsystem provisioning unavailable: ...".
	err = client.Update(context.Background(), application.SubsystemProvisioningInput{
		ApplicationCode: "platform", Environment: "prod",
	})
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("update error = %v", err)
	}
	if !strings.Contains(err.Error(), "requires an approved manifest checksum") {
		t.Fatalf("update error is not actionable: %v", err)
	}
	if strings.Count(err.Error(), application.ErrSubsystemProvisioningUnavailable.Error()) != 1 {
		t.Fatalf("update error repeats the sentinel: %q", err.Error())
	}
	if got := executor.updateInputSnapshot(); got.ApplicationCode != "" {
		t.Fatalf("production executor must not run without a manifest checksum: %#v", got)
	}

	// A request that does carry the checksum still reaches the executor.
	if err := client.Update(context.Background(), application.SubsystemProvisioningInput{
		ApplicationCode: "contract_management", Environment: "prod", ManifestChecksum: "sha256:approved",
	}); err != nil {
		t.Fatalf("approved update: %v", err)
	}
	if got := executor.updateInputSnapshot(); got.ApplicationCode != "contract_management" || got.ManifestChecksum != "sha256:approved" {
		t.Fatalf("approved update input = %#v", got)
	}
}

func TestSubsystemProvisioningServerRejectsInjectedAccessOrigin(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-access-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// SEC-F1：换行注入 payload 必须在 server 边界被拒绝，executor 不能收到该值。
	err = client.ApplyAccess(context.Background(), settingsapplication.AccessApplyInput{
		PublicOrigin: "https://portal.example.com\nDEV_AUTH_ENABLED=false",
	})
	if err == nil || !strings.Contains(err.Error(), "public origin") {
		t.Fatalf("injected origin error = %v", err)
	}
	if got := executor.accessOriginSnapshot(); got != "" {
		t.Fatalf("executor received rejected origin %q", got)
	}
	// 合法 origin 归一化（去空白）后到达 executor。
	if err := client.ApplyAccess(context.Background(), settingsapplication.AccessApplyInput{
		PublicOrigin: "  http://portal.example.com:9090  ",
	}); err != nil {
		t.Fatalf("apply valid origin: %v", err)
	}
	if got := executor.accessOriginSnapshot(); got != "http://portal.example.com:9090" {
		t.Fatalf("executor origin = %q", got)
	}
}

func TestSubsystemProvisioningServerProductionTeardownRequiresManifestChecksum(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-teardown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{mode: "production"}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second, application.SubsystemProvisioningCapabilities{
		Mode: "production",
		Targets: []application.SubsystemProvisioningTarget{
			{ApplicationCode: "contract_management", Environment: "prod", ManifestChecksum: "sha256:approved"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// SEC-F4：没有可解析清单校验和的 teardown 必须被拒绝，生产 executor 不得被调用。
	err = client.Teardown(context.Background(), "tenant-1", "unknown_management", "prod")
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) ||
		!strings.Contains(err.Error(), "requires an approved manifest checksum") {
		t.Fatalf("teardown error = %v", err)
	}
	if code, environment := executor.teardownSnapshot(); code != "" || environment != "" {
		t.Fatalf("production executor ran without a manifest checksum: (%q, %q)", code, environment)
	}

	// 客户端按 code→environment 解析出已批准校验和后放行。
	if err := client.Teardown(context.Background(), "tenant-1", "contract_management", "prod"); err != nil {
		t.Fatalf("approved teardown: %v", err)
	}
	if code, environment := executor.teardownSnapshot(); code != "contract_management" || environment != "prod" {
		t.Fatalf("approved teardown = (%q, %q)", code, environment)
	}
}

func TestRunSubsystemProvisioningServerTightensSocketPermissions(t *testing.T) {
	t.Parallel()
	base, err := os.MkdirTemp("/tmp", "bp-provisioner-perm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	socketDirectory := filepath.Join(base, "runtime")
	// 模拟历史遗留的宽松目录（0777）。
	if err := os.Mkdir(socketDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)

	// SEC-F4：已存在目录必须被主动收紧到 0750（MkdirAll 只保护新建目录）。
	directoryInfo, err := os.Stat(socketDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o750 {
		t.Fatalf("socket directory permissions = %o, want 750", directoryInfo.Mode().Perm())
	}
	// socket 文件必须收紧到 0660；轮询避免落在 Listen 与 Chmod 之间的窗口。
	socketPerm := os.FileMode(0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, statErr := os.Stat(socketPath); statErr == nil {
			socketPerm = info.Mode().Perm()
			if socketPerm == 0o660 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if socketPerm != 0o660 {
		t.Fatalf("socket permissions = %o, want 660", socketPerm)
	}
}
