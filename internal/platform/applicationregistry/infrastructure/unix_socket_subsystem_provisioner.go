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
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

const (
	subsystemProvisioningProtocolVersion  = 2
	subsystemProvisioningMaxConnections   = 16
	subsystemProvisioningHandshakeTimeout = 10 * time.Second
)

type subsystemProvisioningRequest struct {
	Recovery    *application.SubsystemAccessAbandonRequest    `json:"recovery,omitempty"`
	Receipt     *application.SubsystemAccessDeploymentReceipt `json:"receipt,omitempty"`
	Version     int                                           `json:"version"`
	Action      string                                        `json:"action"`
	RequestID   string                                        `json:"request_id,omitempty"`
	Code        string                                        `json:"code,omitempty"`
	TenantID    string                                        `json:"tenant_id,omitempty"`
	Environment string                                        `json:"environment,omitempty"`
	// ManifestChecksum 供 teardown 在生产模式满足与 preflight/provision/update 相同的
	// 已批准清单门槛；客户端按 code→environment 从服务端下发的目标投影解析后填入。
	ManifestChecksum string                                  `json:"manifest_checksum,omitempty"`
	Preflight        *application.SubsystemPreflightInput    `json:"preflight,omitempty"`
	Input            *application.SubsystemProvisioningInput `json:"input,omitempty"`
	Access           *subsystemAccessApplyPayload            `json:"access,omitempty"`
	Discovery        *subsystemServiceDiscoveryRequest       `json:"discovery,omitempty"`
}

type subsystemServiceDiscoveryRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
}

// subsystemAccessApplyPayload 携带 apply-access 动作的对外访问配置。
type subsystemAccessApplyPayload struct {
	PublicOrigin              string `json:"public_origin"`
	AllowInsecureHTTPRedirect bool   `json:"allow_insecure_http_redirect"`
}

type subsystemProvisioningReply struct {
	RecoveryEvidence        *application.SubsystemAccessRecoveryEvidence  `json:"recovery_evidence,omitempty"`
	DeploymentOutcome       *application.SubsystemAccessDeploymentOutcome `json:"deployment_outcome,omitempty"`
	DeploymentCompleted     bool                                          `json:"deployment_completed,omitempty"`
	ErrorCode               string                                        `json:"error_code,omitempty"`
	RuntimeLicenseLifecycle *application.RuntimeLicenseReleaseApproval    `json:"runtime_license_lifecycle,omitempty"`
	RuntimeLicenseApproval  []application.RuntimeLicenseApproval          `json:"runtime_license_approval,omitempty"`
	Success                 bool                                          `json:"success"`
	// Message 是单行短摘要，供平台 next_action 稳定匹配；可能包含换行的脱敏日志详情
	// 单独放 Detail，避免被安全过滤整段吞掉。
	Message      string                                         `json:"message,omitempty"`
	Detail       string                                         `json:"detail,omitempty"`
	Services     []application.SubsystemServiceInstance         `json:"services,omitempty"`
	Candidates   []application.SubsystemDiscoveryCandidate      `json:"candidates,omitempty"`
	Capabilities *application.SubsystemProvisioningCapabilities `json:"capabilities,omitempty"`
	// Images 是批准清单中的镜像在本机是否存在的明细（协议只增不改：旧客户端忽略该字段）。
	Images          []application.SubsystemImageReadiness `json:"images,omitempty"`
	LicenseEvidence *licenseevidence.Report               `json:"license_evidence,omitempty"`
	// RuntimeObservation 是普通只读运行观察；与需要批准材料的 LicenseEvidence 分开。
	RuntimeObservation *licenseevidence.RuntimeObservation `json:"runtime_observation,omitempty"`
}

// subsystemImageReadinessProvider 是可选能力：部署 Agent 读取本机镜像存在性。
// 未实现时应答不带 images 字段，平台按未验证处理（既不当作就绪，也不当作缺失）。
type subsystemImageReadinessProvider interface {
	SubsystemImageReadiness(ctx context.Context, applicationCode, environment string) ([]application.SubsystemImageReadiness, error)
}

type subsystemServiceDiscovery interface {
	DiscoverSubsystemServices(context.Context, string, string) ([]application.SubsystemServiceInstance, error)
}

type subsystemCandidateDiscovery interface {
	DiscoverSubsystemCandidates(context.Context) ([]application.SubsystemDiscoveryCandidate, error)
}

type subsystemAvailableCapabilityProvider interface {
	AvailableCapabilities(context.Context) (application.SubsystemProvisioningCapabilities, error)
}

// UnixSocketSubsystemProvisioner 是无特权 API 侧客户端，只能通过 Unix socket 请求固定动作；
// 它不持有 Docker socket，也不接收命令输出，特权文件和命令执行始终留在隔离 Agent 内。
type UnixSocketSubsystemProvisioner struct {
	enabled                  bool
	socketPath               string
	timeout                  time.Duration
	capabilities             application.SubsystemProvisioningCapabilities
	releaseIntegrityRequired bool
	releaseDigest            string
}

var releaseDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ConfigureReleaseIntegrity 将后续 Provision/Update 请求绑定到
// 预期的已安装发布摘要。Agent 仍会验证清单，
// 并在摘要不匹配时拒绝请求。
func (provisioner *UnixSocketSubsystemProvisioner) ConfigureReleaseIntegrity(required bool, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected != "" && !releaseDigestPattern.MatchString(expected) {
		return fmt.Errorf("expected release digest must be sha256:<64 lowercase hex>")
	}
	if required && expected == "" {
		return fmt.Errorf("release integrity is required but no expected release digest is configured")
	}
	provisioner.releaseIntegrityRequired = required
	provisioner.releaseDigest = expected
	return nil
}

// dialProvisioningSocket 容忍隔离 Agent 重启替换 socket 的短暂窗口；这只是连接层重试，不能
// 重复执行已发送的部署动作，因此真正的幂等性仍由 Agent 和持久部署状态保证。
func dialProvisioningSocket(ctx context.Context, socketPath string) (net.Conn, error) {
	dialer := net.Dialer{}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		connection, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return connection, nil
		}
		lastErr = err
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

// NewUnixSocketSubsystemProvisioner 构造 API 侧自动部署客户端；socket 路径和超时在此固定，
// 浏览器输入不能改变特权通信端点。
func NewUnixSocketSubsystemProvisioner(enabled bool, socketPath string, timeout time.Duration, policies ...application.SubsystemProvisioningCapabilities) (*UnixSocketSubsystemProvisioner, error) {
	socketPath = strings.TrimSpace(socketPath)
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
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
		return nil, errors.New("subsystem provisioning mode must be local or production")
	}
	if enabled && socketPath == "" {
		return nil, errors.New("subsystem provisioning socket path is required")
	}
	return &UnixSocketSubsystemProvisioner{enabled: enabled, socketPath: socketPath, timeout: timeout, capabilities: capabilities}, nil
}

// Capabilities 返回服务端配置部署策略的防御性副本。特权 Agent 独立强制同一套值，
// 绝不信任这个 UI 投影。
func (provisioner *UnixSocketSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	capabilities := provisioner.capabilities
	capabilities.SupportedApplicationCodes = append([]string(nil), capabilities.SupportedApplicationCodes...)
	capabilities.SupportedEnvironments = append([]string(nil), capabilities.SupportedEnvironments...)
	capabilities.Targets = append([]application.SubsystemProvisioningTarget(nil), capabilities.Targets...)
	return capabilities
}

// AvailableCapabilities 向特权 Agent 询问当前非敏感的可用性投影。它与 Capabilities 有意分离：
// 安全检查仍使用静态策略，而实时可用性跟随 prepare/remove 操作，且不把 .release.env 或其
// 其他值暴露给 API 容器。
func (provisioner *UnixSocketSubsystemProvisioner) AvailableCapabilities(ctx context.Context) (application.SubsystemProvisioningCapabilities, error) {
	if !provisioner.enabled {
		capabilities := provisioner.Capabilities()
		if capabilities.Mode == "production" {
			capabilities.SupportedApplicationCodes = nil
			capabilities.SupportedEnvironments = nil
			capabilities.Targets = nil
			capabilities.DefaultApplicationCode = ""
			capabilities.DefaultApplicationName = ""
			capabilities.DefaultDescription = ""
			capabilities.DefaultEnvironment = ""
			capabilities.DefaultUpstreamURL = ""
			capabilities.DefaultPathPrefix = ""
			capabilities.DefaultClientType = ""
		}
		return capabilities, nil
	}
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(operationCtx, provisioner.socketPath)
	if err != nil {
		return application.SubsystemProvisioningCapabilities{}, provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(operationCtx, connection)
	request := subsystemProvisioningRequest{
		Version:   subsystemProvisioningProtocolVersion,
		Action:    "capabilities",
		RequestID: normalizedProvisioningRequestID(requestctx.RequestID(ctx)),
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return application.SubsystemProvisioningCapabilities{}, provisioningError("send capabilities request")
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 256*1024)).Decode(&reply); err != nil {
		return application.SubsystemProvisioningCapabilities{}, provisioningError("read capabilities response")
	}
	if !reply.Success || reply.Capabilities == nil {
		return application.SubsystemProvisioningCapabilities{}, provisioningRejectionError(reply.Message, reply.Detail)
	}
	capabilities := normalizeSubsystemProvisioningCapabilities(*reply.Capabilities)
	capabilities.Enabled = reply.Capabilities.Enabled
	return capabilities, nil
}

// watchProvisioningCancellation 让阻塞的 socket 往返在调用方 ctx 取消时立刻失败。协议的
// Read/Write 只认固定 deadline；没有这个看护，管理请求断连（浏览器刷新、CLI 超时、进程
// 退出）会让 handler 在 Decode 上阻塞满整个部署窗口。
func watchProvisioningCancellation(ctx context.Context, connection net.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		_ = connection.SetDeadline(time.Now())
	}()
	return done
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
	capabilities.ReleaseIntegrity = strings.ToLower(strings.TrimSpace(capabilities.ReleaseIntegrity))
	capabilities.Targets = append([]application.SubsystemProvisioningTarget(nil), capabilities.Targets...)
	for index := range capabilities.Targets {
		capabilities.Targets[index].ApplicationCode = strings.ToLower(strings.TrimSpace(capabilities.Targets[index].ApplicationCode))
		capabilities.Targets[index].ApplicationName = strings.TrimSpace(capabilities.Targets[index].ApplicationName)
		capabilities.Targets[index].Description = strings.TrimSpace(capabilities.Targets[index].Description)
		capabilities.Targets[index].Environment = strings.ToLower(strings.TrimSpace(capabilities.Targets[index].Environment))
		capabilities.Targets[index].UpstreamURL = strings.TrimRight(strings.TrimSpace(capabilities.Targets[index].UpstreamURL), "/")
		capabilities.Targets[index].PathPrefix = strings.TrimRight(strings.TrimSpace(capabilities.Targets[index].PathPrefix), "/")
		capabilities.Targets[index].ClientType = strings.ToLower(strings.TrimSpace(capabilities.Targets[index].ClientType))
	}
	if len(capabilities.Targets) > 0 {
		defaultTarget := capabilities.Targets[0]
		if capabilities.DefaultApplicationCode == "" {
			capabilities.DefaultApplicationCode = defaultTarget.ApplicationCode
		}
		if capabilities.DefaultApplicationName == "" {
			capabilities.DefaultApplicationName = defaultTarget.ApplicationName
		}
		if capabilities.DefaultDescription == "" {
			capabilities.DefaultDescription = defaultTarget.Description
		}
		if capabilities.DefaultEnvironment == "" {
			capabilities.DefaultEnvironment = defaultTarget.Environment
		}
		if capabilities.DefaultUpstreamURL == "" {
			capabilities.DefaultUpstreamURL = defaultTarget.UpstreamURL
		}
		if capabilities.DefaultPathPrefix == "" {
			capabilities.DefaultPathPrefix = defaultTarget.PathPrefix
		}
		if capabilities.DefaultClientType == "" {
			capabilities.DefaultClientType = defaultTarget.ClientType
		}
	}
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
}

// ReadSubsystemAccessImages 读取「批准清单中的镜像在本机是否存在」的明细（只读）。
// reported=false 表示该部署 Agent 未上报该字段（旧版本），调用方必须按「未验证」处理。
func (provisioner *UnixSocketSubsystemProvisioner) ReadSubsystemAccessImages(ctx context.Context, applicationCode, environment string) ([]application.SubsystemImageReadiness, bool, error) {
	if !provisioner.enabled {
		return nil, false, nil
	}
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(operationCtx, provisioner.socketPath)
	if err != nil {
		return nil, false, provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(operationCtx, connection)
	request := subsystemProvisioningRequest{
		Version:     subsystemProvisioningProtocolVersion,
		Action:      "capabilities",
		Code:        strings.TrimSpace(applicationCode),
		Environment: strings.ToLower(strings.TrimSpace(environment)),
		RequestID:   normalizedProvisioningRequestID(requestctx.RequestID(ctx)),
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, false, provisioningError("send capabilities request")
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 256*1024)).Decode(&reply); err != nil {
		return nil, false, provisioningError("read capabilities response")
	}
	if !reply.Success {
		return nil, false, provisioningRejectionError(reply.Message, reply.Detail)
	}
	if reply.Images == nil {
		return nil, false, nil
	}
	return reply.Images, true, nil
}

// Preflight 把前置检查请求转发给部署辅助进程；本进程不做任何本地判断，Agent 侧的租户、
// 镜像与文件边界校验是唯一权威。Agent 未启用时失败关闭。
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

// Provision 把接入请求转发给部署辅助进程执行；本进程不落盘任何部署产物，Agent 未启用时
// 拒绝而不是排队。
func (provisioner *UnixSocketSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	if input.ReleaseDigest == "" {
		input.ReleaseDigest = provisioner.releaseDigest
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion,
		Action:  "provision",
		Input:   &input,
	})
}

// Update 把受控更新请求转发给部署辅助进程执行；失败语义与响应拒绝原因由 Agent 返回，
// 本进程只负责转发与传递取消。
func (provisioner *UnixSocketSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	if input.ReleaseDigest == "" {
		input.ReleaseDigest = provisioner.releaseDigest
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion,
		Action:  "update",
		Input:   &input,
	})
}

// ApplyAccess 请求部署辅助进程重写本机覆盖环境文件并重建受影响容器，使统一前端与子系统
// 回调使用配置的公开 origin（空 origin 恢复仅本机访问）。
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

// Teardown 把拆除请求转发给部署辅助进程，并附上服务端批准投影中的清单校验和，让 Agent
// 能核对该次拆除与已批准清单一致，防止误拆其他部署。
func (provisioner *UnixSocketSubsystemProvisioner) Teardown(ctx context.Context, tenantID, applicationCode, environment string) error {
	if !provisioner.enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchange(ctx, subsystemProvisioningRequest{
		Version:          subsystemProvisioningProtocolVersion,
		Action:           "teardown",
		TenantID:         strings.TrimSpace(tenantID),
		Code:             strings.TrimSpace(applicationCode),
		Environment:      strings.TrimSpace(environment),
		ManifestChecksum: provisioner.manifestChecksumFor(applicationCode, environment),
	})
}

// manifestChecksumFor 按 code→environment 从服务端下发的目标投影解析已批准清单校验和，写法与
// 控制面 handler.manifestChecksumForTarget 一致；生产模式 teardown 依赖它通过 Agent 的清单门槛。
func (provisioner *UnixSocketSubsystemProvisioner) manifestChecksumFor(applicationCode, environment string) string {
	code := strings.TrimSpace(applicationCode)
	targetEnvironment := strings.TrimSpace(environment)
	for _, target := range provisioner.capabilities.Targets {
		if strings.EqualFold(target.ApplicationCode, code) && strings.EqualFold(target.Environment, targetEnvironment) {
			return strings.TrimSpace(target.ManifestChecksum)
		}
	}
	return ""
}

// Discover 向隔离 Agent 查询当前运行时声明的服务。响应只含路由元数据；
// 秘密与宿主路径绝不跨越 socket。
func (provisioner *UnixSocketSubsystemProvisioner) Discover(ctx context.Context, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	if !provisioner.enabled {
		return nil, provisioningError("automatic subsystem deployment is disabled")
	}
	services, err := provisioner.exchangeDiscovery(ctx, subsystemServiceDiscoveryRequest{ApplicationCode: strings.TrimSpace(applicationCode), Environment: strings.TrimSpace(environment)})
	if err != nil {
		return nil, err
	}
	return services, nil
}

// DiscoverSubsystemCandidates 盘点主动声明且尚未登记的子系统容器。API 进程仍无 Docker
// socket；只读扫描由隔离辅助进程执行。
func (provisioner *UnixSocketSubsystemProvisioner) DiscoverSubsystemCandidates(ctx context.Context) ([]application.SubsystemDiscoveryCandidate, error) {
	if !provisioner.enabled {
		return nil, provisioningError("automatic subsystem deployment is disabled")
	}
	return provisioner.exchangeCandidateDiscovery(ctx)
}

// exchange 通过受限Unix协议执行固定部署操作并保留结构化拒绝类别。
// 参数：ctx控制取消与截止时间，request为已授权目标及固定动作封套。
// 返回值：明确执行成功为空；通信或部署失败返回错误，不能据此断定无副作用。
func (provisioner *UnixSocketSubsystemProvisioner) exchange(ctx context.Context, request subsystemProvisioningRequest) error {
	// API 容器不持有 Docker socket，只能通过受限 Unix 协议请求固定动作。超时同时作用于
	// 连接、发送和响应，防止部署 Agent 卡住后耗尽 API 请求协程。
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.timeout)
	defer cancel()
	// HTTP 层生成的规范请求号随受限协议传到 Agent，只用于日志关联，不参与授权、文件名
	// 或命令参数。这样浏览器给出的追踪号能够直接定位到对应的 Agent 失败行。
	request.RequestID = normalizedProvisioningRequestID(requestctx.RequestID(ctx))
	connection, err := dialProvisioningSocket(operationCtx, provisioner.socketPath)
	if err != nil {
		return provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(operationCtx, connection)
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return provisioningError("send deployment request")
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 64*1024)).Decode(&reply); err != nil {
		return provisioningError("read deployment response")
	}
	if !reply.Success {
		if reply.ErrorCode == application.AccessCredentialRetryRequired {
			return serviceCredentialRetryError()
		}
		if known := runtimeApprovalReplyError(reply.ErrorCode); application.RuntimeLicenseApprovalErrorCode(known) == reply.ErrorCode {
			return known
		}
		return provisioningRejectionError(reply.Message, reply.Detail)
	}
	return nil
}

// provisioningRejectionError 把 Agent 的单行摘要与详情合并成一个错误。Agent 为兼容
// next_action 匹配会同时返回摘要和详情，短错误时两者完全相同；这里必须先判重再拼接，
// 否则页面会出现 "原因: 原因: 原因" 这类无法阅读的重复文本。
func provisioningRejectionError(message, detail string) error {
	message = strings.TrimSpace(message)
	detail = strings.TrimSpace(detail)
	var reason string
	switch {
	case message == "" && detail == "":
		return provisioningError("deployment helper rejected the request")
	case detail == "":
		reason = message
	case message == "":
		reason = detail
	// 摘要可能是详情折叠换行后的截断版本，也可能详情只是摘要的一部分；任一情况都保留
	// 信息量更大的一方，避免丢失多行日志细节。
	case message == detail, strings.Contains(detail, message):
		reason = detail
	case strings.Contains(message, detail):
		reason = message
	default:
		reason = message + ": " + detail
	}
	// Agent 内部的兜底拒绝会把哨兵错误原文当作 message 和 detail 返回。此时再包装一次
	// 只会得到 "unavailable: unavailable"，因此直接返回哨兵本身。
	if reason == application.ErrSubsystemProvisioningUnavailable.Error() {
		return application.ErrSubsystemProvisioningUnavailable
	}
	return provisioningError(reason)
}

func (provisioner *UnixSocketSubsystemProvisioner) exchangeDiscovery(ctx context.Context, discovery subsystemServiceDiscoveryRequest) ([]application.SubsystemServiceInstance, error) {
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(operationCtx, provisioner.socketPath)
	if err != nil {
		return nil, provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(operationCtx, connection)
	request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "discover", Discovery: &discovery}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, provisioningError("send discovery request")
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 256*1024)).Decode(&reply); err != nil {
		return nil, provisioningError("read discovery response")
	}
	if !reply.Success {
		message := strings.TrimSpace(reply.Message)
		if message == "" {
			message = "deployment helper rejected the discovery request"
		}
		return nil, provisioningError(message)
	}
	return reply.Services, nil
}

func (provisioner *UnixSocketSubsystemProvisioner) exchangeCandidateDiscovery(ctx context.Context) ([]application.SubsystemDiscoveryCandidate, error) {
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(operationCtx, provisioner.socketPath)
	if err != nil {
		return nil, provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(operationCtx, connection)
	request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "discover-candidates", RequestID: normalizedProvisioningRequestID(requestctx.RequestID(ctx))}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, provisioningError("send discovery request")
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 256*1024)).Decode(&reply); err != nil {
		return nil, provisioningError("read discovery response")
	}
	if !reply.Success {
		message := strings.TrimSpace(reply.Message)
		if message == "" {
			message = "deployment helper rejected the discovery request"
		}
		return nil, provisioningError(message)
	}
	return reply.Candidates, nil
}

// RunSubsystemProvisioningServer 提供窄化的 Unix-socket 协议，监听端点应只属于隔离 Helper，
// 不发布网络端口；协议解析、请求号和错误摘要都在边界处脱敏，避免内部细节回流 API。
//
// SEC-F4 降级说明：对端身份校验（SO_PEERCRED UID/GID 白名单，如
// SUBSYSTEM_PROVISIONING_ALLOWED_UIDS 环境变量）暂未实现——SO_PEERCRED 是 Linux 专属接口，
// 需要按平台拆分带 build tag 的新文件，超出本次修复的 write scope（仅限本文件与 *_test.go）。
// 当前防护依赖 0750 目录 + 0660 socket 权限与生产清单门槛；后续建议新增 peercred_linux.go /
// peercred_stub.go，在 Accept 后按白名单校验对端 UID/GID，不支持的平台回退权限 + 每请求 HMAC。
func RunSubsystemProvisioningServer(ctx context.Context, socketPath string, executor application.SubsystemProvisioner) error {
	if executor == nil || strings.TrimSpace(socketPath) == "" {
		return errors.New("subsystem provisioning server dependencies are required")
	}
	socketPath = filepath.Clean(strings.TrimSpace(socketPath))
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("create provisioning socket directory: %w", err)
	}
	// 安全（SEC-F4）：MkdirAll 只在新建目录时应用 0750，对已存在目录必须再主动 Chmod 收紧，
	// 否则历史遗留的宽松目录会让任意本机用户枚举并连接部署 socket。
	if err := os.Chmod(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("tighten provisioning socket directory permissions: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale provisioning socket: %w", err)
	}
	// 安全（SEC-F4）：unix socket 文件按 0777&^umask 创建，Listen 与随后的 Chmod 0660 之间存在
	// 宽松权限窗口；listen 期间临时把进程 umask 收紧到 077，让文件出生即为 0700，再立刻收紧到
	// 0660。umask 是进程级状态，只在 Agent 启动监听的瞬间切换，且只会让并发创建的文件更严格。
	previousUmask := syscall.Umask(0o077)
	listener, err := net.Listen("unix", socketPath)
	syscall.Umask(previousUmask)
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
	connectionSlots := make(chan struct{}, subsystemProvisioningMaxConnections)
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept provisioning request: %w", acceptErr)
		}
		select {
		case connectionSlots <- struct{}{}:
			go func() {
				defer func() { <-connectionSlots }()
				handleSubsystemProvisioningConnection(ctx, connection, executor)
			}()
		default:
			_ = connection.SetWriteDeadline(time.Now().Add(subsystemProvisioningHandshakeTimeout))
			_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: false, Message: "deployment helper is busy"})
			_ = connection.Close()
		}
	}
}

// handleSubsystemProvisioningConnection 用途：处理受限部署协议及只读完成回执查询。
// 参数：ctx 为服务上下文，connection 为已接受连接，executor 为受控部署执行器。
// 返回值：无；结果通过协议响应返回，连接始终关闭。
func handleSubsystemProvisioningConnection(ctx context.Context, connection net.Conn, executor application.SubsystemProvisioner) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(subsystemProvisioningHandshakeTimeout))
	var request subsystemProvisioningRequest
	if err := json.NewDecoder(io.LimitReader(connection, 1024*1024)).Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: false})
		return
	}
	// 请求读取完成后清除握手期限；真正部署由业务 context 的硬超时控制。
	_ = connection.SetReadDeadline(time.Time{})
	if request.Version != subsystemProvisioningProtocolVersion {
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: false, Message: "deployment protocol version is unsupported"})
		return
	}
	request.RequestID = normalizedProvisioningRequestID(request.RequestID)
	operationContext := ctx
	if request.RequestID != "" {
		operationContext = requestctx.WithRequestID(operationContext, request.RequestID)
	}
	// action 是显式白名单，网络对端不能传入任意命令、路径或参数。新增动作必须同时升级
	// 协议结构和分派逻辑，不能退化成通用 shell 执行接口。
	var err error
	requiresManifestChecksum := false
	if provider, ok := executor.(interface {
		Capabilities() application.SubsystemProvisioningCapabilities
	}); ok {
		requiresManifestChecksum = provider.Capabilities().Mode == "production"
	}
	switch request.Action {
	case "access-deployment-recovery-check", "access-deployment-abandon":
		reader, ok := executor.(accessDeploymentRecoveryAgent)
		if !ok || request.Recovery == nil || request.Receipt != nil || request.Input != nil ||
			request.Preflight != nil || request.Access != nil || request.Discovery != nil ||
			request.Code != "" || request.Environment != "" || request.TenantID != "" || request.ManifestChecksum != "" {
			err = provisioningError("deployment recovery action unavailable")
			break
		}
		if request.Action == "access-deployment-recovery-check" {
			value, readErr := reader.CheckAccessDeploymentRecovery(operationContext, *request.Recovery)
			if readErr != nil {
				err = readErr
				break
			}
			_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, RecoveryEvidence: &value})
			return
		}
		value, abandonErr := reader.AbandonAccessDeployment(operationContext, *request.Recovery)
		if abandonErr != nil {
			err = abandonErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, DeploymentOutcome: &value})
		return
	case "access-deployment-outcome":
		reader, ok := executor.(accessDeploymentOutcomeReader)
		if !ok || request.Receipt == nil || request.Input != nil || request.Preflight != nil ||
			request.Access != nil || request.Discovery != nil || request.Code != "" ||
			request.Environment != "" || request.TenantID != "" || request.ManifestChecksum != "" || request.Recovery != nil {
			err = provisioningError("deployment outcome query unavailable")
			break
		}
		value, found, readErr := reader.ReadAccessDeploymentOutcome(operationContext, *request.Receipt)
		if readErr != nil {
			err = readErr
			break
		}
		reply := subsystemProvisioningReply{Success: true}
		if found {
			reply.DeploymentOutcome = &value
		}
		_ = json.NewEncoder(connection).Encode(reply)
		return
	case "access-deployment-receipt":
		reader, ok := executor.(accessDeploymentReceiptReader)
		if !ok || request.Receipt == nil {
			err = provisioningError("deployment receipt query unavailable")
			break
		}
		completed, readErr := reader.ReadAccessDeploymentCompletion(operationContext, *request.Receipt)
		if readErr != nil {
			err = readErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, DeploymentCompleted: completed})
		return
	case "LICENSE_RUNTIME_LIFECYCLE_APPROVAL":
		provider, ok := executor.(runtimeLicenseLifecycleProvider)
		if !ok || request.Input != nil || request.Preflight != nil || request.Access != nil || request.Discovery != nil || request.ManifestChecksum != "" || request.TenantID != "" {
			err = provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: approved lifecycle action unavailable")
			break
		}
		approval, approvalErr := provider.ApprovedRuntimeLicenseLifecycle(operationContext, request.Code, request.Environment)
		if approvalErr != nil {
			err = approvalErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, RuntimeLicenseLifecycle: &approval})
		return
	case "LICENSE_RUNTIME_APPROVAL":
		provider, ok := executor.(runtimeLicenseApprovalProvider)
		if !ok || request.Input != nil || request.Preflight != nil || request.Access != nil || request.Discovery != nil || request.ManifestChecksum != "" || request.TenantID != "" {
			err = provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: approved release action unavailable")
			break
		}
		items, approvalErr := provider.ApprovedRuntimeLicenseComponents(operationContext, request.Code, request.Environment)
		if approvalErr != nil {
			err = approvalErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, RuntimeLicenseApproval: items})
		return
	case "LICENSE_MIGRATION_INSTALLATION_EVIDENCE":
		provider, ok := executor.(migrationInstallationEvidenceProvider)
		if !ok || request.Code != "" || request.Environment != "" || request.TenantID != "" || request.Input != nil || request.Preflight != nil || request.Access != nil || request.Discovery != nil || request.ManifestChecksum != "" {
			err = provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: migration boundary action unavailable")
			break
		}
		report, evidenceErr := provider.CollectMigrationInstallationEvidence(operationContext)
		if evidenceErr != nil {
			err = evidenceErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, LicenseEvidence: &report})
		return
	case "LICENSE_INSTALLATION_EVIDENCE":
		provider, ok := executor.(installationLicenseEvidenceProvider)
		if !ok || request.Code != "" || request.Environment != "" || request.TenantID != "" || request.Input != nil || request.Preflight != nil || request.Access != nil || request.Discovery != nil || request.ManifestChecksum != "" {
			err = provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: installation boundary action unavailable")
			break
		}
		report, evidenceErr := provider.CollectInstallationLicenseEvidence(operationContext)
		if evidenceErr != nil {
			err = evidenceErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, LicenseEvidence: &report})
		return
	case "LICENSE_EVIDENCE":
		provider, ok := executor.(licenseEvidenceProvider)
		if !ok || request.Input != nil || request.Preflight != nil || request.Access != nil || request.Discovery != nil || request.ManifestChecksum != "" || request.TenantID != "" {
			err = provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: approved evidence action is unavailable")
			break
		}
		report, evidenceErr := provider.CollectLicenseEvidence(operationContext, request.Code, request.Environment)
		if evidenceErr != nil {
			err = evidenceErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, LicenseEvidence: &report})
		return
	case "LICENSE_RUNTIME_OBSERVATION":
		provider, ok := executor.(runtimeObservationProvider)
		if !ok || request.Code == "" || request.Environment == "" || request.TenantID != "" || request.Receipt != nil || request.Input != nil || request.Preflight != nil || request.Access != nil || request.Discovery != nil || request.ManifestChecksum != "" {
			err = provisioningError("LICENSE_RUNTIME_OBSERVATION_UNSUPPORTED: runtime observation action unavailable")
			break
		}
		observationContext, cancel := context.WithTimeout(operationContext, 10*time.Second)
		observation, observeErr := provider.ObserveLicenseRuntime(observationContext, request.Code, request.Environment)
		cancel()
		if observeErr != nil {
			err = observeErr
			break
		}
		_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, RuntimeObservation: &observation})
		return
	case "capabilities":
		provider, ok := executor.(subsystemAvailableCapabilityProvider)
		if !ok {
			err = provisioningError("deployment helper does not support live capabilities")
			break
		}
		capabilities, capabilityErr := provider.AvailableCapabilities(operationContext)
		// 镜像存在性由本机读取；读取失败不影响 capabilities 本身（images 留空即未验证）。
		var images []application.SubsystemImageReadiness
		if imageProvider, ok := executor.(subsystemImageReadinessProvider); ok {
			if reported, imageErr := imageProvider.SubsystemImageReadiness(operationContext, request.Code, request.Environment); imageErr == nil {
				images = reported
			}
		}
		if capabilityErr != nil {
			err = capabilityErr
		} else {
			_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, Capabilities: &capabilities, Images: images})
			return
		}
	case "preflight":
		if request.Preflight == nil {
			err = provisioningError("preflight request payload is incomplete")
		} else if requiresManifestChecksum && strings.TrimSpace(request.Preflight.ManifestChecksum) == "" {
			err = provisioningError("production subsystem preflight requires an approved manifest checksum for the selected target")
		} else {
			err = executor.Preflight(operationContext, *request.Preflight)
		}
	case "provision":
		if request.Input == nil {
			err = provisioningError("provision request payload is incomplete")
		} else if requiresManifestChecksum && strings.TrimSpace(request.Input.ManifestChecksum) == "" {
			err = provisioningError("production subsystem provision requires an approved manifest checksum for the selected target")
		} else {
			err = executor.Provision(operationContext, *request.Input)
		}
	case "update":
		if request.Input == nil {
			err = provisioningError("update request payload is incomplete")
		} else if requiresManifestChecksum && strings.TrimSpace(request.Input.ManifestChecksum) == "" {
			err = provisioningError("production subsystem update requires an approved manifest checksum for the selected target")
		} else {
			err = executor.Update(operationContext, *request.Input)
		}
	case "teardown":
		// 安全（SEC-F4）：生产模式的 preflight/provision/update 都要求已批准清单校验和，
		// teardown 必须走同一门槛，否则可绕过清单审批直接下线生产子系统。
		if requiresManifestChecksum && strings.TrimSpace(request.ManifestChecksum) == "" {
			err = provisioningError("production subsystem teardown requires an approved manifest checksum for the selected target")
		} else {
			err = executor.Teardown(operationContext, request.TenantID, request.Code, request.Environment)
		}
	case "apply-access":
		if request.Access == nil {
			err = provisioningError("access configuration payload is incomplete")
			break
		}
		applier, ok := executor.(interface {
			ApplyAccess(context.Context, settingsapplication.AccessApplyInput) error
		})
		if !ok {
			err = provisioningError("deployment helper does not support applying public access configuration")
			break
		}
		// 安全（SEC-F1）：socket 对端可直接设置 PublicOrigin；转发前先做与 ApplyAccess 内一致的
		// origin 校验，拒绝换行/NUL 注入与非 http(s)、带 path 的值，避免 .env.lan 换行注入。
		publicOrigin, originErr := normalizeAccessPublicOrigin(request.Access.PublicOrigin)
		if originErr != nil {
			err = originErr
			break
		}
		err = applier.ApplyAccess(operationContext, settingsapplication.AccessApplyInput{
			PublicOrigin:              publicOrigin,
			AllowInsecureHTTPRedirect: request.Access.AllowInsecureHTTPRedirect,
		})
	case "discover":
		discoverer, ok := executor.(subsystemServiceDiscovery)
		if request.Discovery == nil {
			err = provisioningError("service discovery payload is incomplete")
			break
		}
		if !ok {
			err = provisioningError("deployment helper does not support service discovery")
			break
		}
		services, discoverErr := discoverer.DiscoverSubsystemServices(operationContext, request.Discovery.ApplicationCode, request.Discovery.Environment)
		if discoverErr != nil {
			err = discoverErr
		} else {
			_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, Services: services})
			return
		}
	case "discover-candidates":
		discoverer, ok := executor.(subsystemCandidateDiscovery)
		if !ok {
			err = provisioningError("deployment helper does not support candidate discovery")
			break
		}
		candidates, discoverErr := discoverer.DiscoverSubsystemCandidates(operationContext)
		if discoverErr != nil {
			err = discoverErr
		} else {
			_ = json.NewEncoder(connection).Encode(subsystemProvisioningReply{Success: true, Candidates: candidates})
			return
		}
	default:
		err = provisioningError("deployment action is not supported")
	}
	reply := subsystemProvisioningReply{Success: err == nil}
	if err != nil {
		if errors.Is(err, application.ErrServiceCredentialRetryRequired) {
			reply.ErrorCode = application.AccessCredentialRetryRequired
		}
		if application.RuntimeLicenseApprovalErrorCode(err) != "" || request.Action == "LICENSE_RUNTIME_APPROVAL" || request.Action == "LICENSE_RUNTIME_LIFECYCLE_APPROVAL" {
			err = application.RuntimeLicenseApprovalFailure(err)
			reply.ErrorCode = application.RuntimeLicenseApprovalErrorCode(err)
		}
		if reply.ErrorCode == "" && (request.Action == "access-deployment-recovery-check" || request.Action == "access-deployment-abandon") {
			if errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
				reply.ErrorCode = "ACCESS_RECOVERY_CONFLICT"
			} else if errors.Is(err, application.ErrValidation) {
				reply.ErrorCode = "ACCESS_RECOVERY_INVALID"
			}
		}
		reply.Message = safeSubsystemProvisioningMessage(err)
		reply.Detail = safeSubsystemProvisioningDetail(err)
		requestID := request.RequestID
		if requestID == "" {
			requestID = "-"
		}
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] request_id=%s action=%s code=%s failed: %s\n", requestID, request.Action, requestCode(request), reply.Message)
	}
	_ = json.NewEncoder(connection).Encode(reply)
}

func normalizedProvisioningRequestID(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 26 {
		return ""
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", character) {
			return ""
		}
	}
	return value
}

// safeSubsystemProvisioningMessage 生成单行短摘要：折叠换行、保留可执行步骤前缀，供平台
// next_action 稳定匹配；长文本和换行交给 safeSubsystemProvisioningDetail。
func safeSubsystemProvisioningMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	message = strings.TrimSpace(strings.TrimPrefix(message, application.ErrSubsystemProvisioningUnavailable.Error()+":"))
	message = strings.Join(strings.Fields(message), " ")
	if message == "" || len(message) > 256 {
		return "deployment helper rejected the request"
	}
	return message
}

// safeSubsystemProvisioningDetail 返回脱敏错误详情（可能包含换行），限制长度避免协议和
// 页面被超大输出淹没。Agent 已在生成详情时移除明文凭据，这里只做兜底截断。
func safeSubsystemProvisioningDetail(err error) string {
	const limit = 4 * 1024
	message := strings.TrimSpace(err.Error())
	message = strings.TrimSpace(strings.TrimPrefix(message, application.ErrSubsystemProvisioningUnavailable.Error()+":"))
	if message == "" {
		return ""
	}
	if len(message) > limit {
		message = message[:limit] + "...(truncated)"
	}
	return message
}

func requestCode(request subsystemProvisioningRequest) string {
	if request.Input != nil {
		return strings.TrimSpace(request.Input.ApplicationCode)
	}
	return strings.TrimSpace(request.Code)
}
