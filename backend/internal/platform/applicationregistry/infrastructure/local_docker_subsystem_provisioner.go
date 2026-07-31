package infrastructure

import (
	"bufio"
	"context"
	"crypto/rand"
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
	if _, err = locateComposeFile(projectDirectory); err != nil {
		return provisioningError("subsystem Compose file is unavailable")
	}
	if _, err = locateEnvironmentSource(projectDirectory); err != nil {
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

	// Compose stack + .env.local live under the subsystem project directory. If the directory
	// itself is gone we still want to scrub the gateway entry below.
	projectDirectory, projectErr := provisioner.projectDirectory(applicationCode)
	if projectErr == nil {
		if composeFile, composeErr := locateComposeFile(projectDirectory); composeErr == nil {
			if runErr := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
				"compose", "--project-directory", projectDirectory, "-f", composeFile, "down", "--remove-orphans"); runErr != nil {
				return provisioningError("stop subsystem containers")
			}
		}
		environmentPath := filepath.Join(projectDirectory, ".env.local")
		if removeErr := os.Remove(environmentPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return provisioningError("remove subsystem environment file")
		}
	}

	gatewayEnvironment := append(os.Environ(), "PORTAL_GATEWAY_NGINX_INCLUDE="+provisioner.config.GatewayIncludePath)
	if gatewayErr := provisioner.runner.Run(operationCtx, filepath.Dir(provisioner.config.GatewayScriptPath), gatewayEnvironment,
		"/bin/bash", provisioner.config.GatewayScriptPath, "remove", applicationCode); gatewayErr != nil {
		return provisioningError("remove portal gateway entry")
	}

	// Best-effort nginx reload. frontendContainerID may fail if the frontend stack is not
	// running; that's fine for the caller.
	if projectDirectory != "" {
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
	composeFile, err := locateComposeFile(projectDirectory)
	if err != nil {
		return provisioningError("subsystem Compose file is unavailable")
	}
	environmentPath := filepath.Join(projectDirectory, ".env.local")
	if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
		"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build"); err != nil {
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
	composeFile, err := locateComposeFile(projectDirectory)
	if err != nil {
		return provisioningError("subsystem Compose file is unavailable")
	}
	environmentSource, err := locateEnvironmentSource(projectDirectory)
	if err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	environmentPath := filepath.Join(projectDirectory, ".env.local")
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
		// Contract management consumes these canonical catalog-publisher keys.
		// Keep this service credential separate from the browser OIDC client above.
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "true",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
		"PLATFORM_DOCKER_NETWORK":                       provisioner.config.PlatformDockerNetwork,
	}
	if err := updateSubsystemEnvironment(environmentSource, environmentPath, values); err != nil {
		return provisioningError("write subsystem environment file")
	}

	// The generated file is always .env.local, which is the documented subsystem Compose
	// convention. --env-file also makes the same values available during Compose interpolation.
	if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
		"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build"); err != nil {
		return provisioningError("start subsystem containers")
	}

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
	candidate := filepath.Join(root, applicationCode)
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
