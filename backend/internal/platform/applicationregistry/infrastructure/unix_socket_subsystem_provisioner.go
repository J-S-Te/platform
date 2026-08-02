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
)

const subsystemProvisioningProtocolVersion = 1

type subsystemProvisioningRequest struct {
	Version     int                                     `json:"version"`
	Action      string                                  `json:"action"`
	Code        string                                  `json:"code,omitempty"`
	Environment string                                  `json:"environment,omitempty"`
	Input       *application.SubsystemProvisioningInput `json:"input,omitempty"`
}

type subsystemProvisioningReply struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// UnixSocketSubsystemProvisioner is the unprivileged API-side client. It can request only the two
// fixed operations supported by the isolated deployment helper and never receives command output.
type UnixSocketSubsystemProvisioner struct {
	enabled    bool
	socketPath string
	timeout    time.Duration
}

// NewUnixSocketSubsystemProvisioner constructs the API-side automatic deployment client.
func NewUnixSocketSubsystemProvisioner(enabled bool, socketPath string, timeout time.Duration) (*UnixSocketSubsystemProvisioner, error) {
	socketPath = strings.TrimSpace(socketPath)
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if enabled && socketPath == "" {
		return nil, errors.New("subsystem provisioning socket path is required")
	}
	return &UnixSocketSubsystemProvisioner{enabled: enabled, socketPath: socketPath, timeout: timeout}, nil
}

func (provisioner *UnixSocketSubsystemProvisioner) Preflight(ctx context.Context, applicationCode string) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion,
		Action:  "preflight",
		Code:    strings.TrimSpace(applicationCode),
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

func (provisioner *UnixSocketSubsystemProvisioner) Teardown(ctx context.Context, applicationCode, environment string) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version:     subsystemProvisioningProtocolVersion,
		Action:      "teardown",
		Code:        strings.TrimSpace(applicationCode),
		Environment: strings.TrimSpace(environment),
	})
}

func (provisioner *UnixSocketSubsystemProvisioner) exchange(ctx context.Context, request subsystemProvisioningRequest) error {
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
	var err error
	switch request.Action {
	case "preflight":
		err = executor.Preflight(ctx, request.Code)
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
		err = executor.Teardown(ctx, request.Code, request.Environment)
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
