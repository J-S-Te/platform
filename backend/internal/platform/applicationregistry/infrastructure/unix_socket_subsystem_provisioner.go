package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	settingsapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/application"
)

const subsystemProvisioningProtocolVersion = 1

type subsystemProvisioningRequest struct {
	Version     int                                     `json:"version"`
	Action      string                                  `json:"action"`
	Code        string                                  `json:"code,omitempty"`
	TenantID    string                                  `json:"tenant_id,omitempty"`
	Environment string                                  `json:"environment,omitempty"`
	Preflight   *application.SubsystemPreflightInput    `json:"preflight,omitempty"`
	Input       *application.SubsystemProvisioningInput `json:"input,omitempty"`
	Access      *subsystemAccessApplyPayload            `json:"access,omitempty"`
}

// subsystemAccessApplyPayload carries the public-access configuration for the apply-access action.
type subsystemAccessApplyPayload struct {
	PublicOrigin              string `json:"public_origin"`
	AllowInsecureHTTPRedirect bool   `json:"allow_insecure_http_redirect"`
}

type subsystemProvisioningReply struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// UnixSocketSubsystemProvisioner is the unprivileged API-side client. It can request only the two
// fixed operations supported by the isolated deployment helper and never receives command output.
type UnixSocketSubsystemProvisioner struct {
<<<<<<< Updated upstream
	enabled    bool
	socketPath string
	timeout    time.Duration
	mode       string
}

// NewUnixSocketSubsystemProvisioner constructs the API-side automatic deployment client.
func NewUnixSocketSubsystemProvisioner(enabled bool, socketPath string, timeout time.Duration, modes ...string) (*UnixSocketSubsystemProvisioner, error) {
=======
	enabled      bool
	socketPath   string
	timeout      time.Duration
	capabilities application.SubsystemProvisioningCapabilities
}

// NewUnixSocketSubsystemProvisioner constructs the API-side automatic deployment client.
func NewUnixSocketSubsystemProvisioner(enabled bool, socketPath string, timeout time.Duration, policies ...application.SubsystemProvisioningCapabilities) (*UnixSocketSubsystemProvisioner, error) {
>>>>>>> Stashed changes
	socketPath = strings.TrimSpace(socketPath)
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
<<<<<<< Updated upstream
	mode := "local"
	if len(modes) > 1 {
		return nil, errors.New("subsystem provisioning accepts at most one deployment mode")
	}
	if len(modes) == 1 && strings.TrimSpace(modes[0]) != "" {
		mode = strings.ToLower(strings.TrimSpace(modes[0]))
	}
	if mode != "local" && mode != "production" {
=======
	if len(policies) > 1 {
		return nil, errors.New("subsystem provisioning accepts at most one capability policy")
	}
	capabilities := application.SubsystemProvisioningCapabilities{
		Enabled: enabled, Mode: "local",
		SupportedEnvironments: []string{"dev", "test", "staging", "prod"},
	}
	if len(policies) == 1 {
		capabilities = normalizeSubsystemProvisioningCapabilities(policies[0])
		capabilities.Enabled = enabled
	}
	if capabilities.Mode != "local" && capabilities.Mode != "production" {
>>>>>>> Stashed changes
		return nil, errors.New("subsystem provisioning mode must be local or production")
	}
	if enabled && socketPath == "" {
		return nil, errors.New("subsystem provisioning socket path is required")
	}
<<<<<<< Updated upstream
	return &UnixSocketSubsystemProvisioner{enabled: enabled, socketPath: socketPath, timeout: timeout, mode: mode}, nil
}

// Capabilities returns the API-side deployment policy used by the management console. The
// privileged Agent enforces the same restrictions again; this method is only a safe UI hint and
// cannot expand the executor's fixed command or filesystem boundary.
func (provisioner *UnixSocketSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	capabilities := application.SubsystemProvisioningCapabilities{
		Enabled:               provisioner.enabled,
		Mode:                  provisioner.mode,
		SupportedEnvironments: []string{"dev", "test", "staging", "prod"},
	}
	if provisioner.mode == "production" {
		capabilities.SupportedApplicationCodes = []string{"contract_management"}
		capabilities.SupportedEnvironments = []string{"prod"}
	}
	return capabilities
=======
	return &UnixSocketSubsystemProvisioner{enabled: enabled, socketPath: socketPath, timeout: timeout, capabilities: capabilities}, nil
}

// Capabilities returns a defensive copy of the server-configured deployment policy. The
// privileged Agent independently enforces the same values and never trusts this UI projection.
func (provisioner *UnixSocketSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	capabilities := provisioner.capabilities
	capabilities.SupportedApplicationCodes = append([]string(nil), capabilities.SupportedApplicationCodes...)
	capabilities.SupportedEnvironments = append([]string(nil), capabilities.SupportedEnvironments...)
	return capabilities
}

func normalizeSubsystemProvisioningCapabilities(capabilities application.SubsystemProvisioningCapabilities) application.SubsystemProvisioningCapabilities {
	capabilities.Mode = strings.ToLower(strings.TrimSpace(capabilities.Mode))
	if capabilities.Mode == "" {
		capabilities.Mode = "local"
	}
	capabilities.SupportedApplicationCodes = normalizedCapabilityValues(capabilities.SupportedApplicationCodes)
	capabilities.SupportedEnvironments = normalizedCapabilityValues(capabilities.SupportedEnvironments)
	capabilities.DefaultApplicationCode = strings.ToLower(strings.TrimSpace(capabilities.DefaultApplicationCode))
	capabilities.DefaultApplicationName = strings.TrimSpace(capabilities.DefaultApplicationName)
	capabilities.DefaultDescription = strings.TrimSpace(capabilities.DefaultDescription)
	capabilities.DefaultEnvironment = strings.ToLower(strings.TrimSpace(capabilities.DefaultEnvironment))
	capabilities.DefaultUpstreamURL = strings.TrimRight(strings.TrimSpace(capabilities.DefaultUpstreamURL), "/")
	capabilities.DefaultPathPrefix = strings.TrimRight(strings.TrimSpace(capabilities.DefaultPathPrefix), "/")
	capabilities.DefaultClientType = strings.ToLower(strings.TrimSpace(capabilities.DefaultClientType))
	return capabilities
}

func normalizedCapabilityValues(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
>>>>>>> Stashed changes
}

func (provisioner *UnixSocketSubsystemProvisioner) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version:   subsystemProvisioningProtocolVersion,
		Action:    "preflight",
		Code:      strings.TrimSpace(input.ApplicationCode),
		Preflight: &input,
	})
}

func (provisioner *UnixSocketSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion,
		Action:  "provision",
		Input:   &input,
	})
}

func (provisioner *UnixSocketSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion,
		Action:  "update",
		Input:   &input,
	})
}

// ApplyAccess asks the deployment helper to rewrite the local override environment files and
// recreate the affected containers so the unified frontend and subsystem callbacks use the
// configured public origin (empty origin restores local-only access).
func (provisioner *UnixSocketSubsystemProvisioner) ApplyAccess(ctx context.Context, input settingsapplication.AccessApplyInput) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion,
		Action:  "apply-access",
		Access: &subsystemAccessApplyPayload{
			PublicOrigin:              input.PublicOrigin,
			AllowInsecureHTTPRedirect: input.AllowInsecureHTTPRedirect,
		},
	})
}

func (provisioner *UnixSocketSubsystemProvisioner) Teardown(ctx context.Context, tenantID, applicationCode, environment string) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version:     subsystemProvisioningProtocolVersion,
		Action:      "teardown",
		TenantID:    strings.TrimSpace(tenantID),
		Code:        strings.TrimSpace(applicationCode),
		Environment: strings.TrimSpace(environment),
	})
}

func (provisioner *UnixSocketSubsystemProvisioner) exchange(ctx context.Context, request subsystemProvisioningRequest) error {
	// API 容器不持有 Docker socket，只能通过受限 Unix 协议请求固定动作。超时同时作用于
	// 连接、发送和响应，防止部署 Agent 卡住后耗尽 API 请求协程。
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.timeout)
	defer cancel()
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(operationCtx, "unix", provisioner.socketPath)
	if err != nil {
		return provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return provisioningError("send deployment request")
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 64*1024)).Decode(&reply); err != nil {
		return provisioningError("read deployment response")
	}
	if !reply.Success {
		message := strings.TrimSpace(reply.Message)
		if message == "" {
			message = "deployment helper rejected the request"
		}
		return provisioningError(message)
	}
	return nil
}

// RunSubsystemProvisioningServer serves the narrow Unix-socket deployment protocol. The listener
// is intended for an isolated helper container with no published network ports.
func RunSubsystemProvisioningServer(ctx context.Context, socketPath string, executor application.SubsystemProvisioner) error {
	if executor == nil || strings.TrimSpace(socketPath) == "" {
		return errors.New("subsystem provisioning server dependencies are required")
	}
	socketPath = filepath.Clean(strings.TrimSpace(socketPath))
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("create provisioning socket directory: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale provisioning socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on provisioning socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0o660); err != nil {
		return fmt.Errorf("set provisioning socket permissions: %w", err)
	}
	// 每条连接独立处理，长时间 Docker 构建不会阻塞新的状态变更请求；真正会修改共享
	// 运行时的动作仍由 executor 内部互斥锁串行化。
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept provisioning request: %w", acceptErr)
		}
		go handleSubsystemProvisioningConnection(ctx, connection, executor)
	}
}

func handleSubsystemProvisioningConnection(ctx context.Context, connection net.Conn, executor application.SubsystemProvisioner) {
	defer connection.Close()
	var request subsystemProvisioningRequest
	if err := json.NewDecoder(io.LimitReader(connection, 1024*1024)).Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: false})
		return
	}
	if request.Version != subsystemProvisioningProtocolVersion {
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: false, Message: "deployment protocol version is unsupported"})
		return
	}
	// action 是显式白名单，网络对端不能传入任意命令、路径或参数。新增动作必须同时升级
	// 协议结构和分派逻辑，不能退化成通用 shell 执行接口。
	var err error
	switch request.Action {
	case "preflight":
		if request.Preflight == nil {
			err = application.ErrSubsystemProvisioningUnavailable
		} else {
			err = executor.Preflight(ctx, *request.Preflight)
		}
	case "provision":
		if request.Input == nil {
			err = application.ErrSubsystemProvisioningUnavailable
		} else {
			err = executor.Provision(ctx, *request.Input)
		}
	case "update":
		if request.Input == nil {
			err = application.ErrSubsystemProvisioningUnavailable
		} else {
			err = executor.Update(ctx, *request.Input)
		}
	case "teardown":
		err = executor.Teardown(ctx, request.TenantID, request.Code, request.Environment)
	case "apply-access":
		if request.Access == nil {
			err = application.ErrSubsystemProvisioningUnavailable
			break
		}
		applier, ok := executor.(interface {
			ApplyAccess(context.Context, settingsapplication.AccessApplyInput) error
		})
		if !ok {
			err = application.ErrSubsystemProvisioningUnavailable
			break
		}
		err = applier.ApplyAccess(ctx, settingsapplication.AccessApplyInput{
			PublicOrigin:              request.Access.PublicOrigin,
			AllowInsecureHTTPRedirect: request.Access.AllowInsecureHTTPRedirect,
		})
	default:
		err = application.ErrSubsystemProvisioningUnavailable
	}
	reply := subsystemProvisioningReply{Success: err == nil}
	if err != nil {
		reply.Message = safeSubsystemProvisioningMessage(err)
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] action=%s code=%s failed: %s\n", request.Action, requestCode(request), reply.Message)
	}
	_ = json.NewEncoder(connection).Encode(reply)
}

func safeSubsystemProvisioningMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	message = strings.TrimSpace(strings.TrimPrefix(message, application.ErrSubsystemProvisioningUnavailable.Error()+":"))
	if message == "" || len(message) > 256 || strings.ContainsAny(message, "\r\n\x00") {
		return "deployment helper rejected the request"
	}
	return message
}

func requestCode(request subsystemProvisioningRequest) string {
	if request.Input != nil {
		return strings.TrimSpace(request.Input.ApplicationCode)
	}
	return strings.TrimSpace(request.Code)
}
