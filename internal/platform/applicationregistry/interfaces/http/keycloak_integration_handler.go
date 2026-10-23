package http

import (
	"bytes"
	"encoding/json"
	"errors"
	stdhttp "net/http"
)

// KeycloakIntegrationHandler 是应用环境认证集成的对外边界。刻意把全部工作
// 委托给既有子系统控制面：应用目录、角色目录与最终授权仍归基础平台持有，
// 本门面只负责 Keycloak 客户端同步与 Issuer 切换操作。
//
// 绝不暴露 Keycloak Admin 凭据、客户端 Secret 或部署运行时凭据。
// 旧版 /subsystem-* 端点继续保留，供既有控制台版本与自动化客户端使用。
type KeycloakIntegrationHandler struct {
	subsystems KeycloakIntegrationUseCases
}

// NewKeycloakIntegrationHandler 装配集成门面；用例集缺失即拒绝构造，
// 避免挂出无后端的空壳端点。
func NewKeycloakIntegrationHandler(subsystems KeycloakIntegrationUseCases) (*KeycloakIntegrationHandler, error) {
	if subsystems == nil {
		return nil, errors.New("subsystem onboarding handler is required")
	}
	return &KeycloakIntegrationHandler{subsystems: subsystems}, nil
}

// Capabilities 仅暴露公开的提供方状态与切换门禁。
func (handler *KeycloakIntegrationHandler) Capabilities(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetSubsystemCapabilities(writer, request)
}

// Status 返回单个应用环境的持久化、非敏感认证集成状态。刻意不向浏览器
// 转发 Keycloak Admin API 对象、客户端 Secret 或运行时凭据。
func (handler *KeycloakIntegrationHandler) Status(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetKeycloakIntegrationStatus(writer, request)
}

// SyncClient 创建或更新服务端 Keycloak 客户端与平台持有的 claims 投影。
// 客户端 Secret 只在平台、Keycloak 与部署 Agent 三方之间流转。
func (handler *KeycloakIntegrationHandler) SyncClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.SyncKeycloakClient(writer, request)
}

// Switch 把环境切到 Keycloak Issuer。Issuer 别名由该路由固定，
// 不接受浏览器载荷指定，防止请求被改道到任意身份源。
func (handler *KeycloakIntegrationHandler) Switch(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.SwitchToKeycloak(writer, request)
}

// Rollback 把环境回退到基础平台 Issuer。与 Switch 相同，目标 Issuer 固定在服务端。
func (handler *KeycloakIntegrationHandler) Rollback(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.RollbackToPlatform(writer, request)
}

// ListProjectionFailures 支撑 Keycloak 授权投影管理页，仅列出 FAILED/死信记录。
func (handler *KeycloakIntegrationHandler) ListProjectionFailures(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.ListKeycloakProjectionFailures(writer, request)
}

// ProjectionAlerts 暴露失败投影记录的紧凑告警/指标状态，不泄露队列载荷、
// 客户端 Secret 或令牌。
func (handler *KeycloakIntegrationHandler) ProjectionAlerts(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	// 保持公开集成契约与内部运维表示相互独立：浏览器与自动化依赖这些
	// 小写 snake_case 键，不应被要求反推 Go 字段名。
	captured := newProjectionAlertsResponseCapture()
	handler.subsystems.GetKeycloakProjectionAlerts(captured, request)
	writeProjectionAlertsContract(writer, captured)
}

// projectionAlertsResponseCapture 在内存中捕获内部运维 handler 的响应，
// 以便按公开集成契约重新封装后输出；Header/WriteHeader/Write 仅用于满足
// stdhttp.ResponseWriter，无其他语义。
type projectionAlertsResponseCapture struct {
	header stdhttp.Header
	status int
	body   bytes.Buffer
}

func newProjectionAlertsResponseCapture() *projectionAlertsResponseCapture {
	return &projectionAlertsResponseCapture{header: make(stdhttp.Header)}
}

func (capture *projectionAlertsResponseCapture) Header() stdhttp.Header { return capture.header }

func (capture *projectionAlertsResponseCapture) WriteHeader(status int) { capture.status = status }

func (capture *projectionAlertsResponseCapture) Write(payload []byte) (int, error) {
	if capture.status == 0 {
		capture.status = stdhttp.StatusOK
	}
	return capture.body.Write(payload)
}

type projectionAlertsEnvelope struct {
	Code      string                `json:"code"`
	Message   string                `json:"message"`
	RequestID string                `json:"request_id"`
	Data      projectionAlertsState `json:"data"`
}

type projectionAlertsState struct {
	Severity                 string `json:"severity"`
	State                    string `json:"state"`
	Summary                  string `json:"summary"`
	FailedCount              int64  `json:"failed_count"`
	AffectedEnvironmentCount int64  `json:"affected_environment_count"`
	OldestFailedAt           any    `json:"oldest_failed_at,omitempty"`
}

func writeProjectionAlertsContract(writer stdhttp.ResponseWriter, captured *projectionAlertsResponseCapture) {
	for key, values := range captured.header {
		writer.Header()[key] = append([]string(nil), values...)
	}
	status := captured.status
	if status == 0 {
		status = stdhttp.StatusOK
	}
	if status != stdhttp.StatusOK {
		writer.WriteHeader(status)
		_, _ = writer.Write(captured.body.Bytes())
		return
	}
	var response projectionAlertsEnvelope
	if err := json.Unmarshal(captured.body.Bytes(), &response); err != nil || response.Code != "OK" {
		writer.WriteHeader(status)
		_, _ = writer.Write(captured.body.Bytes())
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(response)
}

// ReplayProjectionFailure 是带守卫、幂等的 FAILED -> PENDING 受控重放。
func (handler *KeycloakIntegrationHandler) ReplayProjectionFailure(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.ReplayKeycloakProjectionFailure(writer, request)
}

// SyncStatus 返回单个子系统环境的聚焦同步状态（含漂移检测结果），
// 面向运维看板与自动化健康检查。
func (handler *KeycloakIntegrationHandler) SyncStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetKeycloakSyncStatus(writer, request)
}

// HealthDashboard 返回全部子系统的集成健康度汇总。
func (handler *KeycloakIntegrationHandler) HealthDashboard(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetSubsystemHealthDashboard(writer, request)
}

// StartObservation 开启环境切换到 Keycloak 前必需的七天证据观察窗口。
func (handler *KeycloakIntegrationHandler) StartObservation(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.StartKeycloakObservation(writer, request)
}

// VerifyBrokerLogin 在专用集成命名空间下保留 Keycloak JWT 校验步骤；
// 窄口径的 Keycloak Bearer 边界由路由中间件而非本门面执行。
func (handler *KeycloakIntegrationHandler) VerifyBrokerLogin(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.VerifyKeycloakBrokerLogin(writer, request)
}
