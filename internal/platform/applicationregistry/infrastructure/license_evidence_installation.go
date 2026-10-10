package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type installationLicenseEvidenceProvider interface {
	CollectInstallationLicenseEvidence(context.Context) (licenseevidence.Report, error)
}

func (p *UnixSocketSubsystemProvisioner) CollectInstallationLicenseEvidence(ctx context.Context) (licenseevidence.Report, error) {
	if !p.enabled {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_INSTALLATION_EVIDENCE"}) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: send failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 256*1024)).Decode(&reply) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: invalid response")
	}
	if !reply.Success || reply.LicenseEvidence == nil {
		return licenseevidence.Report{}, provisioningRejectionError(reply.Message, "")
	}
	if reply.LicenseEvidence.Scope != "INSTALLATION" {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: invalid scope")
	}
	return *reply.LicenseEvidence, nil
}
func (p *LocalDockerSubsystemProvisioner) CollectInstallationLicenseEvidence(context.Context) (licenseevidence.Report, error) {
	return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: local installation approval missing")
}

func (p *ProductionComposeSubsystemProvisioner) CollectInstallationLicenseEvidence(ctx context.Context) (licenseevidence.Report, error) {
	if !p.enabled || len(p.targets) == 0 {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: installation boundary not approved")
	}
	var root, project string
	for _, target := range p.targets {
		if root == "" {
			root = target.config.DeployRoot
			project = target.config.ComposeProject
		} else if root != target.config.DeployRoot || project != target.config.ComposeProject {
			return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: mixed installation boundaries")
		}
	}
	path, err := productionRuntimeApprovalPath(root, "license-evidence.json")
	if err != nil {
		return licenseevidence.Report{}, err
	}
	config, err := loadInstallationLicenseEvidenceApproval(path, project, p.profiles)
	if err != nil {
		return licenseevidence.Report{}, err
	}
	collector, err := licenseevidence.NewInstallationCollector(licenseevidence.ExecRunner{DockerBinary: p.dockerBinary}, config)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: invalid installation inventory")
	}
	return collector.Collect(ctx), nil
}

func loadInstallationLicenseEvidenceApproval(path, project string, profiles []productionSubsystemProfile) (licenseevidence.InstallationConfig, error) {
	fail := func() (licenseevidence.InstallationConfig, error) {
		return licenseevidence.InstallationConfig{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: complete installation approval required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return fail()
	}
	f, err := os.Open(path)
	if err != nil {
		return fail()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 256*1024 {
		return fail()
	}
	b, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(b) > 256*1024 {
		return fail()
	}
	var a licenseEvidenceApproval
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil || d.Decode(new(any)) != io.EOF || a.Version != 1 || a.Project != project || !a.InstallationBoundary {
		return fail()
	}
	// An excluded unit must be named as infrastructure/init in the reviewed
	// deployment profile. Merely claiming to be infrastructure is insufficient.
	nonbusiness := reviewedPlatformLicenseEvidenceInfrastructure()
	business := map[string]bool{}
	known := map[string]productionSubsystemProfile{}
	for _, p := range profiles {
		known[p.Manifest.Application.Code+"/"+p.Manifest.Application.Environment] = p
		for _, s := range p.Manifest.Compose.RuntimeServices {
			business[s] = true
		}
		for s := range p.Manifest.Compose.ConditionalRuntimeServices {
			business[s] = true
		}
		for _, s := range p.Manifest.Compose.DependencyServices {
			nonbusiness[s] = true
		}
		for _, s := range p.Manifest.Compose.InitializationServices {
			nonbusiness[s] = true
		}
		if s := p.Manifest.Compose.MigrateService; s != "" {
			nonbusiness[s] = true
		}
		if s := p.Manifest.Compose.CatalogSyncService; s != "" {
			nonbusiness[s] = true
		}
	}
	for _, s := range a.ExcludedServices {
		if !nonbusiness[s] || business[s] {
			return fail()
		}
	}
	checked := map[string]bool{}
	for _, s := range a.Services {
		key := s.Application + "/" + s.Environment
		p, ok := known[key]
		if !ok {
			return fail()
		}
		if !checked[key] {
			if _, err := loadLicenseEvidenceApproval(path, project, p); err != nil {
				return fail()
			}
			checked[key] = true
		}
	}
	for _, item := range a.Infrastructure {
		key := item.Application + "/" + item.Environment
		p, ok := known[key]
		if !ok || !business[item.Name] || !licenseevidence.ReviewedControlledBIInfrastructure(item.Application, item.Name) {
			return fail()
		}
		if !checked[key] {
			if _, err := loadLicenseEvidenceApproval(path, project, p); err != nil {
				return fail()
			}
			checked[key] = true
		}
	}
	config := licenseevidence.InstallationConfig{Project: project, Services: a.Services, Infrastructure: a.Infrastructure, ExcludedServices: a.ExcludedServices, Timeout: 30 * time.Second}
	if _, err := licenseevidence.NewInstallationCollector(licenseevidence.ExecRunner{}, config); err != nil {
		return fail()
	}
	return config, nil
}

// Reviewed against deploy/production/docker-compose.yml: platform control/identity,
// shared file infrastructure and the static gateway are outside business tiers.
// This is an allowlist for explicit operator approval, not an automatic exclusion
// and not a label-derived exemption. Business profile runtime membership wins.
func reviewedPlatformLicenseEvidenceInfrastructure() map[string]bool {
	return map[string]bool{
		"keycloak-db": true, "keycloak": true,
		"platform-mysql": true, "platform-migrate": true,
		"platform-api": true, "platform-worker": true,
		"docker-socket-proxy": true, "subsystem-provisioner": true,
		"platform-key-init": true, "subsystem-provisioner-socket-init": true,
		"frontend": true, "file-gateway-mysql": true, "file-gateway": true,
	}
}
