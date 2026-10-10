package infrastructure

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// writeRuntimeLicenseCredentials writes one mode-0600 Agent-owned env file for
// each approved Compose component; never put component secrets in app-wide env.
func (target *productionComposeTarget) writeRuntimeLicenseCredentials(input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
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
	if len(selected) != len(input.RuntimeLicenseCredentials) {
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
	// Validate all paths before the first write. Parent traversal/symlinks must not
	// move privileged Agent writes outside the fixed release runtime directory.
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
		if credential.ReleaseDigest != digest {
			return provisioningError("LICENSE_RUNTIME_DELIVERY_FAILED: reviewed release changed")
		}
	}
	return nil
}

// A new reviewed identity must acquire its own signed snapshot; an old pending
// migration cache must not grant compatibility to a replacement process. Secret
// rotation within the same identity preserves the durable state and clock anchor.
func runtimeLicenseStatePath(settings application.RuntimeLicenseSettings, credential application.RuntimeLicenseCredential) string {
	binding := strings.Join([]string{settings.InstanceID, settings.Environment, credential.ClientID, credential.Approval.ServiceID, credential.Approval.CoverageDigest, credential.Approval.ImageDigest}, "\x00")
	sum := sha256.Sum256([]byte(binding))
	return filepath.Join(settings.StateDirectory, credential.Approval.ServiceID+"-"+hex.EncodeToString(sum[:16])+".json")
}
func writeIsolatedRuntimeLicenseEnvironment(path string, values map[string]string) error {
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
