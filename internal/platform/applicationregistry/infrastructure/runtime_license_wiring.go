package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type runtimeLicenseComposeMount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type runtimeLicenseComposeService struct {
	Environment map[string]string            `json:"environment"`
	Volumes     []runtimeLicenseComposeMount `json:"volumes"`
}

// validateRuntimeLicenseWiring 用途：在重建消费者前确认 V2 私有配置、公钥及独立状态卷实际进入编排。
// 参数：ctx 为部署上下文，input 为已登记并下发的组件与实例绑定。
// 返回值：配置缺失或不匹配时失败关闭；错误不包含配置值或凭据。
func (target *productionComposeTarget) validateRuntimeLicenseWiring(ctx context.Context, input application.SubsystemProvisioningInput) error {
	services := []string{}
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.RegistrationProtocol == 2 {
			services = append(services, credential.Approval.ServiceID)
		}
	}
	if len(services) == 0 {
		return nil
	}
	raw, err := target.runComposeOutput(ctx, "config", "--format", "json")
	if err != nil {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: Compose configuration unavailable")
	}
	var model struct {
		Services map[string]runtimeLicenseComposeService `json:"services"`
	}
	if json.Unmarshal(raw, &model) != nil {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: Compose configuration invalid")
	}
	stateVolumes := map[string]bool{}
	for _, service := range services {
		path := filepath.Join(target.config.DeployRoot, "runtime", "license-"+service+".env")
		expected, readErr := os.ReadFile(path)
		if readErr != nil {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: credential unavailable")
		}
		if err := validateRuntimeLicenseConsumer(service, model.Services[service],
			parseEnvironmentValues(string(expected)), input.RuntimeLicenseSettings, stateVolumes); err != nil {
			return err
		}
	}
	return nil
}

// validateRuntimeLicenseConsumer 用途：核对单个消费者的解析配置且禁止多个消费者共用状态卷。
// 参数：service 为组件名，actual 为解析后的编排，expected 为私有文件，settings 为实例路径，states 为已占用卷。
// 返回值：匹配时返回 nil；否则返回仅含组件名及配置项名的诊断。
func validateRuntimeLicenseConsumer(service string, actual runtimeLicenseComposeService,
	expected map[string]string, settings application.RuntimeLicenseSettings, states map[string]bool) error {
	for key, value := range expected {
		if actual.Environment[key] != value {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: " + service + " missing or overridden " + key)
		}
	}
	state, publicKey := "", false
	for _, mount := range actual.Volumes {
		if mount.Target == settings.StateDirectory && mount.Type == "volume" && !mount.ReadOnly {
			state = mount.Source
		}
		if mount.Target == settings.PlatformPublicKeyPath && mount.ReadOnly && mount.Source != "" {
			publicKey = true
		}
	}
	if state == "" || states[state] || !publicKey {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: " + service + " requires private state volume and readonly public key")
	}
	states[state] = true
	return nil
}
