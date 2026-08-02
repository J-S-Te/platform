package infrastructure

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

var subsystemDirectoryCodePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

const (
	integratedContractApplicationCode = "contract_management"
	integratedCustomerApplicationCode = "customer_and_opportunity"
	integratedPortalApplicationCode   = "customer_portal"
	// This is the compatibility hash compiled into the customer authorization catalog. The
	// customer's catalog tests deliberately fail when its role mapping changes, forcing this
	// deployment contract to be updated in the same reviewed release.
	integratedCustomerRoleConfigHash = "sha256:1160c3133e8d95ee8f4f0d589960c368f431a6237a283d7b803bd21fe107ca6e"
	integratedPortalRoleConfigHash   = "sha256:ef9f349797479d02c43191000adc8eefa596fe20558381313c40a3e30ed4d446"
)

// LocalDockerSubsystemProvisionerConfig controls the trusted local Docker automation used by the
// one-click onboarding endpoint. Paths are operator configuration, never browser input.
type LocalDockerSubsystemProvisionerConfig struct {
	Enabled                 bool
	ProjectsRoot            string
	GatewayScriptPath       string
	GatewayIncludePath      string
	PlatformComposeProject  string
	PlatformFrontendService string
	PlatformDockerNetwork   string
	DockerBinary            string
	Timeout                 time.Duration
	// CatalogSync configures the post-onboarding authorization catalog sync hook. When non-empty
	// the provisioner runs the contract_management catalog sync image after the subsystem Compose
	// stack is up, so the platform's authorization catalog reflects the subsystem's role and
	// permission declarations without any in-band code change in the subsystem itself.
	CatalogSyncEnabled        bool
	CatalogSyncImage          string
	CatalogSyncMysqlContainer string
	CatalogSyncMysqlUser      string
	CatalogSyncMysqlPassword  string
	CatalogSyncMysqlDatabase  string
	CatalogSyncTargetAppCode  string
}

type subsystemCommandRunner interface {
	Run(context.Context, string, []string, string, ...string) error
}

type execSubsystemCommandRunner struct{}

func (execSubsystemCommandRunner) Run(ctx context.Context, directory string, environment []string, name string, arguments ...string) error {
	command := exec.CommandContext(ctx, name, arguments...)
	// `directory` is treated as a hint: command.Dir only accepts real directories, so the
	// caller may pass a unix socket path (e.g. /var/run/docker.sock) for documentation. Fall
	// back to "/" so exec.Command's chdir probe never reports ENOTDIR on a non-directory.
	if info, err := os.Stat(directory); err == nil && info.IsDir() {
		command.Dir = directory
	} else {
		command.Dir = string(filepath.Separator)
	}
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		// Surface a truncated excerpt of the failed command's output to stderr so operators
		// can diagnose provisioning failures. Do not return command arguments or output
		// verbatim: either may contain implementation details. The OAuth secret is never
		// supplied as an argument, but this rule keeps future changes safe.
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] %s %v failed: %v\noutput: %s\n",
			name, truncateArgs(arguments), err, truncateOutput(output))
		return err
	}
	return nil
}

func truncateArgs(args []string) []string {
	const max = 3
	if len(args) <= max {
		return args
	}
	out := make([]string, max+1)
	copy(out, args[:max])
	out[max] = "..."
	return out
}

func truncateOutput(output []byte) string {
	const limit = 2 * 1024
	if len(output) <= limit {
		return string(output)
	}
	return string(output[:limit]) + "...(truncated)"
}

// LocalDockerSubsystemProvisioner writes the generated OIDC configuration into a sibling project,
// starts its Compose stack, and updates the platform's managed nginx gateway include.
type LocalDockerSubsystemProvisioner struct {
	config LocalDockerSubsystemProvisionerConfig
	runner subsystemCommandRunner
	mutex  sync.Mutex
}

// NewLocalDockerSubsystemProvisioner constructs the local deployment automation adapter.
func NewLocalDockerSubsystemProvisioner(config LocalDockerSubsystemProvisionerConfig) (*LocalDockerSubsystemProvisioner, error) {
	return newLocalDockerSubsystemProvisioner(config, execSubsystemCommandRunner{})
}

func newLocalDockerSubsystemProvisioner(config LocalDockerSubsystemProvisionerConfig, runner subsystemCommandRunner) (*LocalDockerSubsystemProvisioner, error) {
	if runner == nil {
		return nil, errors.New("subsystem command runner is required")
	}
	config.ProjectsRoot = strings.TrimSpace(config.ProjectsRoot)
	config.GatewayScriptPath = strings.TrimSpace(config.GatewayScriptPath)
	config.GatewayIncludePath = strings.TrimSpace(config.GatewayIncludePath)
	config.PlatformComposeProject = strings.TrimSpace(config.PlatformComposeProject)
	config.PlatformFrontendService = strings.TrimSpace(config.PlatformFrontendService)
	config.PlatformDockerNetwork = strings.TrimSpace(config.PlatformDockerNetwork)
	config.DockerBinary = strings.TrimSpace(config.DockerBinary)
	if config.DockerBinary == "" {
		config.DockerBinary = "docker"
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Minute
	}
	return &LocalDockerSubsystemProvisioner{config: config, runner: runner}, nil
}

// Preflight rejects missing or unsafe local project configuration before the database aggregate and
// its one-time OAuth secret are created.
func (provisioner *LocalDockerSubsystemProvisioner) Preflight(ctx context.Context, applicationCode string) error {
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	projectDirectory, err := provisioner.projectDirectory(applicationCode)
	if err != nil {
		return err
	}
	if isIntegratedSubsystem(applicationCode) {
		if _, _, _, _, _, _, _, err = provisioner.integratedComposeConfiguration(); err != nil {
			return err
		}
	} else if _, err = locateComposeFile(projectDirectory); err != nil {
		return provisioningError("subsystem Compose file is unavailable")
	}
	if _, _, err = provisioner.subsystemEnvironmentPaths(applicationCode, projectDirectory); err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	if info, statErr := os.Stat(provisioner.config.GatewayScriptPath); statErr != nil || info.IsDir() {
		return provisioningError("portal gateway script is unavailable")
	}
	if strings.TrimSpace(provisioner.config.GatewayIncludePath) == "" {
		return provisioningError("portal gateway include path is unavailable")
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := provisioner.runner.Run(checkCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return provisioningError("Docker service is unavailable")
	}
	return nil
}

// Provision applies the generated configuration and performs the deployment. Calls are serialized
// because each run mutates shared Docker and nginx state.
func (provisioner *LocalDockerSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.applyLocked(ctx, input)
}

// Update rebuilds the running subsystem containers without touching .env.local or the portal
// gateway. The use case is "subsystem code changed, redeploy"; for BaseURL/UpstreamURL/secret
// changes the caller is expected to PATCH the environment and OAuth client first via the regular
// management endpoints, then run a separate update flow. Keeping Update side-effect free on the
// integration layer avoids the problem of the bcrypt-hashed client secret not being recoverable
// for a re-issued .env.local.
func (provisioner *LocalDockerSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.rebuildLocked(ctx, input)
}

// Teardown stops the subsystem Compose stack, removes its generated .env.local, drops the
// portal gateway include, and reloads nginx. The HTTP layer is responsible for the subsequent
// DELETE on /environments and /applications.
func (provisioner *LocalDockerSubsystemProvisioner) Teardown(ctx context.Context, applicationCode, _ /* environment */ string) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()

	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	applicationCode = strings.TrimSpace(applicationCode)
	if !subsystemDirectoryCodePattern.MatchString(applicationCode) {
		return provisioningError("subsystem project path is invalid")
	}

	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()

	// Standalone Compose stacks and their .env.local live under the subsystem project directory.
	// The integrated contract, customer and Portal APIs share the platform Compose project, so teardown
	// only stops their API service and deliberately preserves database/key material in the shared
	// runtime environment files.
	projectDirectory, projectErr := provisioner.projectDirectory(applicationCode)
	if projectErr == nil {
		if isIntegratedSubsystem(applicationCode) {
			service := "contract-api"
			if applicationCode == integratedCustomerApplicationCode {
				service = "customer-api"
			} else if applicationCode == integratedPortalApplicationCode {
				service = "portal-api"
			}
			if runErr := provisioner.runIntegratedPlatformCompose(operationCtx, "stop", service); runErr != nil {
				return provisioningError("stop subsystem containers")
			}
		} else {
			if composeFile, composeErr := locateComposeFile(projectDirectory); composeErr == nil {
				runErr := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
					"compose", "--project-directory", projectDirectory, "-f", composeFile, "down", "--remove-orphans")
				if runErr != nil {
					return provisioningError("stop subsystem containers")
				}
			}
			environmentPath := filepath.Join(projectDirectory, ".env.local")
			if removeErr := os.Remove(environmentPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return provisioningError("remove subsystem environment file")
			}
		}
	}

	if !isIntegratedSubsystem(applicationCode) {
		gatewayEnvironment := append(os.Environ(), "PORTAL_GATEWAY_NGINX_INCLUDE="+provisioner.config.GatewayIncludePath)
		if gatewayErr := provisioner.runner.Run(operationCtx, filepath.Dir(provisioner.config.GatewayScriptPath), gatewayEnvironment,
			"/bin/bash", provisioner.config.GatewayScriptPath, "remove", applicationCode); gatewayErr != nil {
			return provisioningError("remove portal gateway entry")
		}
	}

	// Best-effort nginx reload. frontendContainerID may fail if the frontend stack is not
	// running; that's fine for the caller.
	if projectDirectory != "" && !isIntegratedSubsystem(applicationCode) {
		if containerID, err := provisioner.frontendContainerID(operationCtx, projectDirectory); err == nil {
			_ = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-t")
			_ = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-s", "reload")
		}
	}
	return nil
}

// rebuildLocked rebuilds the subsystem Compose stack without modifying the gateway or the
// generated environment file. The caller must hold the mutex.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()

	projectDirectory, err := provisioner.projectDirectory(input.ApplicationCode)
	if err != nil {
		return err
	}
	environmentPath := filepath.Join(projectDirectory, ".env.local")
	var rebuildErr error
	switch input.ApplicationCode {
	case integratedContractApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedContractStack(operationCtx)
	case integratedCustomerApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedCustomerStack(operationCtx)
	case integratedPortalApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedPortalStack(operationCtx)
	default:
		composeFile, locateErr := locateComposeFile(projectDirectory)
		if locateErr != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
		rebuildErr = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
			"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build")
	}
	if rebuildErr != nil {
		return provisioningError("rebuild subsystem containers")
	}
	// Re-publish the authorization catalog after a controlled rebuild so a subsystem restart
	// that changed its own role/permission set is reflected in the platform. The sync is
	// best-effort: failures are logged but do not abort the update response.
	if err := provisioner.maybeSyncContractCatalogLocked(operationCtx, input); err != nil {
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] post-rebuild catalog sync skipped or failed: %v\n", err)
	}
	return nil
}

// applyLocked contains the shared Provision body. Caller must hold the mutex.
func (provisioner *LocalDockerSubsystemProvisioner) applyLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()

	projectDirectory, err := provisioner.projectDirectory(input.ApplicationCode)
	if err != nil {
		return err
	}
	composeFile := ""
	if !isIntegratedSubsystem(input.ApplicationCode) {
		composeFile, err = locateComposeFile(projectDirectory)
		if err != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
	}
	environmentSource, environmentPath, err := provisioner.subsystemEnvironmentPaths(input.ApplicationCode, projectDirectory)
	if err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	values := map[string]string{
		"PLATFORM_BASE_URL":         input.Issuer,
		"OIDC_ISSUER":               input.Issuer,
		"OIDC_CLIENT_ID":            input.ClientID,
		"OIDC_CLIENT_SECRET":        input.ClientSecret,
		"OIDC_REDIRECT_URI":         input.RedirectURI,
		"OIDC_SCOPES":               "openid profile",
		"OIDC_TENANT_ID":            input.TenantID,
		"APP_PUBLIC_URL":            input.PublicURL,
		"APP_PATH_PREFIX":           input.PathPrefix,
		"PLATFORM_APPLICATION_ID":   input.ApplicationID,
		"PLATFORM_APPLICATION_CODE": input.ApplicationCode,
		"PLATFORM_ENVIRONMENT_CODE": input.Environment,
		// Local onboarding publishes the catalog through the isolated provisioner after the
		// subsystem starts. Do not couple contract-api availability to this control-plane
		// operation; keep the isolated credential only for explicit reconciliation.
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "false",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
		"PLATFORM_DOCKER_NETWORK":                       provisioner.config.PlatformDockerNetwork,
	}
	if input.ApplicationCode == integratedCustomerApplicationCode {
		// customer-api is compiled into the unified local topology. OIDC still uses the public
		// issuer, while discovery/token calls are routed over the private Compose alias. The
		// compatibility hash is derived from the application's embedded catalog at startup so a
		// catalog-only release cannot drift from a platform-side hard-coded value.
		values["APP_PUBLIC_ORIGIN"] = input.Issuer
		values["DEV_AUTH_ENABLED"] = "false"
		values["OIDC_BACKCHANNEL_BASE_URL"] = "http://platform-api:8080"
		values["OIDC_POST_LOGOUT_REDIRECT_URI"] = input.PublicURL
		values["OIDC_ROLE_CONFIG_HASH"] = integratedCustomerRoleConfigHash
		values["OIDC_SESSION_COOKIE_SECURE"] = booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(input.Issuer), "https://"))
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		credentials, credentialErr := requiredPortalServiceCredentials(input)
		if credentialErr != nil {
			return credentialErr
		}
		values = map[string]string{
			"PORTAL_PUBLIC_ORIGIN":                        input.Issuer,
			"PORTAL_PATH_PREFIX":                          input.PathPrefix,
			"PORTAL_OIDC_ISSUER":                          input.Issuer,
			"PORTAL_OIDC_BACKCHANNEL_BASE_URL":            "http://platform-api:8080",
			"PORTAL_OIDC_CLIENT_ID":                       input.ClientID,
			"PORTAL_OIDC_CLIENT_SECRET":                   input.ClientSecret,
			"PORTAL_OIDC_REDIRECT_URI":                    input.RedirectURI,
			"PORTAL_OIDC_SCOPES":                          "openid profile",
			"PORTAL_OIDC_TENANT_ID":                       input.TenantID,
			"PORTAL_ROLE_CONFIG_HASH":                     integratedPortalRoleConfigHash,
			"PORTAL_SESSION_COOKIE_SECURE":                booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(input.Issuer), "https://")),
			"PORTAL_ACCOUNT_SECURITY_CENTER_URL":          strings.TrimRight(input.Issuer, "/") + "/settings/security",
			"PORTAL_MACHINE_TOKEN_ISSUER":                 "basic-platform",
			"PORTAL_MACHINE_TOKEN_AUDIENCE":               "basic-platform-application",
			"PORTAL_MACHINE_TOKEN_PUBLIC_KEY_PATH":        "/app/data/keys/jwt-ed25519-public.pem",
			"PORTAL_CRM_PROVISION_CLIENT_SUBJECT":         credentials[application.ServiceCredentialPortalMappingProvision].OAuthClient.ClientID,
			"PORTAL_CRM_DISABLE_CLIENT_SUBJECT":           credentials[application.ServiceCredentialPortalMappingDisable].OAuthClient.ClientID,
			"PORTAL_CRM_INVITE_BASE_URL":                  "http://customer-api:8090/customer-opportunity/api/v1",
			"PORTAL_CRM_INVITE_TOKEN_URL":                 "http://platform-api:8080/oauth2/token",
			"PORTAL_CRM_INVITE_CLIENT_ID":                 credentials[application.ServiceCredentialPortalInviteVerify].OAuthClient.ClientID,
			"PORTAL_CRM_INVITE_CLIENT_SECRET":             credentials[application.ServiceCredentialPortalInviteVerify].PlaintextSecret,
			"PORTAL_PLATFORM_BASE_URL":                    "http://platform-api:8080",
			"PORTAL_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "false",
			"PORTAL_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
			"PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
			"PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
		}
		runtimeSecrets, secretErr := portalRuntimeSecrets(environmentSource)
		if secretErr != nil {
			return provisioningError("prepare Portal runtime secrets")
		}
		for key, value := range runtimeSecrets {
			values[key] = value
		}
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		customerEnvironment := filepath.Join(platformRoot, "docker", ".env.customer.local")
		portalURL := strings.TrimRight(input.PublicURL, "/")
		customerValues := map[string]string{
			"PORTAL_INVITE_ENABLED":                "true",
			"PORTAL_PUBLIC_URL":                    portalURL,
			"PORTAL_PROVISION_URL":                 "http://portal-api:8091/customer-portal/internal/accounts/provision",
			"PORTAL_PROVISION_TOKEN_URL":           "http://platform-api:8080/oauth2/token",
			"PORTAL_PROVISION_CLIENT_ID":           credentials[application.ServiceCredentialPortalMappingProvision].OAuthClient.ClientID,
			"PORTAL_PROVISION_CLIENT_SECRET":       credentials[application.ServiceCredentialPortalMappingProvision].PlaintextSecret,
			"PORTAL_DISABLE_URL":                   "http://portal-api:8091/customer-portal/internal/accounts/disable",
			"PORTAL_DISABLE_CLIENT_ID":             credentials[application.ServiceCredentialPortalMappingDisable].OAuthClient.ClientID,
			"PORTAL_DISABLE_CLIENT_SECRET":         credentials[application.ServiceCredentialPortalMappingDisable].PlaintextSecret,
			"PLATFORM_EXTERNAL_IDENTITY_ENABLED":   "true",
			"PLATFORM_EXTERNAL_USER_PROVISION_URL": "http://platform-api:8080/api/v1/internal/external-users",
			"PLATFORM_APPLICATION_ROLE_ASSIGN_URL": "http://platform-api:8080/api/v1/internal/application-roles",
			"PLATFORM_APPLICATION_ROLE_REVOKE_URL": "http://platform-api:8080/api/v1/internal/application-roles/revoke",
			"PLATFORM_MANAGEMENT_TOKEN_URL":        "http://platform-api:8080/oauth2/token",
			"PLATFORM_PORTAL_APPLICATION_CODE":     integratedPortalApplicationCode,
			"PLATFORM_EXTERNAL_USER_CLIENT_ID":     credentials[application.ServiceCredentialExternalUserProvision].OAuthClient.ClientID,
			"PLATFORM_EXTERNAL_USER_CLIENT_SECRET": credentials[application.ServiceCredentialExternalUserProvision].PlaintextSecret,
			"PLATFORM_ROLE_ASSIGN_CLIENT_ID":       credentials[application.ServiceCredentialApplicationRoleAssign].OAuthClient.ClientID,
			"PLATFORM_ROLE_ASSIGN_CLIENT_SECRET":   credentials[application.ServiceCredentialApplicationRoleAssign].PlaintextSecret,
			"PLATFORM_ROLE_REVOKE_CLIENT_ID":       credentials[application.ServiceCredentialApplicationRoleRevoke].OAuthClient.ClientID,
			"PLATFORM_ROLE_REVOKE_CLIENT_SECRET":   credentials[application.ServiceCredentialApplicationRoleRevoke].PlaintextSecret,
		}
		if err := updateSubsystemEnvironment(customerEnvironment, customerEnvironment, customerValues); err != nil {
			return provisioningError("write CRM Portal integration configuration")
		}
	}
	if err := updateSubsystemEnvironment(environmentSource, environmentPath, values); err != nil {
		return provisioningError("write subsystem environment file")
	}
	if input.ApplicationCode == integratedContractApplicationCode {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		platformEnvironmentPath := filepath.Join(platformRoot, "docker", ".env.local")
		if err := updateSubsystemEnvironment(platformEnvironmentPath, platformEnvironmentPath, map[string]string{
			"CONTRACT_CATALOG_PUBLISHER_CLIENT_ID":     input.CatalogPublisherClientID,
			"CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET": input.CatalogPublisherClientSecret,
		}); err != nil {
			return provisioningError("write contract catalog publisher operations configuration")
		}
	}

	// The generated file is always .env.local, which is the documented subsystem Compose
	// convention. --env-file also makes the same values available during Compose interpolation.
	var startErr error
	switch input.ApplicationCode {
	case integratedContractApplicationCode:
		startErr = provisioner.rebuildIntegratedContractStack(operationCtx)
	case integratedCustomerApplicationCode:
		startErr = provisioner.rebuildIntegratedCustomerStack(operationCtx)
	case integratedPortalApplicationCode:
		startErr = provisioner.rebuildIntegratedPortalStack(operationCtx)
	default:
		startErr = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
			"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build")
	}
	if startErr != nil {
		return provisioningError("start subsystem containers")
	}

	// contract_management is compiled into the unified frontend and its API routes are already
	// part of nginx/default.conf. Reloading that same Nginx while it is proxying this onboarding
	// request can close the client-side upstream connection just before the successful 201 response.
	// Only independently deployed subsystems need a generated whole-site proxy entry.
	if !isIntegratedSubsystem(input.ApplicationCode) {
		gatewayEnvironment := append(os.Environ(), "PORTAL_GATEWAY_NGINX_INCLUDE="+provisioner.config.GatewayIncludePath)
		if err := provisioner.runner.Run(operationCtx, filepath.Dir(provisioner.config.GatewayScriptPath), gatewayEnvironment,
			"/bin/bash", provisioner.config.GatewayScriptPath, "add", input.ApplicationCode, input.PathPrefix, input.UpstreamURL); err != nil {
			return provisioningError("update portal gateway configuration")
		}

		containerID, err := provisioner.frontendContainerID(operationCtx, projectDirectory)
		if err != nil {
			return err
		}
		if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-t"); err != nil {
			return provisioningError("validate portal gateway configuration")
		}
		if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-s", "reload"); err != nil {
			return provisioningError("reload portal gateway")
		}
	}

	// Post-onboarding authorization catalog sync. The hook only fires for the configured target
	// application code (contract_management today) and only when the operator has supplied the
	// required image + database coordinates. The script is best-effort: failures are logged but
	// do not block the onboarding response, so a missing publisher client in the seed data
	// cannot strand a new subsystem in an unrecoverable state.
	if err := provisioner.maybeSyncContractCatalogLocked(operationCtx, input); err != nil {
		if provisioner.config.ProjectsRoot == "" {
			// logger is intentionally not available in the runner; surface via stderr fallback.
			fmt.Fprintf(os.Stderr, "[subsystem-provisioner] contract catalog sync skipped or failed: %v\n", err)
		}
	}
	return nil
}

// rebuildIntegratedContractStack keeps contract_management on the workspace's single local
// Compose topology. The unified frontend already routes to basic-platform-local/contract-api;
// starting the subsystem's standalone Compose file as well would create a second contract-api
// network alias backed by a different MySQL volume and make requests non-deterministic.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedContractStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "contract-mysql", "temporal"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "contract-migrate"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--build", "--no-deps", "contract-api")
}

// rebuildIntegratedCustomerStack publishes the application-owned authorization catalog before
// starting customer-api in OIDC mode. Catalog publication is a one-shot deployment operation;
// normal API restarts therefore do not depend on a long-lived publisher credential remaining valid.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedCustomerStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "customer-mysql"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "build", "customer-api"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "customer-migrate"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "customer-api", "./authz-catalog", "publish", "crm"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "customer-api")
}

// rebuildIntegratedPortalStack uses an isolated Portal database and process while reusing the
// customer repository's immutable backend image. The catalog is published before the Portal is
// started, then customer-api is recreated so invitation endpoints receive the generated,
// least-privilege machine credentials.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedPortalStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "portal-mysql"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "build", "portal-api"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "portal-migrate"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "portal-api", "./authz-catalog", "publish", "portal"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "portal-api"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--force-recreate", "--no-deps", "customer-api")
}

func (provisioner *LocalDockerSubsystemProvisioner) runIntegratedPlatformCompose(ctx context.Context, arguments ...string) error {
	platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, err := provisioner.integratedComposeConfiguration()
	if err != nil {
		return err
	}
	composeArguments := []string{
		"compose", "--project-name", provisioner.config.PlatformComposeProject,
		"--project-directory", platformRoot,
		"--env-file", platformEnvironment,
		"--env-file", contractEnvironment,
		"--env-file", customerEnvironment,
		"--env-file", portalEnvironment,
		"-f", composeFile,
	}
	composeArguments = append(composeArguments, arguments...)
	runnerEnvironment := append([]string{}, os.Environ()...)
	runnerEnvironment = append(runnerEnvironment,
		"BASIC_PLATFORM_RUNTIME_ENV_FILE="+platformEnvironment,
		"CONTRACT_RUNTIME_ENV_FILE="+contractEnvironment,
		"CUSTOMER_RUNTIME_ENV_FILE="+customerEnvironment,
		"PORTAL_RUNTIME_ENV_FILE="+portalEnvironment,
		"BASIC_PLATFORM_HOST_PROJECT_ROOT="+platformRoot,
		"SUBSYSTEM_HOST_PROJECTS_ROOT="+workspaceRoot,
	)
	return provisioner.runner.Run(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
}

func (provisioner *LocalDockerSubsystemProvisioner) integratedComposeConfiguration() (string, string, string, string, string, string, string, error) {
	platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
	workspaceRoot := filepath.Dir(platformRoot)
	composeFile := filepath.Join(platformRoot, "compose.local.yaml")
	platformEnvironment := filepath.Join(platformRoot, "docker", ".env.local")
	contractEnvironment := filepath.Join(workspaceRoot, integratedContractApplicationCode, ".env.local")
	customerEnvironment := filepath.Join(platformRoot, "docker", ".env.customer.local")
	portalEnvironment := filepath.Join(platformRoot, "docker", ".env.portal.local")
	for _, required := range []string{composeFile, platformEnvironment, contractEnvironment, customerEnvironment} {
		if info, err := os.Stat(required); err != nil || info.IsDir() {
			return "", "", "", "", "", "", "", provisioningError("integrated subsystem Compose configuration is unavailable")
		}
	}
	// Existing local installations predate customer_portal. Other integrated subsystems must
	// remain operable without forcing an immediate new environment file. Portal preflight checks
	// its own template separately, and Provision writes .env.portal.local before Compose is run.
	if info, err := os.Stat(portalEnvironment); err != nil || info.IsDir() {
		portalEnvironment = os.DevNull
	}
	return platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, nil
}

func (provisioner *LocalDockerSubsystemProvisioner) subsystemEnvironmentPaths(applicationCode, projectDirectory string) (string, string, error) {
	if applicationCode == integratedCustomerApplicationCode {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		destination := filepath.Join(platformRoot, "docker", ".env.customer.local")
		if info, err := os.Stat(destination); err == nil && !info.IsDir() {
			return destination, destination, nil
		}
		source := filepath.Join(platformRoot, "docker", ".env.customer.local.example")
		if info, err := os.Stat(source); err == nil && !info.IsDir() {
			return source, destination, nil
		}
		return "", "", os.ErrNotExist
	}
	if applicationCode == integratedPortalApplicationCode {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		destination := filepath.Join(platformRoot, "docker", ".env.portal.local")
		if info, err := os.Stat(destination); err == nil && !info.IsDir() {
			return destination, destination, nil
		}
		source := filepath.Join(platformRoot, "docker", ".env.portal.local.example")
		if info, err := os.Stat(source); err == nil && !info.IsDir() {
			return source, destination, nil
		}
		return "", "", os.ErrNotExist
	}
	source, err := locateEnvironmentSource(projectDirectory)
	if err != nil {
		return "", "", err
	}
	return source, filepath.Join(projectDirectory, ".env.local"), nil
}

func isIntegratedSubsystem(applicationCode string) bool {
	switch strings.TrimSpace(applicationCode) {
	case integratedContractApplicationCode, integratedCustomerApplicationCode, integratedPortalApplicationCode:
		return true
	default:
		return false
	}
}

func requiredPortalServiceCredentials(input application.SubsystemProvisioningInput) (map[string]application.SubsystemServiceCredential, error) {
	purposes := []string{
		application.ServiceCredentialExternalUserProvision,
		application.ServiceCredentialApplicationRoleAssign,
		application.ServiceCredentialApplicationRoleRevoke,
		application.ServiceCredentialPortalMappingProvision,
		application.ServiceCredentialPortalMappingDisable,
		application.ServiceCredentialPortalInviteVerify,
	}
	result := make(map[string]application.SubsystemServiceCredential, len(purposes))
	for _, purpose := range purposes {
		credential, ok := input.ServiceCredential(purpose)
		if !ok {
			return nil, provisioningError("customer Portal service credentials are incomplete")
		}
		result[purpose] = credential
	}
	return result, nil
}

func booleanEnvironmentValue(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// portalRuntimeSecrets preserves already generated local material and fills every secret on the
// first Agent-driven onboarding. The DSN is always rebuilt from the same generated database
// password, avoiding a template placeholder or a second unrelated random value in the connection
// string. Browser and machine OAuth credentials are intentionally handled by the caller instead.
func portalRuntimeSecrets(sourcePath string) (map[string]string, error) {
	values, err := readEnvironmentValues(sourcePath)
	if err != nil {
		return nil, err
	}
	databasePassword, err := existingOrRandomHex(values["PORTAL_MYSQL_PASSWORD"])
	if err != nil {
		return nil, err
	}
	rootPassword, err := existingOrRandomHex(values["PORTAL_MYSQL_ROOT_PASSWORD"])
	if err != nil {
		return nil, err
	}
	result := map[string]string{
		"PORTAL_MYSQL_PASSWORD":      databasePassword,
		"PORTAL_MYSQL_ROOT_PASSWORD": rootPassword,
		"PORTAL_MYSQL_DSN":           "portal:" + databasePassword + "@tcp(portal-mysql:3306)/customer_portal?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true",
	}
	for _, key := range []string{"PORTAL_ENCRYPTION_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_HMAC_KEY_BASE64"} {
		value := strings.TrimSpace(values[key])
		if value == "" || strings.HasPrefix(value, "REPLACE_WITH_") {
			buffer := make([]byte, 32)
			if _, err := rand.Read(buffer); err != nil {
				return nil, fmt.Errorf("generate Portal key: %w", err)
			}
			value = base64.StdEncoding.EncodeToString(buffer)
		}
		result[key] = value
	}
	return result, nil
}

func existingOrRandomHex(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" && !strings.HasPrefix(value, "REPLACE_WITH_") {
		return value, nil
	}
	return randomHex(32)
}

func readEnvironmentValues(path string) (map[string]string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !validEnvironmentKey(strings.TrimSpace(key)) {
			continue
		}
		result[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// maybeSyncContractCatalogLocked runs the platform's catalog sync helper image for the configured
// target subsystem. The helper pulls the application-owned role/permission manifest out of the
// platform's MySQL, mints a catalog-publisher access token, and PUTs the manifest back to the
// platform's /authorization-catalog endpoint using the subsystem's own service credential.
//
// The helper is launched as a one-shot `docker run --rm --network=host` from the provisioner
// (which has no internal network of its own). Failure to sync is non-fatal: the operator can
// always re-run the script out of band, and the regular handbook flow remains usable.
func (provisioner *LocalDockerSubsystemProvisioner) maybeSyncContractCatalogLocked(operationCtx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.config.CatalogSyncEnabled {
		return nil
	}
	if strings.TrimSpace(provisioner.config.CatalogSyncTargetAppCode) != "" &&
		input.ApplicationCode != provisioner.config.CatalogSyncTargetAppCode {
		return nil
	}
	if strings.TrimSpace(input.CatalogPublisherClientID) == "" || strings.TrimSpace(input.CatalogPublisherClientSecret) == "" {
		return fmt.Errorf("catalog publisher client credentials are missing for application %s", input.ApplicationCode)
	}
	if strings.TrimSpace(provisioner.config.CatalogSyncImage) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlContainer) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlUser) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlPassword) == "" {
		return fmt.Errorf("catalog sync image / MySQL coordinates are not fully configured")
	}
	arguments := []string{
		"run", "--rm", "--network=host",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		// `-e NAME` forwards the runner environment without placing secret values in
		// docker's argv (and therefore /proc/<pid>/cmdline / command audit logs).
		"-e", "PLATFORM_APPLICATION_ID",
		"-e", "PLATFORM_BASE_URL",
		"-e", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID",
		"-e", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
		"-e", "PLATFORM_MYSQL_CONTAINER",
		"-e", "PLATFORM_MYSQL_USER",
		"-e", "PLATFORM_MYSQL_PASSWORD",
		"-e", "PLATFORM_MYSQL_DATABASE",
		provisioner.config.CatalogSyncImage,
		"/usr/local/bin/sync-contract-catalog.sh",
	}
	runnerEnvironment := append(os.Environ(),
		"PLATFORM_APPLICATION_ID="+input.ApplicationID,
		"PLATFORM_BASE_URL="+input.Issuer,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID="+input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET="+input.CatalogPublisherClientSecret,
		"PLATFORM_MYSQL_CONTAINER="+provisioner.config.CatalogSyncMysqlContainer,
		"PLATFORM_MYSQL_USER="+provisioner.config.CatalogSyncMysqlUser,
		"PLATFORM_MYSQL_PASSWORD="+provisioner.config.CatalogSyncMysqlPassword,
		"PLATFORM_MYSQL_DATABASE="+provisioner.config.CatalogSyncMysqlDatabase,
	)
	return provisioner.runner.Run(operationCtx, "/var/run/docker.sock", runnerEnvironment, provisioner.config.DockerBinary, arguments...)
}

// logTeardownLeftovers removes the gateway entry and .env.local for a subsystem whose Compose
// project is already gone. Called from Teardown when the compose file lookup fails; the user
// intent is still "tear this subsystem down", so we must scrub everything that is left.
func logTeardownLeftovers(runner subsystemCommandRunner, ctx context.Context, projectDirectory, applicationCode string) {
	environmentPath := filepath.Join(projectDirectory, ".env.local")
	_ = os.Remove(environmentPath)
}

func (provisioner *LocalDockerSubsystemProvisioner) frontendContainerID(ctx context.Context, directory string) (string, error) {
	command := exec.CommandContext(ctx, provisioner.config.DockerBinary, "ps",
		"--filter", "label=com.docker.compose.project="+provisioner.config.PlatformComposeProject,
		"--filter", "label=com.docker.compose.service="+provisioner.config.PlatformFrontendService,
		"--format", "{{.ID}}")
	command.Dir = directory
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return "", provisioningError("locate portal frontend container")
	}
	identifiers := strings.Fields(string(output))
	if len(identifiers) != 1 {
		return "", provisioningError("portal frontend container is not uniquely available")
	}
	return identifiers[0], nil
}

func (provisioner *LocalDockerSubsystemProvisioner) projectDirectory(applicationCode string) (string, error) {
	applicationCode = strings.TrimSpace(applicationCode)
	if !subsystemDirectoryCodePattern.MatchString(applicationCode) || strings.TrimSpace(provisioner.config.ProjectsRoot) == "" {
		return "", provisioningError("subsystem project path is invalid")
	}
	root, err := filepath.Abs(provisioner.config.ProjectsRoot)
	if err != nil {
		return "", provisioningError("subsystem projects root is invalid")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", provisioningError("subsystem projects root is unavailable")
	}
	directoryCode := applicationCode
	if applicationCode == integratedPortalApplicationCode {
		directoryCode = integratedCustomerApplicationCode
	}
	candidate := filepath.Join(root, directoryCode)
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", provisioningError("subsystem project directory is unavailable")
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", provisioningError("subsystem project directory is outside the configured root")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", provisioningError("subsystem project directory is unavailable")
	}
	return candidate, nil
}

func locateComposeFile(projectDirectory string) (string, error) {
	for _, name := range []string{"docker-compose.yml", "compose.yaml", "compose.yml", "docker-compose.yaml"} {
		candidate := filepath.Join(projectDirectory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

func locateEnvironmentSource(projectDirectory string) (string, error) {
	for _, name := range []string{".env.local", ".env.example"} {
		candidate := filepath.Join(projectDirectory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

func updateSubsystemEnvironment(sourcePath, destinationPath string, replacements map[string]string) error {
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	for key, value := range replacements {
		if !validEnvironmentValue(value) || !validEnvironmentKey(key) {
			return errors.New("invalid environment replacement")
		}
	}

	remaining := make(map[string]string, len(replacements))
	for key, value := range replacements {
		remaining[key] = value
	}
	lines := make([]string, 0, strings.Count(string(content), "\n")+len(replacements)+1)
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := scanner.Text()
		key, ok := environmentLineKey(line)
		if replacement, exists := remaining[key]; ok && exists {
			lines = append(lines, key+"="+encodeEnvironmentValue(replacement))
			delete(remaining, key)
			continue
		}
		if ok && strings.Contains(strings.ToUpper(key), "PASSWORD") {
			_, current, found := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export ")), "=")
			if found && strings.HasPrefix(strings.Trim(strings.TrimSpace(current), "\"'"), "REPLACE_WITH_") {
				generated, generateErr := randomHex(32)
				if generateErr != nil {
					return generateErr
				}
				lines = append(lines, key+"="+generated)
				continue
			}
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, key := range sortedEnvironmentKeys(remaining) {
		lines = append(lines, key+"="+encodeEnvironmentValue(remaining[key]))
	}
	output := strings.Join(lines, "\n") + "\n"

	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destinationPath), ".env.local.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.WriteString(output)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporaryPath, destinationPath)
}

func environmentLineKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}
	key, _, found := strings.Cut(trimmed, "=")
	key = strings.TrimSpace(key)
	return key, found && validEnvironmentKey(key)
}

func validEnvironmentKey(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func validEnvironmentValue(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00")
}

func encodeEnvironmentValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return !(character == '-' || character == '_' || character == '.' || character == '/' || character == ':' || character == '@' || character == ',' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9')
	}) == -1 {
		return value
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func randomHex(byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func sortedEnvironmentKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func provisioningError(message string) error {
	return fmt.Errorf("%w: %s", application.ErrSubsystemProvisioningUnavailable, message)
}
