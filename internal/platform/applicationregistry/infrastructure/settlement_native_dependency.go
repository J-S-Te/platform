package infrastructure

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const settlementContractBindingPrefix = "related_service.contract_management.settlement_contract_ingest."

var settlementContractIdentityFields = []struct{ key, source string }{
	{"CLIENT_ID", "client_id"}, {"OAUTH_CLIENT_ID", "oauth_client_id"},
	{"APPLICATION_ID", "application_id"}, {"ENVIRONMENT_ID", "environment_id"},
}

// nativeSettlementContractOptional 只承认原生财务明确开启且合同接入明确关闭的组合。
// 参数：values为Agent读取的受控运行文件，不来自浏览器或context声明。
// 返回值：可选标记；非法布尔值返回错误，缺省或空值保持严格依赖。
func nativeSettlementContractOptional(values map[string]string) (bool, error) {
	native, disabled := false, false
	for _, key := range []string{"SETTLEMENT_NATIVE_FINANCIAL_ENABLED", "SETTLEMENT_INTEGRATION_ENABLED"} {
		raw := values[key]
		if raw == "" {
			continue
		}
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return false, provisioningError("invalid Settlement business source mode")
		}
		if key == "SETTLEMENT_NATIVE_FINANCIAL_ENABLED" {
			native = value
		} else {
			disabled = !value
		}
	}
	return native && disabled, nil
}

// readSettlementSourceValues 只读既有普通运行文件，缺失文件不产生可选策略。
// 参数：path为Agent由审核目标推导的固定文件路径。
// 返回值：非敏感判断所用配置；文件异常时失败关闭，不返回文件内容或凭据。
func readSettlementSourceValues(path string) (map[string]string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, provisioningError("Settlement runtime source policy is unavailable")
	}
	values, err := readEnvironmentValues(path)
	if err != nil {
		return nil, provisioningError("Settlement runtime source policy is unreadable")
	}
	optional, err := nativeSettlementContractOptional(values)
	if err != nil {
		return nil, err
	}
	if optional && info.Mode().Perm()&0077 != 0 {
		return nil, provisioningError("native Settlement runtime source requires private file permissions")
	}
	return values, nil
}

// resolveSettlementRuntimeBinding 仅对精确本地财务模式放宽不存在的四项合同公开身份。
// 参数：input为可信目标及已登记来源，source为审核绑定键，current为Agent读取的现值。
// 返回值：严格绑定值；明确可选且来源缺失返回空，已登记来源始终执行原严格校验。
func resolveSettlementRuntimeBinding(input application.SubsystemProvisioningInput, source string, current map[string]string) (string, error) {
	if input.ApplicationCode != "settlement" || !strings.HasPrefix(source, settlementContractBindingPrefix) {
		return resolveProductionBinding(input, source)
	}
	optional, err := nativeSettlementContractOptional(current)
	if err != nil {
		return "", err
	}
	for _, related := range input.RelatedServiceClients {
		if related.ApplicationCode == "contract_management" && related.Purpose == application.ServiceCredentialSettlementContractIngest {
			if related.ApplicationID == "" || related.EnvironmentID == "" ||
				related.Client.ClientID != "contract_management-"+input.Environment+"-settlement-integration" {
				return "", provisioningError("registered contract source identity is incomplete or drifted")
			}
			return resolveProductionBinding(input, source)
		}
	}
	if optional {
		for _, field := range settlementContractIdentityFields {
			if source == settlementContractBindingPrefix+field.source {
				return "", nil
			}
		}
	}
	return resolveProductionBinding(input, source)
}

// resolveRuntimeBinding 按本目标的受控文件判断合同依赖，其他映射保留原规则。
// 参数：input为已授权目标，file为审核运行文件，source为固定绑定来源。
// 返回值：真实身份或显式本地无来源空值；文件错误及身份漂移仍返回错误。
func (target *productionComposeTarget) resolveRuntimeBinding(input application.SubsystemProvisioningInput, file productionSubsystemRuntimeFileManifest, source string) (string, error) {
	if input.ApplicationCode != "settlement" || !strings.HasPrefix(source, settlementContractBindingPrefix) {
		return resolveProductionBinding(input, source)
	}
	current, err := target.readSettlementRuntimeSource(file.Path)
	if err != nil {
		return "", err
	}
	return resolveSettlementRuntimeBinding(input, source, current)
}

// readSettlementRuntimeSource 读取审核目标内的运行配置，拒绝父目录符号链接越界。
// 参数：relative为已审核profile声明的相对运行文件。
// 返回值：现有配置或严格缺省空值；任何实际解析越界返回错误。
func (target *productionComposeTarget) readSettlementRuntimeSource(relative string) (map[string]string, error) {
	path := filepath.Join(target.config.DeployRoot, filepath.FromSlash(relative))
	if !pathWithinRoot(target.config.DeployRoot, path) {
		return nil, provisioningError("Settlement runtime source path is invalid")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil && !pathWithinRoot(target.config.DeployRoot, resolved) {
		return nil, provisioningError("Settlement runtime source resolves outside target")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, provisioningError("Settlement runtime source cannot be resolved")
	}
	return readSettlementSourceValues(path)
}

// settlementContractRuntimeValues 生成本地结算的四项权威合同身份或明确空来源。
// 参数：input为已登记目标，current为既有运行值，values为自动生成配置。
// 返回值：缺省缺依赖、已登记漂移或非法模式返回错误。
func settlementContractRuntimeValues(input application.SubsystemProvisioningInput, current, values map[string]string) error {
	optional, err := nativeSettlementContractOptional(current)
	if err != nil {
		return err
	}
	for _, field := range settlementContractIdentityFields {
		value, err := resolveSettlementRuntimeBinding(input, settlementContractBindingPrefix+field.source, current)
		if err != nil {
			return err
		}
		values["CONTRACT_INTEGRATION_"+field.key] = value
	}
	values["SETTLEMENT_INTEGRATION_ENABLED"] = booleanEnvironmentValue(!optional)
	return nil
}

// settlementContractOptionalEnvironment 投影生产审核目标的明确原生结算环境。
// 参数：无；只读取接收者固定profile中的结算运行文件。
// 返回值：可选环境或空；多个来源文件、越界路径及非法模式返回错误。
func (target *productionComposeTarget) settlementContractOptionalEnvironment() (string, error) {
	if target.config.Profile.Manifest.Application.Code != "settlement" {
		return "", nil
	}
	path := ""
	for _, file := range target.selectedRuntimeFiles() {
		if file.ComposeEnvironmentKey != "SETTLEMENT_RUNTIME_ENV_FILE" {
			continue
		}
		if path != "" {
			return "", provisioningError("Settlement runtime source is ambiguous")
		}
		path = file.Path
	}
	if path == "" {
		return "", provisioningError("Settlement runtime source is invalid")
	}
	values, err := target.readSettlementRuntimeSource(path)
	if err != nil {
		return "", err
	}
	optional, err := nativeSettlementContractOptional(values)
	if err != nil || !optional {
		return "", err
	}
	return target.config.Profile.Manifest.Application.Environment, nil
}
