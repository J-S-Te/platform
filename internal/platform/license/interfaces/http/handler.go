package licensehttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	core "github.com/J-S-Te/license-core"
)

type Handler struct {
	service     *application.Service
	environment string
}

func NewHandler(service *application.Service, environment string) (*Handler, error) {
	if service == nil || environment == "" {
		return nil, domain.ErrInvalid
	}
	return &Handler{service, environment}, nil
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

type appState struct {
	Code      string `json:"code"`
	Status    string `json:"status"`
	NotBefore int64  `json:"not_before,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

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
		items = append(items, item)
	}
	httpresponse.WriteSuccess(w, r, 200, "商业授权状态查询成功", map[string]any{"initialized": initialized, "deployment": st, "systems": items, "server_time": now.Format(time.RFC3339), "configured_environment": h.environment, "runtime_enforcement": "NOT_CONNECTED"})
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
