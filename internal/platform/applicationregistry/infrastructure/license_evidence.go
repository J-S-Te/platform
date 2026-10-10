package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type licenseEvidenceProvider interface {
	CollectLicenseEvidence(context.Context, string, string) (licenseevidence.Report, error)
}

// CollectLicenseEvidence is a fixed read-only Agent operation, not a discovery
// fallback. Only selectors are sent; approved Docker/image metadata stays local.
func (p *UnixSocketSubsystemProvisioner) CollectLicenseEvidence(ctx context.Context, app, environment string) (licenseevidence.Report, error) {
	if !p.enabled {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_EVIDENCE", Code: app, Environment: environment}) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: send failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 256*1024)).Decode(&reply) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: response invalid")
	}
	if !reply.Success || reply.LicenseEvidence == nil {
		return licenseevidence.Report{}, provisioningRejectionError(reply.Message, "")
	}
	return *reply.LicenseEvidence, nil
}

func (p *LocalDockerSubsystemProvisioner) CollectLicenseEvidence(context.Context, string, string) (licenseevidence.Report, error) {
	return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: local deployments have no approved immutable release inventory")
}

type licenseEvidenceApproval struct {
	Infrastructure       []licenseevidence.Infrastructure `json:"infrastructure,omitempty"`
	InstallationBoundary bool                             `json:"installation_boundary"`
	ExcludedServices     []string                         `json:"excluded_services"`
	Version              int                              `json:"version"`
	Project              string                           `json:"project"`
	Services             []licenseevidence.Service        `json:"services"`
}

func (p *ProductionComposeSubsystemProvisioner) CollectLicenseEvidence(ctx context.Context, app, environment string) (licenseevidence.Report, error) {
	if !p.enabled {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: agent disabled")
	}
	var target *productionComposeTarget
	for _, t := range p.targets {
		if t.config.Profile.Manifest.Application.Code == app && t.config.Profile.Manifest.Application.Environment == environment {
			target = t
			break
		}
	}
	if target == nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: target not approved")
	}
	path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "license-evidence.json")
	if err != nil {
		return licenseevidence.Report{}, err
	}
	config, err := loadLicenseEvidenceApproval(path, target.config.ComposeProject, target.config.Profile)
	if err != nil {
		return licenseevidence.Report{}, err
	}
	collector, err := licenseevidence.NewCollector(licenseevidence.ExecRunner{DockerBinary: p.dockerBinary}, config)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: release evidence approval invalid")
	}
	return collector.Collect(ctx), nil
}

func loadLicenseEvidenceApproval(path, project string, profile productionSubsystemProfile) (licenseevidence.Config, error) {
	unsupported := func() (licenseevidence.Config, error) {
		return licenseevidence.Config{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: approved digest version protocol inventory required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return unsupported()
	}
	f, err := os.Open(path)
	if err != nil {
		return unsupported()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 256*1024 {
		return unsupported()
	}
	b, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(b) > 256*1024 {
		return unsupported()
	}
	var approval licenseEvidenceApproval
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&approval) != nil || decoder.Decode(new(any)) != io.EOF || approval.Version != 1 || approval.Project != project {
		return unsupported()
	}
	// Conditional units are required to belong to runtime_services by profile
	// validation. Approval is a set union, never duplicate component IDs.
	expectedSet := map[string]bool{}
	consumers, err := runtimeLicenseConsumerServices(profile)
	if err != nil {
		return unsupported()
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
	sort.Strings(expected)
	services := []licenseevidence.Service{}
	for _, s := range approval.Services {
		if s.Application == profile.Manifest.Application.Code && s.Environment == profile.Manifest.Application.Environment {
			services = append(services, s)
		}
	}
	actual := make([]string, len(services))
	for i, s := range services {
		actual[i] = s.Name
	}
	sort.Strings(actual)
	if len(actual) != len(expected) || len(actual) == 0 {
		return unsupported()
	}
	for i := range actual {
		if actual[i] != expected[i] {
			return unsupported()
		}
	}
	infra := []licenseevidence.Infrastructure{}
	for _, item := range approval.Infrastructure {
		if item.Application == profile.Manifest.Application.Code && item.Environment == profile.Manifest.Application.Environment {
			infra = append(infra, item)
		}
	}
	expectedInfra := 0
	for _, name := range profile.Manifest.Compose.RuntimeServices {
		if licenseevidence.ReviewedControlledBIInfrastructure(profile.Manifest.Application.Code, name) {
			expectedInfra++
		}
	}
	if len(infra) != expectedInfra {
		return unsupported()
	}
	for _, item := range infra {
		if !licenseevidence.ReviewedControlledBIInfrastructure(item.Application, item.Name) {
			return unsupported()
		}
	}
	config := licenseevidence.Config{Project: project, Services: services, Infrastructure: infra, Timeout: 30 * time.Second}
	if _, err := licenseevidence.NewCollector(licenseevidence.ExecRunner{}, config); err != nil {
		return unsupported()
	}
	return config, nil
}

var _ licenseEvidenceProvider = (*UnixSocketSubsystemProvisioner)(nil)
