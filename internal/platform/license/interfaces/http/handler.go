package licensehttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	core "github.com/J-S-Te/license-core"
)

// runtimeStatusReader 是协调器按应用读取运行状态的最小只读入口。
type runtimeStatusReader interface {
	Status(context.Context, string) (coordination.Status, error)
}

type Handler struct {
	service     *application.Service
	environment string
	runtime     runtimeStatusReader
}

func NewHandler(service *application.Service, environment string) (*Handler, error) {
	if service == nil || environment == "" {
		return nil, domain.ErrInvalid
	}
	return &Handler{service: service, environment: environment}, nil
}

// ConfigureRuntimeStatus 挂接运行时协调状态读取；nil 表示协调器不可用，
// 此时状态接口仍可解析，仅把运行执行为空表示。
func (h *Handler) ConfigureRuntimeStatus(reader runtimeStatusReader) {
	h.runtime = reader
}
func principal(w http.ResponseWriter, r *http.Request, manage bool) (domain.Actor, bool) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok || p.User.ID == "" || p.Tenant.ID == "" {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return domain.Actor{}, false
	}
	permission := "platform:license:read"
	if manage {
		permission = "platform:license:manage"
	}
	for _, v := range p.PermissionCodes {
		if v == permission {
			return domain.Actor{TenantID: p.Tenant.ID, UserID: p.User.ID}, true
		}
	}
	httpresponse.WriteError(w, r, 403, httperror.Forbidden)
	return domain.Actor{}, false
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxTokenBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(target); e != nil {
		writeError(w, r, domain.ErrInvalid)
		return false
	}
	var rest any
	if e := d.Decode(&rest); e != io.EOF {
		writeError(w, r, domain.ErrInvalid)
		return false
	}
	return true
}
func writeError(w http.ResponseWriter, r *http.Request, e error) {
	code, status, message := "LICENSE_STORAGE_FAILED", 500, "授权存储暂时不可用，请稍后重试"
	switch {
	case errors.Is(e, domain.ErrNotInitialized):
		code, status, message = "LICENSE_NOT_INITIALIZED", 409, "请先初始化部署实例"
	case errors.Is(e, domain.ErrBindingMismatch):
		code, status, message = "LICENSE_BINDING_MISMATCH", 409, "该部署实例已绑定其他客户或环境，不能在此环境初始化"
	case errors.Is(e, domain.ErrVersionNotIncreasing):
		code, status, message = "LICENSE_VERSION_NOT_INCREASING", 409, "该许可证版本不高于当前已生效版本，无需重复导入；如需变更请让厂商签发更高版本"
	case errors.Is(e, domain.ErrConflict):
		code, status, message = "LICENSE_VERSION_CONFLICT", 409, "授权状态已变化或版本过旧，请重新预览"
	case errors.Is(e, domain.ErrConfirmation):
		code, status, message = "LICENSE_CONFIRMATION_REQUIRED", 400, "请明确确认授权差异或替换待生效许可证"
	case errors.Is(e, domain.ErrClock):
		code, status, message = "LICENSE_CLOCK_RECOVERY_REQUIRED", 409, "检测到时间回拨，需要签名时间恢复凭据"
	case errors.Is(e, domain.ErrCorrupt):
		code, status, message = "LICENSE_STATE_INVALID", 409, "持久授权状态校验失败，需要管理员核查"
	case errors.Is(e, domain.ErrTrustNotConfigured):
		code, status, message = "LICENSE_TRUST_NOT_CONFIGURED", 503, "当前发行版尚未配置厂商可信公钥"
	case errors.Is(e, domain.ErrInvalid), errors.Is(e, core.ErrInvalid), errors.Is(e, core.ErrDenied), errors.Is(e, core.ErrRollback):
		code, status, message = "LICENSE_VALIDATION_FAILED", 400, "许可证或请求无效，请检查签名、实例、环境及字段"
	}
	httpresponse.WriteError(w, r, status, httperror.New(code, message, nil))
}

// appEnforcement 是单个应用的运行强制执行聚合；不可用或未登记也保留结构，
// 便于前端区分“未接入”与“已接入但未登记”。
type appEnforcement struct {
	Available          bool   `json:"available"`
	Registration       string `json:"registration"`
	Status             string `json:"status"`
	Revision           uint64 `json:"revision"`
	DeploymentRevision uint64 `json:"deployment_revision"`
	MemberCount        int    `json:"member_count"`
	ReadyCount         int    `json:"ready_count"`
	AckCount           int    `json:"ack_count"`
	LatestReadyAt      string `json:"latest_ready_at"`
	LatestAckAt        string `json:"latest_ack_at"`
	SnapshotCurrent    bool   `json:"snapshot_current"`
}

type appState struct {
	Code        string          `json:"code"`
	Status      string          `json:"status"`
	ReasonCode  string          `json:"reason_code"`
	NotBefore   int64           `json:"not_before,omitempty"`
	ExpiresAt   int64           `json:"expires_at,omitempty"`
	Kind        string          `json:"kind,omitempty"`
	Enforcement *appEnforcement `json:"enforcement"`
}

// licenseReasonCode 把应用授权状态映射为稳定的原因码。VALID 没有原因码，
// 其余状态与原因码一一对应；校验失败对外仍报 VALIDATION_ERROR 状态，
// 原因码用 STATE_INVALID 表示持久状态校验失败。
func licenseReasonCode(status string) string {
	switch status {
	case "NOT_EFFECTIVE", "EXPIRED", "NO_LICENSE", "NOT_PURCHASED", "CLOCK_ABNORMAL":
		return status
	case "VALIDATION_ERROR":
		return "STATE_INVALID"
	}
	return ""
}

// runtimeEnforcement 汇总单个应用的协调状态。任何读取失败都收敛为可解析的
// 聚合结果，不返回错误：单个应用不可读不得影响其他应用或整体响应。
func (h *Handler) runtimeEnforcement(ctx context.Context, application string) *appEnforcement {
	if h.runtime == nil {
		return nil
	}
	out := appEnforcement{Available: true, Registration: "UNREGISTERED", Status: "UNREGISTERED"}
	status, e := h.runtime.Status(ctx, application)
	switch {
	case errors.Is(e, coordination.ErrNotReady):
		return &out
	case e != nil:
		return &appEnforcement{Available: false, Registration: "UNKNOWN", Status: "UNKNOWN"}
	}
	out.Registration = "REGISTERED"
	out.Status = string(status.State)
	if out.Status == "" {
		out.Status = "UNKNOWN"
	}
	out.Revision = status.Revision
	out.DeploymentRevision = status.DeploymentRevision
	out.SnapshotCurrent = status.SnapshotCurrent
	var readyAt, ackAt time.Time
	for _, member := range status.Members {
		// 退役成员不参与任何计数与时间聚合，避免历史成员污染当前覆盖度。
		if member.RetiredAt != nil {
			continue
		}
		out.MemberCount++
		if member.ReadyAt != nil {
			out.ReadyCount++
			if member.ReadyAt.After(readyAt) {
				readyAt = *member.ReadyAt
			}
		}
		if member.AckAt != nil {
			if member.AckAt.After(ackAt) {
				ackAt = *member.AckAt
			}
			if member.AckRevision == status.Revision {
				out.AckCount++
			}
		}
	}
	// 零值时间输出空串，避免出现 0001-01-01 这类假时间。
	if !readyAt.IsZero() {
		out.LatestReadyAt = readyAt.UTC().Format(time.RFC3339)
	}
	if !ackAt.IsZero() {
		out.LatestAckAt = ackAt.UTC().Format(time.RFC3339)
	}
	return &out
}

// Status 返回部署实例与各应用的授权事实，并附带运行强制执行聚合，
// 使前端无需逐应用回查协调器（避免 N+1）。
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	st, e := h.service.Read(r.Context())
	initialized := !errors.Is(e, domain.ErrNotInitialized)
	if e != nil && !errors.Is(e, domain.ErrNotInitialized) && !errors.Is(e, domain.ErrClock) && !errors.Is(e, domain.ErrCorrupt) {
		writeError(w, r, e)
		return
	}
	now := time.Now().UTC()
	items := []appState{}
	// runtimeReads 统计本次请求内真正读到协调状态的应用数（含“未登记”），
	// 用于把顶层 runtime_enforcement 从“是否注入”修正为“实际可用性”。
	runtimeReads := 0
	for _, code := range []string{"contract_management", "customer_and_opportunity", "project_management", "settlement", "data_analysis", "customer_portal"} {
		item := appState{Code: code, Status: "NO_LICENSE"}
		if st.Current != nil {
			item.Status = "NOT_PURCHASED"
			for _, a := range st.Current.Applications {
				if a.Code == code {
					item.NotBefore = a.NotBefore
					item.ExpiresAt = a.ExpiresAt
					item.Kind = a.Kind
					item.Status = "VALID"
					if now.Unix() < st.Current.NotBefore || now.Unix() < a.NotBefore {
						item.Status = "NOT_EFFECTIVE"
					} else if now.Unix() >= a.ExpiresAt {
						item.Status = "EXPIRED"
					}
				}
			}
		}
		if errors.Is(e, domain.ErrClock) {
			item.Status = "CLOCK_ABNORMAL"
		} else if errors.Is(e, domain.ErrCorrupt) {
			item.Status = "VALIDATION_ERROR"
		}
		item.ReasonCode = licenseReasonCode(item.Status)
		item.Enforcement = h.runtimeEnforcement(r.Context(), code)
		if item.Enforcement != nil && item.Enforcement.Available {
			runtimeReads++
		}
		items = append(items, item)
	}
	// 顶层只区分 AVAILABLE/UNAVAILABLE（前端契约）：协调器未注入，或已注入但
	// 本次请求没有任何应用成功读取，均视为运行强制执行不可用。
	runtimeEnforcement := "UNAVAILABLE"
	if h.runtime != nil && runtimeReads > 0 {
		runtimeEnforcement = "AVAILABLE"
	}
	httpresponse.WriteSuccess(w, r, 200, "商业授权状态查询成功", map[string]any{"initialized": initialized, "deployment": st, "systems": items, "server_time": now.Format(time.RFC3339), "configured_environment": h.environment, "runtime_enforcement": runtimeEnforcement, "runtime_enforcement_source": "per_application_coordination"})
}

// Artifact 导出已持久化许可证产物。厂商续签只能由上一版许可证推导版本号
// （version = 上一版 + 1），因此原始文件丢失时必须以库中保留的最高版本作为续签基准。
func (h *Handler) Artifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	export, e := h.service.Export(r.Context())
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "许可证产物查询成功", export)
}
func (h *Handler) Initialize(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r, true)
	if !ok {
		return
	}
	var in application.InitializeInput
	if !decode(w, r, &in) {
		return
	}
	if in.Environment != h.environment {
		writeError(w, r, domain.ErrInvalid)
		return
	}
	st, e := h.service.Initialize(r.Context(), in, actor)
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "部署实例初始化成功", st)
}
func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	v, e := h.service.Request(r.Context())
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "签发申请查询成功", v)
}
func (h *Handler) Preview(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, true); !ok {
		return
	}
	var in struct {
		RawJWS string `json:"raw_jws"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, e := h.service.Preview(r.Context(), in.RawJWS)
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "许可证验签预览成功", v)
}
func (h *Handler) Commit(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r, true)
	if !ok {
		return
	}
	var in application.CommitInput
	if !decode(w, r, &in) {
		return
	}
	v, e := h.service.Commit(r.Context(), in, actor)
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "许可证导入成功", v)
}
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	page, size := 1, 20
	var e error
	if v := r.URL.Query().Get("page"); v != "" {
		page, e = strconv.Atoi(v)
		if e != nil {
			writeError(w, r, domain.ErrInvalid)
			return
		}
	}
	if v := r.URL.Query().Get("page_size"); v != "" {
		size, e = strconv.Atoi(v)
		if e != nil {
			writeError(w, r, domain.ErrInvalid)
			return
		}
	}
	out, e := h.service.Events(r.Context(), page, size)
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "授权事件查询成功", out)
}
func (h *Handler) RestoreClock(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r, true)
	if !ok {
		return
	}
	var in struct {
		RawJWS string `json:"raw_jws"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, e := h.service.RestoreClock(r.Context(), in.RawJWS, actor)
	if e != nil {
		writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "时间恢复凭据已消费", out)
}
