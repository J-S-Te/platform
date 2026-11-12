package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/license-core/catalog"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

// 这是经过审核的开发拓扑。生产继续使用单独审核的清单；dev 源指纹不能批准 prod。
var localRuntimeDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var localRuntimeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

var localLicenseServices = map[string][]string{
	"reimbursement":            {"reimbursement-api", "reimbursement-worker"},
	"contract_management":      {"contract-api", "contract-worker"},
	"customer_and_opportunity": {"customer-api", "customer-opportunity-alert-worker", "customer-owner-notification-worker", "customer-presale-assignment-notification-worker", "customer-presale-progress-notification-worker", "customer-notification-delivery-worker", "customer-presale-alert-worker", "presale-worker", "contract-transfer-worker"},
	"customer_portal":          {"portal-api", "portal-report-worker", "portal-invite-compensation-worker"},
	"project_management":       {"project-api", "sla-notifier"},
	"settlement":               {"settlement-api", "settlement-worker"},
	"data_analysis":            {"dashboard-api", "aggregation-worker", "alert-worker"},
}

type localLicenseMigration struct {
	Service     string `json:"service"`
	ImageDigest string `json:"image_digest"`
	Version     string `json:"version"`
	Protocol    string `json:"protocol"`
}

var localLicenseMigrations = map[string]string{"reimbursement": "reimbursement-migrate", "contract_management": "contract-migrate", "customer_and_opportunity": "customer-migrate", "customer_portal": "portal-migrate", "project_management": "project-migrate", "settlement": "settlement-migrate", "data_analysis": "dashboard-migrate"}

// localLicenseComposeProfiles 是本地许可运行态解析镜像时必须启用的 compose profile 全集。
// 报销服务（api/worker/migrate/catalog-sync）只在这些 profile 下才会被 compose config 展开；
// 缺任一 profile 会表现为 local service image missing 或 local migration image missing，
// 且失败后 retry 仍会重复同一错误。
var localLicenseComposeProfiles = []string{
	"bootstrap", "customer", "portal", "project", "data-analysis", "presale-worker",
	"reimbursement", "reimbursement-release",
}

// localLicenseComposeProfileArgs 返回 compose 全局的 profile 标志。
// 许可镜像解析与集成子系统重建必须共用同一份 profile 集合，否则两条路径会各自漏加，
// 表现为 local service image missing 或 no such service。
// 参数：无。
// 返回值：形如 --profile X 的参数切片。
func localLicenseComposeProfileArgs() []string {
	args := make([]string, 0, len(localLicenseComposeProfiles)*2)
	for _, profile := range localLicenseComposeProfiles {
		args = append(args, "--profile", profile)
	}
	return args
}

type localRuntimeRelease struct {
	Migration     localLicenseMigration                `json:"migration"`
	Version       int                                  `json:"version"`
	Project       string                               `json:"project"`
	Application   string                               `json:"application"`
	Environment   string                               `json:"environment"`
	Operator      string                               `json:"operator"`
	Reviewed      bool                                 `json:"reviewed"`
	ComposeSHA256 string                               `json:"compose_sha256"`
	SourceDigest  string                               `json:"source_digest"`
	Components    []application.RuntimeLicenseApproval `json:"components"`
	Services      []licenseevidence.Service            `json:"services"`
}

func safeLocalPath(path string, missingLeaf bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("unsafe local runtime path")
	}
	target := path
	if missingLeaf {
		target = filepath.Dir(path)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil || resolved != target {
		return fmt.Errorf("unsafe local runtime path")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe local runtime file")
	} else if err != nil && !(missingLeaf && os.IsNotExist(err)) {
		return fmt.Errorf("local runtime file unavailable")
	}
	return nil
}
func readLocalPrivateJSON(path string, out any) error {
	if err := safeLocalPath(path, false); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 256*1024 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("private local runtime document required")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(raw) > 256*1024 || validateLocalJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
		return fmt.Errorf("invalid local runtime document")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("invalid local runtime document")
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid local runtime document")
	}
	return nil
}
func (p *LocalDockerSubsystemProvisioner) localPlatformRoot() string {
	return filepath.Dir(filepath.Dir(p.config.GatewayScriptPath))
}

func (p *LocalDockerSubsystemProvisioner) localComposeFile() string {
	name := p.config.PlatformComposeFile
	if name == "" {
		name = "compose.local.yaml"
	}
	return filepath.Join(p.localPlatformRoot(), name)
}

type localImageMetadata struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Protocol string `json:"protocol"`
}

func (p *LocalDockerSubsystemProvisioner) localImageProjection(ctx context.Context, id string) (localImageMetadata, error) {
	var result localImageMetadata
	args := []string{"image", "inspect", "--format", `{"id":{{json .Id}},"version":{{json (index .Config.Labels "org.opencontainers.image.version")}},"protocol":{{json (index .Config.Labels "com.basic-platform.license.protocol")}}}`, id}
	b, err := p.localDockerProjection(ctx, args...)
	if err != nil {
		// 镜像缺失与守护进程不可达在 inspect 中都表现为失败。探测服务端时不解析也不转发
		// Docker 可能敏感的输出。
		_, probeErr := p.localDockerProjection(ctx, "info")
		if probeErr != nil {
			return result, application.ErrRuntimeLicenseDockerUnavailable
		}
		return result, application.ErrRuntimeLicenseApprovalMismatch
	}
	if len(b) > 4096 || json.Unmarshal(b, &result) != nil {
		return result, application.ErrRuntimeLicenseApprovalMismatch
	}
	return result, nil
}

func (p *LocalDockerSubsystemProvisioner) localDockerProjection(ctx context.Context, args ...string) ([]byte, error) {
	return p.localDockerProjectionLimit(ctx, 4096, args...)
}

func (p *LocalDockerSubsystemProvisioner) localDockerProjectionLimit(ctx context.Context, limit int, args ...string) ([]byte, error) {
	if _, ok := p.runner.(execSubsystemCommandRunner); ok {
		if len(args) == 1 && args[0] == "info" {
			// 普通 info 保留 CLI 的连接失败退出码；其可能很大且敏感的输出被丢弃，绝不缓存。
			return nil, exec.CommandContext(ctx, p.config.DockerBinary, "info").Run()
		}
		if len(args) > 0 && args[0] == "compose" {
			// Compose config 是只读拓扑查询，但证据白名单有意只允许 inspect/ps。
			// 由受信 Agent 直接运行，并使用有界输出缓冲。
			command := exec.CommandContext(ctx, p.config.DockerBinary, args...)
			command.Dir = p.localPlatformRoot()
			command.Env = os.Environ()
			b, e := command.Output()
			if e != nil || len(b) > limit {
				return nil, application.ErrRuntimeLicenseDockerUnavailable
			}
			return b, nil
		}
		return (licenseevidence.ExecRunner{DockerBinary: p.config.DockerBinary}).RunOutput(ctx, limit, "docker", args...)
	} else if runner, ok := p.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	}); ok {
		b, err := runner.RunOutput(ctx, p.localPlatformRoot(), os.Environ(), p.config.DockerBinary, args...)
		if err != nil || len(b) > limit {
			return nil, application.ErrRuntimeLicenseDockerUnavailable
		}
		return b, nil
	}
	return nil, application.ErrRuntimeLicenseDockerUnavailable
}

// SubsystemImageReadiness 按内置运行目录与当前本地 Compose 镜像检查可用性。
// V1 批准清单已下线：不再用批准文件里的 digest 充当就绪依据。
func (p *LocalDockerSubsystemProvisioner) SubsystemImageReadiness(ctx context.Context, applicationCode, environment string) ([]application.SubsystemImageReadiness, error) {
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(ctx, applicationCode, environment)
	if err != nil {
		return nil, err
	}
	images, err := p.localComposeImages(ctx)
	if err != nil {
		return nil, err
	}
	readiness := make([]application.SubsystemImageReadiness, 0, len(set.Components))
	for _, component := range set.Components {
		imageRef := images[localComposeService(component.ServiceID)]
		item := application.SubsystemImageReadiness{ServiceID: component.ServiceID}
		if imageRef == "" {
			readiness = append(readiness, item)
			continue
		}
		projection, projectionErr := p.localImageProjection(ctx, imageRef)
		if errors.Is(projectionErr, application.ErrRuntimeLicenseDockerUnavailable) {
			return nil, projectionErr
		}
		item.ImageDigest = strings.TrimSpace(projection.ID)
		item.Present = projectionErr == nil && localRuntimeDigest.MatchString(item.ImageDigest)
		readiness = append(readiness, item)
	}
	return readiness, nil
}

// CollectLicenseEvidence 在本地 V2 模式下不采集运行证据：证据采集依赖按文件审批的
// 评审发行（V1），该路径已下线。返回明确的不支持错误，绝不伪造报告。
func (p *LocalDockerSubsystemProvisioner) CollectLicenseEvidence(ctx context.Context, app, env string) (licenseevidence.Report, error) {
	return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: local evidence collection requires the retired V1 review flow")
}

type localPinnedService struct {
	PullPolicy  string            `json:"pull_policy,omitempty"`
	Image       string            `json:"image"`
	EnvFile     []string          `json:"env_file,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}
type localPinnedCompose struct {
	Services map[string]localPinnedService `json:"services"`
	Volumes  map[string]struct{}           `json:"volumes"`
}

func (p *LocalDockerSubsystemProvisioner) localRuntimeOverride() (string, error) {
	path := filepath.Join(p.localPlatformRoot(), "runtime", "local-license.compose.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return "", nil
	}
	var override localPinnedCompose
	if err := readLocalPrivateJSON(path, &override); err != nil {
		return "", err
	}
	consumerCount := 0
	for id, item := range override.Services {
		if err := validateLocalProjectBackgroundEnvironment(id, item.Image, item.Environment); err != nil {
			return "", err
		}
		migration := false
		for _, name := range localLicenseMigrations {
			migration = migration || id == name
		}
		if item.PullPolicy != "" && item.PullPolicy != "never" {
			return "", fmt.Errorf("local image pulling must be disabled")
		}
		if migration {
			if !localRuntimeDigest.MatchString(item.Image) || len(item.EnvFile) != 0 || len(item.Volumes) != 0 {
				return "", fmt.Errorf("invalid local migration pin")
			}
			continue
		}
		consumerCount++
		known := false
		for _, services := range localLicenseServices {
			for _, name := range services {
				known = known || id == name
			}
		}
		expectedEnv := []string{filepath.Join(p.localPlatformRoot(), "runtime", "license-"+id+".env")}
		expectedVolumes := []string{"license-state-" + id + ":/var/lib/commercial-license", filepath.Join(p.localPlatformRoot(), "data", "keys") + ":/app/data/keys:ro"}
		if !known || !localRuntimeDigest.MatchString(item.Image) || !reflect.DeepEqual(item.EnvFile, expectedEnv) || !reflect.DeepEqual(item.Volumes, expectedVolumes) {
			return "", provisioningError("invalid local licensed Compose override")
		}
	}
	if len(override.Volumes) != consumerCount {
		return "", fmt.Errorf("invalid local state volume declarations")
	}
	for id, item := range override.Services {
		if len(item.EnvFile) == 0 {
			continue
		}
		if _, ok := override.Volumes["license-state-"+id]; !ok {
			return "", fmt.Errorf("missing local state volume")
		}
	}
	return path, nil
}

// deliverLocalRuntimeLicense 只交付内置目录组件身份；出现任何 V1 凭据即失败关闭。
// 凭据为空表示该应用尚未登记商业运行时（例如首次接入前），按未授权兼容模式继续部署，
// 与生产交付口径一致；一旦有凭据就必须是完整的 V2 绑定。
func (p *LocalDockerSubsystemProvisioner) deliverLocalRuntimeLicense(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.RegistrationProtocol != catalog.RegistrationProtocolV2 {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: legacy V1 runtime credential is no longer supported")
		}
	}
	return p.deliverLocalBuiltinRuntimeLicense(ctx, input, false)
}
func localComposeService(serviceID string) string {
	switch serviceID {
	case "customer-presale-worker":
		return "presale-worker"
	case "customer-contract-transfer-worker":
		return "contract-transfer-worker"
	case "project-sla-notifier":
		return "sla-notifier"
	case "data-analysis-api":
		return "dashboard-api"
	case "data-analysis-aggregation-worker":
		return "aggregation-worker"
	case "data-analysis-alert-worker":
		return "alert-worker"
	default:
		return serviceID
	}
}

func (p *LocalDockerSubsystemProvisioner) localComposeImages(ctx context.Context) (map[string]string, error) {
	root := p.localPlatformRoot()
	composePath := p.localComposeFile()
	if err := safeLocalPath(composePath, false); err != nil {
		return nil, err
	}
	args := []string{
		"compose", "-p", p.config.PlatformComposeProject,
		"--env-file", filepath.Join(root, "docker", ".env.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "contract_management", ".env.local"),
		"--env-file", filepath.Join(root, "docker", ".env.customer.local"),
		"--env-file", filepath.Join(root, "docker", ".env.portal.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "project_management", ".env.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "data_analysis", ".env.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "Settlement", ".env.local"),
	}
	args = append(args, localLicenseComposeProfileArgs()...)
	args = append(args, "-f", composePath, "config", "--format", "json")
	out, err := p.localDockerProjectionLimit(ctx, 2<<20, args...)
	if err != nil {
		return nil, err
	}
	var config struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err = json.Unmarshal(out, &config); err != nil || len(config.Services) == 0 {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	images := make(map[string]string, len(config.Services))
	for name, service := range config.Services {
		if strings.TrimSpace(service.Image) != "" {
			images[name] = strings.TrimSpace(service.Image)
		}
	}
	return images, nil
}

func (p *LocalDockerSubsystemProvisioner) deliverLocalBuiltinRuntimeLicense(ctx context.Context, input application.SubsystemProvisioningInput, allowSubset bool) error {
	settings := input.RuntimeLicenseSettings
	if settings.InstanceID == "" || settings.Environment == "" || settings.PlatformBaseURL != "http://platform-api:8080" || settings.PlatformPublicKeyPath != "/app/data/keys/jwt-ed25519-public.pem" || settings.StateDirectory != "/var/lib/commercial-license" || !settings.AllowHTTP {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local runtime binding incomplete")
	}
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(ctx, input.ApplicationCode, input.Environment)
	if err != nil || (!allowSubset && len(set.Components) != len(input.RuntimeLicenseCredentials)) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local component catalog mismatch")
	}
	images, err := p.localComposeImages(ctx)
	if err != nil {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local compose images unavailable: " + err.Error())
	}
	expected := map[string]application.RuntimeComponentSpec{}
	for _, component := range set.Components {
		expected[component.ServiceID] = component
	}
	root := p.localPlatformRoot()
	runtimeDir := filepath.Join(root, "runtime")
	if err = ensureLocalRuntimeDirectory(runtimeDir); err != nil {
		return err
	}
	if resolved, e := filepath.EvalSymlinks(runtimeDir); e != nil || resolved != runtimeDir {
		return provisioningError("unsafe local runtime directory")
	}
	override := localPinnedCompose{Services: map[string]localPinnedService{}, Volumes: map[string]struct{}{}}
	overridePath := filepath.Join(runtimeDir, "local-license.compose.json")
	if existing, e := p.localRuntimeOverride(); e != nil {
		return e
	} else if existing != "" {
		if e = readLocalPrivateJSON(existing, &override); e != nil {
			return e
		}
	}
	updates := map[string]map[string]string{}
	for _, credential := range input.RuntimeLicenseCredentials {
		component, ok := expected[credential.Approval.ServiceID]
		if !ok || credential.RegistrationProtocol != catalog.RegistrationProtocolV2 || credential.CatalogVersion != set.CatalogVersion || credential.CredentialGeneration != component.CredentialGeneration || credential.ClientID == "" || credential.ClientSecret == "" {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local catalog binding mismatch")
		}
		delete(expected, component.ServiceID)
		imageRef := images[localComposeService(component.ServiceID)]
		if imageRef == "" {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local service image missing")
		}
		projection, e := p.localImageProjection(ctx, imageRef)
		if e != nil || !localRuntimeDigest.MatchString(projection.ID) {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local service image unavailable")
		}
		values := map[string]string{
			"COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_INSTANCE_ID": settings.InstanceID,
			"COMMERCIAL_LICENSE_ENVIRONMENT": settings.Environment, "COMMERCIAL_LICENSE_SERVICE_ID": component.ServiceID,
			"COMMERCIAL_LICENSE_STATE_PATH":               runtimeLicenseStatePath(settings, credential),
			"COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH": settings.PlatformPublicKeyPath,
			"COMMERCIAL_LICENSE_PLATFORM_BASE_URL":        settings.PlatformBaseURL,
			"COMMERCIAL_LICENSE_CLIENT_ID":                credential.ClientID, "COMMERCIAL_LICENSE_CLIENT_SECRET": credential.ClientSecret,
			"COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL": "2", "COMMERCIAL_LICENSE_CATALOG_VERSION": "1",
			"COMMERCIAL_LICENSE_ALLOW_HTTP": "true",
		}
		for key, value := range values {
			if !validEnvironmentKey(key) || !validEnvironmentValue(value) {
				return provisioningError("invalid local runtime credential")
			}
		}
		composeID := localComposeService(component.ServiceID)
		envPath := filepath.Join(runtimeDir, "license-"+composeID+".env")
		if e = safeLocalPath(envPath, true); e != nil {
			return e
		}
		updates[envPath] = values
		override.Volumes["license-state-"+composeID] = struct{}{}
		override.Services[composeID] = localPinnedService{PullPolicy: "never", Image: projection.ID, EnvFile: []string{envPath}, Volumes: []string{"license-state-" + composeID + ":/var/lib/commercial-license", filepath.Join(root, "data", "keys") + ":/app/data/keys:ro"}}
		item := override.Services[composeID]
		item.Environment = localProjectBackgroundEnvironment(composeID, projection.ID)
		override.Services[composeID] = item
	}
	migrationService := localLicenseMigrations[input.ApplicationCode]
	imageRef := images[migrationService]
	if imageRef == "" {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local migration image missing")
	}
	projection, err := p.localImageProjection(ctx, imageRef)
	if err != nil || !localRuntimeDigest.MatchString(projection.ID) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local migration image unavailable")
	}
	override.Services[migrationService] = localPinnedService{PullPolicy: "never", Image: projection.ID}
	for path, values := range updates {
		if err = writeIsolatedRuntimeLicenseEnvironment(path, values); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(override)
	if err != nil {
		return err
	}
	return writeLocalRuntimeJSON(overridePath, raw)
}

func writeLocalRuntimeJSON(path string, b []byte) error {
	if err := safeLocalPath(path, true); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".local-license-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// 本操作之前必须已完成收编登记。在这里构建会使控制面刚登记的发布失效，
// 因此全部运行时镜像保持钉定。
// deployLicensedLocalStack 使用当前已批准镜像依次执行备份、迁移、目录及组件启动。
// 参数：ctx为受控操作上下文，input为批准目标及许可材料。
// 返回值：全部实际步骤及许可预检成功时返回nil；失败保留现场。
func (p *LocalDockerSubsystemProvisioner) deployLicensedLocalStack(ctx context.Context, input application.SubsystemProvisioningInput) error {
	// 受许可容器必须在固定记录里带 license-state-<service> 卷与只读公钥挂载：缺失时 API
	// 启动即因“commercial license configuration failed”重启，且不会进入健康检查失败的
	// 可见路径。这里在第一个 Compose 操作之前失败关闭。
	if err := p.requireLicensedLocalRuntimeVolumes(input); err != nil {
		return err
	}
	run := p.runIntegratedPlatformCompose
	var deps []string
	var migration, api string
	var catalog []string
	switch input.ApplicationCode {
	case "contract_management":
		deps = []string{"contract-mysql", "temporal"}
		migration = "contract-migrate"
		api = "contract-api"
		catalog = []string{"./authz-catalog", "publish"}
	case "customer_and_opportunity":
		deps = []string{"contract-mysql", "customer-mysql", "temporal"}
		migration = "customer-migrate"
		api = "customer-api"
		catalog = []string{"./authz-catalog", "publish", "crm"}
	case "customer_portal":
		deps = []string{"portal-mysql"}
		migration = "portal-migrate"
		api = "portal-api"
		catalog = []string{"./authz-catalog", "publish", "portal"}
	case "project_management":
		deps = []string{"contract-mysql", "project-mysql", "temporal"}
		migration = "project-migrate"
		api = "project-api"
	case "settlement":
		deps = []string{"settlement-mysql"}
		migration = "settlement-migrate"
		api = "settlement-api"
	case "reimbursement":
		deps = []string{"reimbursement-mysql"}
		migration = "reimbursement-migrate"
		api = "reimbursement-api"
	case "data_analysis":
		deps = []string{"dashboard-mysql"}
		migration = "dashboard-migrate"
		api = "dashboard-api"
	default:
		return provisioningError("local licensed target unsupported")
	}
	if err := run(ctx, append([]string{"up", "-d", "--wait", "--no-deps", "--no-build", "--pull", "never"}, deps...)...); err != nil {
		return err
	}
	var background *localProjectBackground
	if input.ApplicationCode == "project_management" {
		var err error
		background, err = p.projectBackgroundTarget(ctx)
		if err != nil {
			return err
		}
		if background.tenant != input.TenantID || background.environment != input.Environment {
			return errors.New("project runtime identity differs from controlled target")
		}
		if err := background.preflight(ctx); err != nil {
			return err
		}
		if err := prepareProjectBackground(ctx, background); err != nil {
			return err
		}
	}
	migrationArgs := []string{"run", "--rm", "--no-deps", migration}
	if input.ApplicationCode == "reimbursement" {
		if err := backupLocalReimbursement(ctx, run); err != nil {
			return err
		}
		var err error
		migrationArgs, err = reimbursementMigrationArguments("", migration, true)
		if err != nil {
			return err
		}
	}
	if err := run(ctx, migrationArgs...); err != nil {
		return err
	}
	if input.ApplicationCode == "reimbursement" {
		if err := run(ctx, "run", "--rm", "--no-deps", "--entrypoint", "/app/reimbursement-authz-catalog", api, "publish"); err != nil {
			return err
		}
	}
	if len(catalog) > 0 {
		if err := run(ctx, append([]string{"run", "--rm", "--no-deps", api}, catalog...)...); err != nil {
			return err
		}
	}
	if input.ApplicationCode == "settlement" {
		if err := p.syncSettlementCatalog(ctx, input); err != nil {
			return err
		}
	}
	if err := run(ctx, "up", "-d", "--wait", "--no-deps", "--no-build", "--pull", "never", api); err != nil {
		return err
	}
	if input.ApplicationCode == "customer_and_opportunity" {
		if err := run(ctx, "up", "-d", "--wait", "--no-deps", "--no-build", "--pull", "never", "presale-integration-mock"); err != nil {
			return err
		}
	}
	services := []string{}
	for _, name := range localLicenseServices[input.ApplicationCode] {
		if name != api {
			services = append(services, name)
		}
	}
	if len(services) > 0 {
		if err := run(ctx, append([]string{"up", "-d", "--wait", "--wait-timeout", "120", "--no-deps", "--no-build", "--pull", "never"}, services...)...); err != nil {
			return err
		}
	}
	if background != nil {
		if err := activateProjectBackground(ctx, background); err != nil {
			return err
		}
	}
	return p.verifyLicensedLocalStartup(ctx, input.ApplicationCode, input.Environment)
}

func ensureLocalRuntimeDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("unsafe local runtime directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe local runtime directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensureLocalRuntimeDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return os.Mkdir(path, 0700)
}
func validateLocalJSONKeys(d *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate JSON field")
			}
			seen[name] = true
			if err := validateLocalJSONKeys(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := validateLocalJSONKeys(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}
