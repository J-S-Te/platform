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

// writeRuntimeLicenseCredentials 为每个已批准的 Compose 组件写一个 mode-0600、Agent 持有的
// env 文件；组件秘密绝不进入应用级共享 env。
func (target *productionComposeTarget) writeRuntimeLicenseCredentials(input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
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
		if err := target.writeLegacyRuntimeLicenseCredentials(legacyInput, true); err != nil {
			return err
		}
		return target.writeBuiltinRuntimeLicenseCredentials(builtinInput, true)
	}
	if len(builtin) > 0 {
		return target.writeBuiltinRuntimeLicenseCredentials(input, false)
	}
	return target.writeLegacyRuntimeLicenseCredentials(input, false)
}

func (target *productionComposeTarget) writeLegacyRuntimeLicenseCredentials(input application.SubsystemProvisioningInput, allowSubset bool) error {
	path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "runtime-license-"+input.ApplicationCode+"-"+input.Environment+".json")
	if err != nil {
		return err
	}
	release, err := loadRuntimeLicenseReleaseDocument(path, target.config.Profile)
	if err != nil {
		return err
	}
	if err = target.verifyRuntimeLicenseRelease(input, release); err != nil {
		return err
	}
	approved := release.Components
	selected := map[string]bool{}
	for _, id := range target.selectedRuntimeLicenseConsumers() {
		selected[id] = true
	}
	if !allowSubset && len(selected) != len(input.RuntimeLicenseCredentials) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: component set mismatch")
	}
	expected := map[string]application.RuntimeLicenseApproval{}
	for _, item := range approved {
		if selected[item.ServiceID] {
			expected[item.ServiceID] = item
		}
	}
	settings := input.RuntimeLicenseSettings
	if settings.InstanceID == "" || settings.Environment == "" || settings.PlatformBaseURL == "" || !filepath.IsAbs(settings.PlatformPublicKeyPath) || !filepath.IsAbs(settings.StateDirectory) {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: runtime binding incomplete")
	}
	updates := map[string]map[string]string{}
	for _, credential := range input.RuntimeLicenseCredentials {
		item, ok := expected[credential.Approval.ServiceID]
		if !ok || item != credential.Approval || credential.ClientID == "" || credential.ClientSecret == "" {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: approved binding mismatch")
		}
		delete(expected, item.ServiceID)
		values := map[string]string{"COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_INSTANCE_ID": settings.InstanceID, "COMMERCIAL_LICENSE_ENVIRONMENT": settings.Environment, "COMMERCIAL_LICENSE_SERVICE_ID": item.ServiceID, "COMMERCIAL_LICENSE_STATE_PATH": runtimeLicenseStatePath(settings, credential), "COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH": settings.PlatformPublicKeyPath, "COMMERCIAL_LICENSE_PLATFORM_BASE_URL": settings.PlatformBaseURL, "COMMERCIAL_LICENSE_CLIENT_ID": credential.ClientID, "COMMERCIAL_LICENSE_CLIENT_SECRET": credential.ClientSecret, "COMMERCIAL_LICENSE_COVERAGE_DIGEST": item.CoverageDigest, "COMMERCIAL_LICENSE_IMAGE_DIGEST": item.ImageDigest, "COMMERCIAL_LICENSE_ALLOW_HTTP": booleanEnvironmentValue(settings.AllowHTTP)}
		for key, value := range values {
			if !validEnvironmentKey(key) || !validEnvironmentValue(value) {
				return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: invalid runtime value")
			}
		}
		updates[filepath.Join(target.config.DeployRoot, "runtime", "license-"+item.ServiceID+".env")] = values
	}
	// 在首次写入前校验全部路径。父目录穿越/符号链接不能把特权 Agent 写入移出固定的
	// 发布运行时目录。
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

func (target *productionComposeTarget) runtimeLicenseReleaseDigest(release runtimeLicenseRelease) (string, error) {
	selected := map[string]bool{}
	for _, id := range target.selectedRuntimeLicenseConsumers() {
		selected[id] = true
	}
	approval := application.RuntimeLicenseReleaseApproval{ReleaseGeneration: release.ReleaseGeneration, RetireServiceIDs: release.RetireServiceIDs}
	for _, item := range release.Components {
		if selected[item.ServiceID] {
			approval.Components = append(approval.Components, item)
			approval.RequiredServiceIDs = append(approval.RequiredServiceIDs, item.ServiceID)
		}
	}
	return application.RuntimeLicenseReleaseDigest(approval)
}

func (target *productionComposeTarget) verifyRuntimeLicenseRelease(input application.SubsystemProvisioningInput, release runtimeLicenseRelease) error {
	digest, err := target.runtimeLicenseReleaseDigest(release)
	if err != nil {
		return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: approval invalid")
	}
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.RegistrationProtocol == catalog.RegistrationProtocolV2 {
			continue
		}
		if credential.ReleaseDigest != digest {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: reviewed release changed")
		}
	}
	return nil
}

// 新的受审身份必须获取自己的签名快照；旧的待迁移缓存不得授予替代进程兼容性。
// 同一身份内的秘密轮换保留持久状态与时钟锚点。
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
