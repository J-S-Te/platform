package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sort"
	"strings"
	"time"
	"unicode"
)

// 这是经过审核的开发拓扑。生产继续使用单独审核的清单；dev 源指纹不能批准 prod。
var localRuntimeDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var localRuntimeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

var localLicenseServices = map[string][]string{
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

var localLicenseMigrations = map[string]string{"contract_management": "contract-migrate", "customer_and_opportunity": "customer-migrate", "customer_portal": "portal-migrate", "project_management": "project-migrate", "settlement": "settlement-migrate", "data_analysis": "dashboard-migrate"}

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

func localDigest(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

// 路径由宿主配置固定，绝不由 HTTP 请求决定。解析整个父级链，符号链接便无法重定向
// 特权读或写。
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
func localSourceDigest(root string) (string, error) {
	if err := safeLocalPath(filepath.Join(root, "go.mod"), false); err != nil {
		return "", err
	}
	if err := safeLocalPath(filepath.Join(root, "Dockerfile"), false); err != nil {
		return "", err
	}
	files := []string{}
	for _, name := range []string{"cmd", "internal", "migrations", "authz", "third_party/license-core", "go.mod", "go.sum", "Dockerfile", "docker-entrypoint.sh"} {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if resolved, err := filepath.EvalSymlinks(path); err != nil || resolved != path {
					return fmt.Errorf("unsafe development source directory")
				}
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in development source")
			}
			if !entry.IsDir() {
				if !entry.Type().IsRegular() {
					return fmt.Errorf("invalid development source")
				}
				if err := safeLocalPath(path, false); err != nil {
					return err
				}
				rel, e := filepath.Rel(root, path)
				if e != nil {
					return e
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("development source unavailable")
	}
	sort.Strings(files)
	var b strings.Builder
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s\x00%s\n", name, localDigest(raw))
	}
	return localDigest([]byte(b.String())), nil
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
func (p *LocalDockerSubsystemProvisioner) localRelease(ctx context.Context, app, env string) (localRuntimeRelease, error) {
	fail := func() (localRuntimeRelease, error) {
		return localRuntimeRelease{}, application.ErrRuntimeLicenseApprovalMismatch
	}
	if p == nil || !p.config.Enabled || env != "dev" || p.config.PlatformComposeProject != "basic-platform-local" || len(localLicenseServices[app]) == 0 {
		return localRuntimeRelease{}, application.ErrRuntimeLicenseApprovalUnavailable
	}
	root := p.localPlatformRoot()
	var release localRuntimeRelease
	if readLocalPrivateJSON(filepath.Join(root, "runtime-approvals", "local-"+app+"-dev.json"), &release) != nil {
		return localRuntimeRelease{}, application.ErrRuntimeLicenseApprovalUnavailable
	}
	if release.Version != 1 || release.Project != p.config.PlatformComposeProject || release.Application != app || release.Environment != env || !release.Reviewed || !validLocalReviewer(release.Operator) || application.ValidateRuntimeLicenseApprovals(release.Components) != nil {
		return fail()
	}
	if err := safeLocalPath(p.localComposeFile(), false); err != nil {
		return fail()
	}
	raw, err := os.ReadFile(p.localComposeFile())
	if err != nil || localDigest(raw) != release.ComposeSHA256 {
		return fail()
	}
	directory := app
	if app == "customer_portal" {
		directory = "customer_and_opportunity"
	}
	if app == "settlement" {
		directory = "Settlement"
	}
	source, err := localSourceDigest(filepath.Join(filepath.Dir(root), directory))
	if err != nil || source != release.SourceDigest {
		return fail()
	}
	expected := localLicenseServices[app]
	if len(release.Components) != len(expected) || len(release.Services) != len(expected) {
		return fail()
	}
	coverage := localDigest([]byte("dev-source-fingerprint\x00" + app + "\x00" + source))
	components := map[string]application.RuntimeLicenseApproval{}
	services := map[string]licenseevidence.Service{}
	for _, item := range release.Components {
		components[item.ServiceID] = item
	}
	for _, item := range release.Services {
		if _, ok := services[item.Name]; ok {
			return fail()
		}
		services[item.Name] = item
	}
	for _, id := range expected {
		c, ok := components[id]
		s := services[id]
		if !ok || c.CoverageDigest != coverage || s.Application != app || s.Environment != env || s.Protocol != "1" || s.Replicas != 1 || s.ImageDigest != c.ImageDigest || !localRuntimeVersion.MatchString(s.Version) {
			return fail()
		}
		b, err := p.localImageProjection(ctx, c.ImageDigest)
		if err != nil {
			return localRuntimeRelease{}, err
		}
		if b.ID != c.ImageDigest || b.Version != s.Version || b.Protocol != "1" {
			return fail()
		}
	}
	m := release.Migration
	if m.Service != localLicenseMigrations[app] || !localRuntimeDigest.MatchString(m.ImageDigest) || !localRuntimeVersion.MatchString(m.Version) || m.Protocol != "1" {
		return fail()
	}
	image, e := p.localImageProjection(ctx, m.ImageDigest)
	if e != nil {
		return localRuntimeRelease{}, e
	}
	if image.ID != m.ImageDigest || image.Version != m.Version || image.Protocol != m.Protocol {
		return fail()
	}
	return release, nil
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

// ApprovedRuntimeLicenseComponents 返回本地发行已批准的运行时组件清单；读取全程持有
// provisioner 互斥锁，未建立本地发行审批时返回错误而非空清单。
func (p *LocalDockerSubsystemProvisioner) ApprovedRuntimeLicenseComponents(ctx context.Context, app, env string) ([]application.RuntimeLicenseApproval, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	r, e := p.localRelease(ctx, app, env)
	return r.Components, e
}

// SubsystemImageReadiness 检查批准清单中的镜像在本机是否存在（只读，不改变任何状态）。
// 语义：摘要匹配 → Present=true；镜像缺失 → Present=false；Docker 不可用 → 返回错误（由上层按「未验证」处理，不谎报缺失）。
func (p *LocalDockerSubsystemProvisioner) SubsystemImageReadiness(ctx context.Context, applicationCode, environment string) ([]application.SubsystemImageReadiness, error) {
	components, err := p.ApprovedRuntimeLicenseComponents(ctx, applicationCode, environment)
	if err != nil {
		return nil, err
	}
	readiness := make([]application.SubsystemImageReadiness, 0, len(components))
	for _, component := range components {
		item := application.SubsystemImageReadiness{ServiceID: component.ServiceID, ImageDigest: component.ImageDigest}
		projection, projectionErr := p.localImageProjection(ctx, component.ImageDigest)
		if errors.Is(projectionErr, application.ErrRuntimeLicenseDockerUnavailable) {
			return nil, projectionErr
		}
		item.Present = projectionErr == nil && strings.EqualFold(strings.TrimSpace(projection.ID), strings.TrimSpace(component.ImageDigest))
		readiness = append(readiness, item)
	}
	return readiness, nil
}

// ApprovedRuntimeLicenseLifecycle 返回本地发行的许可证生命周期视图，包含组件与由发行内容
// 摘要派生的 ReleaseGeneration；未建立本地发行审批时失败关闭。
func (p *LocalDockerSubsystemProvisioner) ApprovedRuntimeLicenseLifecycle(ctx context.Context, app, env string) (application.RuntimeLicenseReleaseApproval, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	r, e := p.localRelease(ctx, app, env)
	if e != nil {
		return application.RuntimeLicenseReleaseApproval{}, e
	}
	return localLifecycle(r), nil
}
func localLifecycle(r localRuntimeRelease) application.RuntimeLicenseReleaseApproval {
	binding, _ := json.Marshal([]any{r.ComposeSHA256, r.SourceDigest, r.Migration})
	generation := "dev-" + strings.TrimPrefix(localDigest(binding), "sha256:")[:32]
	out := application.RuntimeLicenseReleaseApproval{Components: r.Components, ReleaseGeneration: generation}
	for _, c := range r.Components {
		out.RequiredServiceIDs = append(out.RequiredServiceIDs, c.ServiceID)
	}
	return out
}

// CollectLicenseEvidence 基于已评审的本地 dev 发行采集许可证证据；发行未建立时返回
// LICENSE_EVIDENCE_UNSUPPORTED，Docker 投影仅限白名单只读命令。
func (p *LocalDockerSubsystemProvisioner) CollectLicenseEvidence(ctx context.Context, app, env string) (licenseevidence.Report, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	r, err := p.localRelease(ctx, app, env)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: reviewed local dev release required")
	}
	collector, err := licenseevidence.NewCollector(localLicenseEvidenceRunner{p}, licenseevidence.Config{Project: r.Project, Services: r.Services, Timeout: 30 * time.Second})
	if err != nil {
		return licenseevidence.Report{}, err
	}
	return collector.Collect(ctx), nil
}

type localPinnedService struct {
	PullPolicy string   `json:"pull_policy,omitempty"`
	Image      string   `json:"image"`
	EnvFile    []string `json:"env_file,omitempty"`
	Volumes    []string `json:"volumes,omitempty"`
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
func (p *LocalDockerSubsystemProvisioner) deliverLocalRuntimeLicense(ctx context.Context, input application.SubsystemProvisioningInput) error {
	var legacy, builtin []application.RuntimeLicenseCredential
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.RegistrationProtocol == catalog.RegistrationProtocolV2 {
			builtin = append(builtin, credential)
		} else {
			legacy = append(legacy, credential)
		}
	}
	if len(legacy) > 0 && len(builtin) > 0 {
		legacyInput, builtinInput := input, input
		legacyInput.RuntimeLicenseCredentials = legacy
		builtinInput.RuntimeLicenseCredentials = builtin
		if err := p.deliverLocalLegacyRuntimeLicense(ctx, legacyInput, true); err != nil {
			return err
		}
		return p.deliverLocalBuiltinRuntimeLicense(ctx, builtinInput, true)
	}
	if len(builtin) > 0 {
		return p.deliverLocalBuiltinRuntimeLicense(ctx, input, false)
	}
	return p.deliverLocalLegacyRuntimeLicense(ctx, input, false)
}

func (p *LocalDockerSubsystemProvisioner) deliverLocalLegacyRuntimeLicense(ctx context.Context, input application.SubsystemProvisioningInput, allowSubset bool) error {
	path := filepath.Join(p.localPlatformRoot(), "runtime-approvals", "local-"+input.ApplicationCode+"-dev.json")
	if len(input.RuntimeLicenseCredentials) == 0 {
		if _, err := os.Lstat(path); err == nil {
			// 同时保留子系统供应失败语义（既有错误处理）与授权实例未初始化语义（用户可读诊断）。
			return localReleaseCredentialError{message: "LICENSE_RUNTIME_DELIVERY_FAILED: 已批准本地发行必须交付组件凭据；请先在「商业授权」完成部署实例初始化并导入签发产物，再回到当前环境重试接入"}
		}
		return nil
	}
	release, err := p.localRelease(ctx, input.ApplicationCode, input.Environment)
	if err != nil {
		return err
	}
	digest, err := application.RuntimeLicenseReleaseDigest(localLifecycle(release))
	if err != nil {
		return err
	}
	settings := input.RuntimeLicenseSettings
	if settings.InstanceID == "" || settings.Environment == "" || settings.PlatformBaseURL != "http://platform-api:8080" || settings.PlatformPublicKeyPath != "/app/data/keys/jwt-ed25519-public.pem" || settings.StateDirectory != "/var/lib/commercial-license" || !settings.AllowHTTP {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local runtime binding incomplete")
	}
	if !allowSubset && len(input.RuntimeLicenseCredentials) != len(release.Components) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local component set mismatch")
	}
	expected := map[string]application.RuntimeLicenseApproval{}
	for _, c := range release.Components {
		expected[c.ServiceID] = c
	}
	root := p.localPlatformRoot()
	runtimeDir := filepath.Join(root, "runtime")
	if err := ensureLocalRuntimeDirectory(runtimeDir); err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(runtimeDir); err != nil || resolved != runtimeDir {
		return provisioningError("unsafe local runtime directory")
	}
	override := localPinnedCompose{Services: map[string]localPinnedService{}, Volumes: map[string]struct{}{}}
	overridePath := filepath.Join(runtimeDir, "local-license.compose.json")
	if existing, err := p.localRuntimeOverride(); err != nil {
		return err
	} else if existing != "" {
		if err = readLocalPrivateJSON(existing, &override); err != nil {
			return err
		}
	}
	// 升级早期 pin 文档，但不改变镜像或状态身份。
	for id, item := range override.Services {
		item.PullPolicy = "never"
		item.Environment = localProjectBackgroundEnvironment(id, item.Image)
		override.Services[id] = item
	}
	updates := map[string]map[string]string{}
	for _, credential := range input.RuntimeLicenseCredentials {
		c := credential.Approval
		if wanted, ok := expected[c.ServiceID]; !ok || !reflect.DeepEqual(wanted, c) || credential.ClientID == "" || credential.ClientSecret == "" || credential.ReleaseDigest != digest {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: local approved binding mismatch")
		}
		delete(expected, c.ServiceID)
		values := map[string]string{"COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_INSTANCE_ID": settings.InstanceID, "COMMERCIAL_LICENSE_ENVIRONMENT": settings.Environment, "COMMERCIAL_LICENSE_SERVICE_ID": c.ServiceID, "COMMERCIAL_LICENSE_STATE_PATH": runtimeLicenseStatePath(settings, credential), "COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH": settings.PlatformPublicKeyPath, "COMMERCIAL_LICENSE_PLATFORM_BASE_URL": settings.PlatformBaseURL, "COMMERCIAL_LICENSE_CLIENT_ID": credential.ClientID, "COMMERCIAL_LICENSE_CLIENT_SECRET": credential.ClientSecret, "COMMERCIAL_LICENSE_COVERAGE_DIGEST": c.CoverageDigest, "COMMERCIAL_LICENSE_IMAGE_DIGEST": c.ImageDigest, "COMMERCIAL_LICENSE_ALLOW_HTTP": "true"}
		for k, v := range values {
			if !validEnvironmentKey(k) || !validEnvironmentValue(v) {
				return provisioningError("invalid local runtime credential")
			}
		}
		envPath := filepath.Join(runtimeDir, "license-"+c.ServiceID+".env")
		if err := safeLocalPath(envPath, true); err != nil {
			return err
		}
		updates[envPath] = values
		// 具名卷继承不可变镜像目录的属主，使非 root 的 Project 与 Settlement 消费者可以
		// 持久化其状态。
		override.Volumes["license-state-"+c.ServiceID] = struct{}{}
		override.Services[c.ServiceID] = localPinnedService{PullPolicy: "never", Image: c.ImageDigest, EnvFile: []string{envPath}, Volumes: []string{"license-state-" + c.ServiceID + ":/var/lib/commercial-license", filepath.Join(root, "data", "keys") + ":/app/data/keys:ro"}}
		item := override.Services[c.ServiceID]
		item.Environment = localProjectBackgroundEnvironment(c.ServiceID, c.ImageDigest)
		override.Services[c.ServiceID] = item
	}
	for path, values := range updates {
		if err := writeIsolatedRuntimeLicenseEnvironment(path, values); err != nil {
			return err
		}
	}
	override.Services[release.Migration.Service] = localPinnedService{PullPolicy: "never", Image: release.Migration.ImageDigest}
	raw, err := json.Marshal(override)
	if err != nil {
		return err
	}
	return writeLocalRuntimeJSON(overridePath, raw)
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
	out, err := p.localDockerProjectionLimit(ctx, 2<<20, "compose", "-p", p.config.PlatformComposeProject,
		"--env-file", filepath.Join(root, "docker", ".env.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "contract_management", ".env.local"),
		"--env-file", filepath.Join(root, "docker", ".env.customer.local"),
		"--env-file", filepath.Join(root, "docker", ".env.portal.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "project_management", ".env.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "data_analysis", ".env.local"),
		"--env-file", filepath.Join(filepath.Dir(root), "Settlement", ".env.local"),
		"--profile", "bootstrap", "--profile", "customer", "--profile", "portal", "--profile", "project", "--profile", "data-analysis", "--profile", "presale-worker",
		"-f", composePath, "config", "--format", "json")
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
func (p *LocalDockerSubsystemProvisioner) deployLicensedLocalStack(ctx context.Context, input application.SubsystemProvisioningInput) error {
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
	if err := run(ctx, "run", "--rm", "--no-deps", migration); err != nil {
		return err
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

func validLocalReviewer(value string) bool {
	if strings.TrimSpace(value) == "" || len([]rune(value)) > 128 {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// encoding/json 默认接受重复键。批准与 pin 文档对 Python 审阅器和 Go Agent 必须只有
// 一种含义。
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
