package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// errLicenseRuntimeStateUnavailable 是“目标运行配置要求商业许可运行态、但本次没有可交付的
// 许可运行凭据（或固定记录缺少 license-state 卷）”这一 fail-closed 判定的专用错误。
// 它同时暴露稳定诊断码 ErrRuntimeLicenseStateUnavailable，供 HTTP 层映射为可执行指引。
var errLicenseRuntimeStateUnavailable = application.ErrRuntimeLicenseStateUnavailable

// licenseRuntimeStateUnavailableError 构造带应用编码的专用错误；应用编码让运维不必翻日志
// 就能确认是哪个目标被拦下（例如 license runtime state is unavailable for project_management）。
func licenseRuntimeStateUnavailableError(applicationCode string, causes ...error) error {
	code := strings.TrimSpace(applicationCode)
	if code == "" {
		code = "unknown application"
	}
	result := fmt.Errorf("%w for %s", errLicenseRuntimeStateUnavailable, code)
	for _, cause := range causes {
		if cause == nil {
			continue
		}
		if detail := singleLineFailureDetail(cause.Error(), nil); detail != "" {
			result = fmt.Errorf("%w (%s)", result, detail)
		}
	}
	return result
}

// licenseRuntimeStateEnabled 判定一份已解析的运行配置是否强制要求商业许可运行态。
// 只有归一后等于 true 才算要求；false/缺省保持既有的兼容启动语义。
func licenseRuntimeStateEnabled(values map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(values["COMMERCIAL_LICENSE_ENABLED"]), "true")
}

// licenseRuntimeStateRequired 读取目标运行文件，判断其中是否有文件要求商业许可运行态。
// 文件缺失或不可读不构成失败：它们无法把许可要求带进容器，判定交给后续读取路径。
func licenseRuntimeStateRequired(paths ...string) bool {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		values, err := readEnvironmentValues(path)
		if err != nil {
			continue
		}
		if licenseRuntimeStateEnabled(values) {
			return true
		}
	}
	return false
}

// localLicenseRuntimeStateFiles 列出本次本地重建中真正会进入目标容器环境的运行文件：
// 平台签发的 runtime/license-<service>.env 与子系统自身的环境文件。
func (provisioner *LocalDockerSubsystemProvisioner) localLicenseRuntimeStateFiles(input application.SubsystemProvisioningInput) []string {
	paths := []string{}
	runtimeDirectory := filepath.Join(provisioner.localPlatformRoot(), "runtime")
	for _, service := range localLicenseServices[input.ApplicationCode] {
		paths = append(paths, filepath.Join(runtimeDirectory, "license-"+service+".env"))
	}
	directory, err := provisioner.projectDirectory(input.ApplicationCode)
	if err != nil {
		return paths
	}
	source, destination, pathErr := provisioner.subsystemEnvironmentPaths(input.ApplicationCode, directory)
	if pathErr != nil {
		// 模板缺失不影响 fail-fast 判定：已经存在的目标运行文件仍必须被检查。
		return append(paths, filepath.Join(directory, ".env.local"))
	}
	return append(paths, source, destination)
}

// requireLocalLicenseRuntimeState 在创建或重建任何容器之前拒绝“运行配置要求商业许可运行态、
// 但本次没有许可运行凭据”的组合。放行只会让目标 API 启动即失败并每分钟重启，Compose 永远
// 起不来，平台侧最后只剩一句“部署 Agent 执行失败”。
// 参数：input 为已授权的目标与本次公开运行配置（凭据为空即未交付许可运行态）。
// 返回值：需要许可运行态却没有凭据时返回专用错误；其余情况返回 nil，不改动任何容器。
func (provisioner *LocalDockerSubsystemProvisioner) requireLocalLicenseRuntimeState(input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) > 0 {
		return nil
	}
	if !licenseRuntimeStateRequired(provisioner.localLicenseRuntimeStateFiles(input)...) {
		return nil
	}
	return licenseRuntimeStateUnavailableError(input.ApplicationCode)
}

// requireLicensedLocalRuntimeVolumes 在启动任何受许可容器前确认固定记录为本次要启动的每个
// 受许可组件声明了 license-state-<service> 卷与只读公钥挂载。缺失即失败关闭：静默生成一个
// 没有持久许可状态的容器，等于把故障推迟到容器的启动日志里。
// 参数：input 为带运行凭据的受控部署输入。
// 返回值：固定记录缺失、不合法或卷声明不完整时返回专用错误。
func (provisioner *LocalDockerSubsystemProvisioner) requireLicensedLocalRuntimeVolumes(input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	path, err := provisioner.localRuntimeOverride()
	if err != nil {
		return licenseRuntimeStateUnavailableError(input.ApplicationCode, err)
	}
	if strings.TrimSpace(path) == "" {
		return licenseRuntimeStateUnavailableError(input.ApplicationCode,
			errors.New("licensed Compose override is absent"))
	}
	override := localPinnedCompose{Services: map[string]localPinnedService{}, Volumes: map[string]struct{}{}}
	if err = readLocalPrivateJSON(path, &override); err != nil {
		return licenseRuntimeStateUnavailableError(input.ApplicationCode, err)
	}
	publicKeyMount := filepath.Join(provisioner.localPlatformRoot(), "data", "keys") + ":/app/data/keys:ro"
	for _, service := range localLicenseServices[input.ApplicationCode] {
		pinned, ok := override.Services[service]
		if !ok {
			return licenseRuntimeStateUnavailableError(input.ApplicationCode,
				errors.New("license state volume is missing for "+service))
		}
		if _, declared := override.Volumes["license-state-"+service]; !declared {
			return licenseRuntimeStateUnavailableError(input.ApplicationCode,
				errors.New("license state volume is missing for "+service))
		}
		if !slices.Contains(pinned.Volumes, "license-state-"+service+":/var/lib/commercial-license") ||
			!slices.Contains(pinned.Volumes, publicKeyMount) ||
			len(pinned.EnvFile) == 0 {
			return licenseRuntimeStateUnavailableError(input.ApplicationCode,
				errors.New("license state volume is missing for "+service))
		}
	}
	return nil
}

// productionLicenseRuntimeStateFiles 列出生产重建中会进入目标容器环境的许可运行文件：
// 平台签发到 runtime/ 的组件凭据，以及受控运行配置本身。
func (target *productionComposeTarget) productionLicenseRuntimeStateFiles() []string {
	paths := []string{}
	runtimeDirectory := filepath.Join(target.config.DeployRoot, "runtime")
	for _, service := range target.selectedRuntimeLicenseConsumers() {
		paths = append(paths, filepath.Join(runtimeDirectory, "license-"+service+".env"))
	}
	return append(paths, strings.TrimSpace(target.config.RuntimeEnvPath))
}

// requireProductionLicenseRuntimeState 在创建或重建任何生产容器之前拒绝“运行配置要求商业
// 许可运行态、但本次没有许可运行凭据”的组合。它覆盖“上一次受控接入写入了
// runtime/license-<service>.env、本次平台却没有可交付凭据”的重复重试场景。
// 参数：input 为已授权目标与本次公开运行配置。
// 返回值：需要许可运行态却没有凭据时返回专用错误；其余情况返回 nil。
func (target *productionComposeTarget) requireProductionLicenseRuntimeState(input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) > 0 {
		return nil
	}
	if !licenseRuntimeStateRequired(target.productionLicenseRuntimeStateFiles()...) {
		return nil
	}
	return licenseRuntimeStateUnavailableError(target.config.Profile.Manifest.Application.Code)
}

// subsystemCommandOutputRunner 是可选能力：真实 Agent runner 与部分测试 fake 支持捕获输出。
// runner 不实现时诊断自动降级为原错误，既有错误语义不变。
type subsystemCommandOutputRunner interface {
	RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
}

// deploymentDiagnosticSecrets 收集本次受控操作交付的明文凭据，仅用于失败诊断脱敏；
// 任何凭据都不会写回错误、日志或状态表。
func deploymentDiagnosticSecrets(input application.SubsystemProvisioningInput) []string {
	values := make([]string, 0, 2+len(input.ServiceCredentials)+len(input.RuntimeLicenseCredentials))
	for _, value := range []string{input.ClientSecret, input.CatalogPublisherClientSecret} {
		if value != "" {
			values = append(values, value)
		}
	}
	for _, credential := range input.ServiceCredentials {
		if credential.PlaintextSecret != "" {
			values = append(values, credential.PlaintextSecret)
		}
	}
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.ClientSecret != "" {
			values = append(values, credential.ClientSecret)
		}
	}
	return values
}

// singleLineFailureDetail 把命令输出或下层错误压成单行、限长、已脱敏的诊断摘要。
// 单行与限长是硬约束：subsystem_deployment_state.last_error_message 是 varchar(1000)，
// 换行或超长会破坏状态接口的 JSON 与写入。limit<=0 时使用默认 800 字符。
func singleLineFailureDetail(raw string, secrets []string) string {
	detail := strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' {
			return ' '
		}
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, raw)
	for _, secret := range secrets {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
	}
	detail = strings.Join(strings.Fields(detail), " ")
	const limit = 800
	if utf8.RuneCountInString(detail) > limit {
		// 按 rune 截断：按字节切会切碎多字节字符，JSON 编码随后会把它们替换成 U+FFFD。
		detail = string([]rune(detail)[:limit]) + "...(truncated)"
	}
	return detail
}

// localComposeTargetServices 从完整的 Compose 参数中取出本次操作的受控服务名，
// 供失败时抓取这些容器最近日志。只对会创建/启动容器的子命令返回服务集合。
func localComposeTargetServices(composeArguments []string) []string {
	index, subcommand := -1, ""
	for position, argument := range composeArguments {
		switch argument {
		case "up", "run", "start", "restart":
			index, subcommand = position, argument
		}
		if index >= 0 {
			break
		}
	}
	if index < 0 {
		return nil
	}
	services := []string{}
	for _, argument := range composeArguments[index+1:] {
		if strings.HasPrefix(argument, "-") {
			continue
		}
		services = append(services, argument)
		// run 的子命令参数不是服务名；up 最多取四个目标，避免日志请求无限放大。
		if subcommand == "run" || len(services) == 4 {
			break
		}
	}
	return services
}

// localComposeServiceLogs 抓取失败服务最近 40 行日志并压成单行摘要；抓取本身失败时返回
// 空串，回退为 Compose 输出，绝不影响原始错误语义。
func (provisioner *LocalDockerSubsystemProvisioner) localComposeServiceLogs(ctx context.Context, composeArguments []string) string {
	services := localComposeTargetServices(composeArguments)
	if len(services) == 0 {
		return ""
	}
	outputRunner, ok := provisioner.runner.(subsystemCommandOutputRunner)
	if !ok {
		return ""
	}
	logContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	arguments := append([]string{
		"compose", "--project-name", provisioner.config.PlatformComposeProject,
		"logs", "--tail", "40", "--no-color",
	}, services...)
	output, err := outputRunner.RunOutput(logContext, provisioner.localPlatformRoot(), os.Environ(),
		provisioner.config.DockerBinary, arguments...)
	if err != nil {
		return ""
	}
	return singleLineFailureDetail(string(output), provisioner.operationSecrets)
}

// localComposeFailureReason 汇总 Compose 自身的失败输出与失败服务日志，作为下层原因并入
// 错误链。运维因此能从状态表与 API details 直接看到“商业许可配置失败”等原因。
func (provisioner *LocalDockerSubsystemProvisioner) localComposeFailureReason(ctx context.Context, composeArguments []string, output []byte) string {
	parts := []string{}
	if detail := singleLineFailureDetail(string(output), provisioner.operationSecrets); detail != "" {
		parts = append(parts, "compose output: "+detail)
	}
	if detail := provisioner.localComposeServiceLogs(ctx, composeArguments); detail != "" {
		parts = append(parts, "recent service logs: "+detail)
	}
	return strings.Join(parts, " | ")
}

// runStandaloneCompose 执行独立子系统自有 Compose 文件的操作；失败时把命令输出（截断并
// 脱敏）并入错误链，让独立子系统的重建失败同样可诊断。
func (provisioner *LocalDockerSubsystemProvisioner) runStandaloneCompose(ctx context.Context, projectDirectory, environmentPath, composeFile string, arguments ...string) error {
	composeArguments := append([]string{
		"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile,
	}, arguments...)
	outputRunner, ok := provisioner.runner.(subsystemCommandOutputRunner)
	if !ok {
		return provisioner.runner.Run(ctx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, composeArguments...)
	}
	output, runErr := outputRunner.RunOutput(ctx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, composeArguments...)
	if runErr == nil {
		return nil
	}
	if detail := singleLineFailureDetail(string(output), provisioner.operationSecrets); detail != "" {
		return fmt.Errorf("%w: compose output: %s", runErr, detail)
	}
	return runErr
}
