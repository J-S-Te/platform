package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/license-core/catalog"
)

// writeRuntimeLicenseCredentials 为每个内置目录组件写一个 mode-0600、Agent 持有的
// env 文件；组件秘密绝不进入应用级共享 env。
// V1 审批交付已下线：只要出现非 V2 凭据就 fail-closed，不回退到按文件审批的路径。
func (target *productionComposeTarget) writeRuntimeLicenseCredentials(input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.RegistrationProtocol != catalog.RegistrationProtocolV2 {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: legacy V1 runtime credential is no longer supported")
		}
	}
	return target.writeBuiltinRuntimeLicenseCredentials(input, false)
}

func (target *productionComposeTarget) writeBuiltinRuntimeLicenseCredentials(input application.SubsystemProvisioningInput, allowSubset bool) error {
	settings := input.RuntimeLicenseSettings
	if settings.InstanceID == "" || settings.Environment == "" || settings.PlatformBaseURL == "" || !filepath.IsAbs(settings.PlatformPublicKeyPath) || !filepath.IsAbs(settings.StateDirectory) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: runtime binding incomplete")
	}
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), input.ApplicationCode, input.Environment)
	if err != nil {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: component catalog unavailable")
	}
	selected := map[string]bool{}
	for _, id := range target.selectedRuntimeLicenseConsumers() {
		selected[id] = true
	}
	if !allowSubset && len(selected) != len(input.RuntimeLicenseCredentials) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: component set mismatch")
	}
	expected := map[string]application.RuntimeComponentSpec{}
	for _, component := range set.Components {
		if selected[component.ServiceID] {
			expected[component.ServiceID] = component
		}
	}
	if !allowSubset && len(expected) != len(input.RuntimeLicenseCredentials) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: component catalog mismatch")
	}
	updates := map[string]map[string]string{}
	for _, credential := range input.RuntimeLicenseCredentials {
		component, ok := expected[credential.Approval.ServiceID]
		if !ok || credential.RegistrationProtocol != catalog.RegistrationProtocolV2 || credential.CatalogVersion != set.CatalogVersion || credential.CredentialGeneration != component.CredentialGeneration || credential.ClientID == "" || credential.ClientSecret == "" {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: catalog binding mismatch")
		}
		delete(expected, credential.Approval.ServiceID)
		values := map[string]string{
			"COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_INSTANCE_ID": settings.InstanceID,
			"COMMERCIAL_LICENSE_ENVIRONMENT": settings.Environment, "COMMERCIAL_LICENSE_SERVICE_ID": component.ServiceID,
			"COMMERCIAL_LICENSE_STATE_PATH":               runtimeLicenseStatePath(settings, credential),
			"COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH": settings.PlatformPublicKeyPath,
			"COMMERCIAL_LICENSE_PLATFORM_BASE_URL":        settings.PlatformBaseURL,
			"COMMERCIAL_LICENSE_CLIENT_ID":                credential.ClientID, "COMMERCIAL_LICENSE_CLIENT_SECRET": credential.ClientSecret,
			"COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL": "2", "COMMERCIAL_LICENSE_CATALOG_VERSION": strconv.Itoa(set.CatalogVersion),
			"COMMERCIAL_LICENSE_ALLOW_HTTP": booleanEnvironmentValue(settings.AllowHTTP),
		}
		for key, value := range values {
			if !validEnvironmentKey(key) || !validEnvironmentValue(value) {
				return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: invalid runtime value")
			}
		}
		updates[filepath.Join(target.config.DeployRoot, "runtime", "license-"+component.ServiceID+".env")] = values
	}
	runtimeDir := filepath.Join(target.config.DeployRoot, "runtime")
	resolved, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil || resolved != runtimeDir {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: unsafe runtime directory")
	}
	for path := range updates {
		if info, e := os.Lstat(path); e == nil {
			if !info.Mode().IsRegular() {
				return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: unsafe credential file")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: credential file unavailable")
		}
	}
	for path, values := range updates {
		if err := writeIsolatedRuntimeLicenseEnvironment(path, values); err != nil {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: credential write failed")
		}
	}
	return nil
}

func runtimeLicenseStatePath(settings application.RuntimeLicenseSettings, credential application.RuntimeLicenseCredential) string {
	parts := []string{settings.InstanceID, settings.Environment, credential.ClientID, credential.Approval.ServiceID}
	if credential.RegistrationProtocol == catalog.RegistrationProtocolV2 {
		parts = append(parts, strconv.Itoa(credential.CatalogVersion), credential.CredentialGeneration)
	} else {
		parts = append(parts, credential.Approval.CoverageDigest, credential.Approval.ImageDigest)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return filepath.Join(settings.StateDirectory, credential.Approval.ServiceID+"-"+hex.EncodeToString(sum[:16])+".json")
}

// writeIsolatedRuntimeLicenseEnvironment 用途：原子保存私有许可凭据并继承部署目录属主。
// 参数：path 为受控凭据文件路径，values 为仅在内存交付的运行配置。
// 返回值：保存成功返回 nil；属主、写入或同步失败返回错误且不替换原文件。
func writeIsolatedRuntimeLicenseEnvironment(path string, values map[string]string) error {
	// Agent 以 root 运行，生成文件仍应归部署目录属主所有，以便完整备份读取；
	// 通过继承属主解决读取问题，不扩大 0600 权限。
	owner, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, key := range sortedEnvironmentKeys(values) {
		b.WriteString(key + "=" + encodeEnvironmentValue(values[key]) + "\n")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".license-env.*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		if stat, ok := owner.Sys().(*syscall.Stat_t); ok {
			err = file.Chown(int(stat.Uid), int(stat.Gid))
		}
	}
	if err == nil {
		_, err = file.WriteString(b.String())
	}
	if err == nil {
		err = file.Sync()
	}
	if e := file.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
