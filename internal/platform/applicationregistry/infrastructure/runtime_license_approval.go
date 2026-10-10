package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

var runtimeLicenseServiceIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type runtimeLicenseLifecycleProvider interface {
	ApprovedRuntimeLicenseLifecycle(context.Context, string, string) (application.RuntimeLicenseReleaseApproval, error)
}

func (p *ProductionComposeSubsystemProvisioner) ApprovedRuntimeLicenseLifecycle(ctx context.Context, app, env string) (application.RuntimeLicenseReleaseApproval, error) {
	items, err := p.ApprovedRuntimeLicenseComponents(ctx, app, env)
	if err != nil {
		return application.RuntimeLicenseReleaseApproval{}, err
	}
	for _, target := range p.targets {
		if target.config.Profile.Manifest.Application.Code != app || target.config.Profile.Manifest.Application.Environment != env {
			continue
		}
		path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "runtime-license-"+app+"-"+env+".json")
		if err != nil {
			return application.RuntimeLicenseReleaseApproval{}, err
		}
		release, err := loadRuntimeLicenseReleaseDocument(path, target.config.Profile)
		if err != nil {
			return application.RuntimeLicenseReleaseApproval{}, err
		}
		// Selection and metadata must come from the same immutable reviewed release.
		byID := map[string]application.RuntimeLicenseApproval{}
		for _, item := range release.Components {
			byID[item.ServiceID] = item
		}
		out := application.RuntimeLicenseReleaseApproval{Components: items, RetireServiceIDs: release.RetireServiceIDs, ReleaseGeneration: release.ReleaseGeneration}
		for _, item := range items {
			if byID[item.ServiceID] != item {
				return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: release changed while reading")
			}
			out.RequiredServiceIDs = append(out.RequiredServiceIDs, item.ServiceID)
		}
		return out, nil
	}
	return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: target not approved")
}

func (p *UnixSocketSubsystemProvisioner) ApprovedRuntimeLicenseLifecycle(ctx context.Context, app, env string) (application.RuntimeLicenseReleaseApproval, error) {
	if !p.enabled {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return application.RuntimeLicenseReleaseApproval{}, err
	}
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_LIFECYCLE_APPROVAL", Code: app, Environment: env}) != nil {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: request failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&reply) != nil || !reply.Success || reply.RuntimeLicenseLifecycle == nil || application.ValidateRuntimeLicenseApprovals(reply.RuntimeLicenseLifecycle.Components) != nil {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: response invalid")
	}
	return *reply.RuntimeLicenseLifecycle, nil
}

type runtimeLicenseApprovalProvider interface {
	ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]application.RuntimeLicenseApproval, error)
}

func (p *LocalDockerSubsystemProvisioner) ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]application.RuntimeLicenseApproval, error) {
	return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: local deployment has no reviewed immutable release")
}

type runtimeLicenseRelease struct {
	Version           int                                  `json:"version"`
	Application       string                               `json:"application"`
	Environment       string                               `json:"environment"`
	Components        []application.RuntimeLicenseApproval `json:"components"`
	RetireServiceIDs  []string                             `json:"retire_service_ids,omitempty"`
	ReleaseGeneration string                               `json:"release_generation,omitempty"`
}

func (p *ProductionComposeSubsystemProvisioner) ApprovedRuntimeLicenseComponents(ctx context.Context, app, env string) ([]application.RuntimeLicenseApproval, error) {
	if !p.enabled {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: deployment agent disabled")
	}
	for _, target := range p.targets {
		if target.config.Profile.Manifest.Application.Code == app && target.config.Profile.Manifest.Application.Environment == env {
			// Read-only approval must not mutate a concurrent deployment's selected
			// service cache. Its selection is recomputed again under the deploy lock.
			target = &productionComposeTarget{config: target.config, runner: target.runner}
			if err := target.validateEnabledServices(ctx); err != nil {
				return nil, err
			}
			path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "runtime-license-"+app+"-"+env+".json")
			if err != nil {
				return nil, err
			}
			items, err := loadRuntimeLicenseRelease(path, target.config.Profile)
			if err != nil {
				return nil, err
			}
			selected := map[string]bool{}
			for _, id := range target.selectedRuntimeLicenseConsumers() {
				selected[id] = true
			}
			result := []application.RuntimeLicenseApproval{}
			for _, item := range items {
				if selected[item.ServiceID] {
					result = append(result, item)
				}
			}
			if application.ValidateRuntimeLicenseApprovals(result) != nil {
				return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: no selected components")
			}
			return result, nil
		}
	}
	return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: target not approved")
}
func loadRuntimeLicenseRelease(path string, profile productionSubsystemProfile) ([]application.RuntimeLicenseApproval, error) {
	release, err := loadRuntimeLicenseReleaseDocument(path, profile)
	return release.Components, err
}
func loadRuntimeLicenseReleaseDocument(path string, profile productionSubsystemProfile) (runtimeLicenseRelease, error) {
	failure := func() (runtimeLicenseRelease, error) {
		return runtimeLicenseRelease{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: reviewed component inventory required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return failure()
	}
	f, err := os.Open(path)
	if err != nil {
		return failure()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 65536 {
		return failure()
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return failure()
	}
	var release runtimeLicenseRelease
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&release) != nil || d.Decode(new(any)) != io.EOF || release.Version != 1 || release.Application != profile.Manifest.Application.Code || release.Environment != profile.Manifest.Application.Environment || application.ValidateRuntimeLicenseApprovals(release.Components) != nil {
		return failure()
	}
	// Conditional units are required to belong to runtime_services by profile
	// validation. Approval is a set union, never duplicate component IDs.
	expectedSet := map[string]bool{}
	consumers, err := runtimeLicenseConsumerServices(profile)
	if err != nil {
		return failure()
	}
	for _, service := range consumers {
		expectedSet[service] = true
	}
	for service := range profile.Manifest.Compose.ConditionalRuntimeServices {
		expectedSet[service] = true
	}
	expected := make([]string, 0, len(expectedSet))
	for service := range expectedSet {
		expected = append(expected, service)
	}
	actual := make([]string, 0, len(release.Components))
	for _, c := range release.Components {
		actual = append(actual, c.ServiceID)
	}
	sort.Strings(expected)
	sort.Strings(actual)
	if len(expected) != len(actual) {
		return failure()
	}
	for i := range expected {
		if expected[i] != actual[i] {
			return failure()
		}
	}
	retired := map[string]bool{}
	if release.ReleaseGeneration != "" && !runtimeLicenseServiceIdentifier.MatchString(release.ReleaseGeneration) {
		return failure()
	}
	if len(release.RetireServiceIDs) > 32 {
		return failure()
	}
	for _, id := range release.RetireServiceIDs {
		_, conditional := profile.Manifest.Compose.ConditionalRuntimeServices[id]
		prefix := runtimeLicenseRetirementPrefix(release.Application)
		if prefix == "" || !strings.HasPrefix(id, prefix) || !runtimeLicenseServiceIdentifier.MatchString(id) || (expectedSet[id] && !conditional) || retired[id] || licenseevidence.ReviewedControlledBIInfrastructure(release.Application, id) {
			return failure()
		}
		retired[id] = true
	}
	return release, nil
}
func (p *UnixSocketSubsystemProvisioner) ApprovedRuntimeLicenseComponents(ctx context.Context, app, env string) ([]application.RuntimeLicenseApproval, error) {
	if !p.enabled {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_APPROVAL", Code: app, Environment: env}) != nil {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: request failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&reply) != nil || !reply.Success || application.ValidateRuntimeLicenseApprovals(reply.RuntimeLicenseApproval) != nil {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: response invalid")
	}
	return reply.RuntimeLicenseApproval, nil
}
