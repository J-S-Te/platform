package licensehttp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type runtimeCoordinator interface {
	Ready(context.Context, string, coordination.ReadyInput) error
	Snapshot(context.Context, string) (coordination.SnapshotOutput, error)
	Ack(context.Context, string, coordination.AckInput) error
	Status(context.Context, string) (coordination.Status, error)
	BeginActivation(context.Context, string, uint64, domain.Actor) error
}
type EvidenceSource interface {
	CollectLicenseEvidence(context.Context, string, string) (evidence.Report, error)
}
type installationEvidenceSource interface {
	CollectInstallationLicenseEvidence(context.Context) (evidence.Report, error)
}
type RuntimeHandler struct {
	service     runtimeCoordinator
	evidence    EvidenceSource
	environment string
}

func NewRuntimeHandler(service runtimeCoordinator) (*RuntimeHandler, error) {
	if service == nil {
		return nil, domain.ErrInvalid
	}
	return &RuntimeHandler{service: service}, nil
}

// ConfigureEvidence attaches the already constrained deployment Agent, never a
// browser-provided collector configuration or Docker socket.
func (h *RuntimeHandler) ConfigureEvidence(source EvidenceSource, environment string) {
	h.evidence = source
	h.environment = environment
}
func (h *RuntimeHandler) Evidence(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	if h.evidence == nil || h.environment == "" {
		httpresponse.WriteError(w, r, 503, httperror.New("LICENSE_EVIDENCE_UNSUPPORTED", "尚未配置受控部署证据来源，不能授予迁移资格", nil))
		return
	}
	report, err := h.evidence.CollectLicenseEvidence(r.Context(), applicationPath(r), h.environment)
	if err != nil {
		// Agent errors can include host paths. Public diagnostics deliberately
		// retain a fixed code rather than exposing the command/error text.
		httpresponse.WriteError(w, r, 503, httperror.New("LICENSE_EVIDENCE_UNAVAILABLE", "部署证据未获批准或无法完整采集，请检查 Agent 的批准发行清单", nil))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(w, r, 200, "部署证据核验结果；不代表获得存量资格", report)
}
func (h *RuntimeHandler) InstallationEvidence(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	source, ok := h.evidence.(installationEvidenceSource)
	if !ok {
		httpresponse.WriteError(w, r, 503, httperror.New("LICENSE_EVIDENCE_UNSUPPORTED", "尚未配置受控安装边界，不能授予迁移资格", nil))
		return
	}
	report, err := source.CollectInstallationLicenseEvidence(r.Context())
	if err != nil {
		httpresponse.WriteError(w, r, 503, httperror.New("LICENSE_EVIDENCE_UNAVAILABLE", "安装证据未获批准或无法完整采集，请检查 Agent 的批准发行清单", nil))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(w, r, 200, "完整安装证据核验结果；不代表获得存量资格", report)
}
func runtimeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, coordination.ErrForbidden):
		httpresponse.WriteError(w, r, 403, httperror.Forbidden)
	case errors.Is(err, coordination.ErrInvalid):
		writeError(w, r, domain.ErrInvalid)
	case errors.Is(err, coordination.ErrConflict):
		writeError(w, r, domain.ErrConflict)
	case errors.Is(err, coordination.ErrNotReady):
		httpresponse.WriteError(w, r, 409, httperror.New("LICENSE_RUNTIME_NOT_READY", "执行组件尚未完成登记、覆盖核验或就绪确认", nil))
	default:
		writeError(w, r, err)
	}
}
func machine(w http.ResponseWriter, r *http.Request) bool {
	p, ok := appctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return false
	}
	if !p.HasScope("license.runtime") {
		httpresponse.WriteError(w, r, 403, httperror.Forbidden)
		return false
	}
	return true
}

// Route tails are fixed by registration. The coordinator rechecks the service
// ID against the authenticated OAuth client, application and environment.
func runtimeServiceID(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 7 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "internal" || parts[3] != "licenses" || parts[4] != "runtime" {
		return ""
	}
	return parts[5]
}
func (h *RuntimeHandler) Ready(w http.ResponseWriter, r *http.Request) {
	if !machine(w, r) {
		return
	}
	var in coordination.ReadyInput
	if !decode(w, r, &in) {
		return
	}
	if err := h.service.Ready(r.Context(), runtimeServiceID(r), in); err != nil {
		runtimeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "执行组件已确认就绪", map[string]bool{"ready": true})
}
func (h *RuntimeHandler) Snapshot(w http.ResponseWriter, r *http.Request) {
	if !machine(w, r) {
		return
	}
	out, err := h.service.Snapshot(r.Context(), runtimeServiceID(r))
	if err != nil {
		runtimeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(w, r, 200, "签名执行快照", out)
}
func (h *RuntimeHandler) Ack(w http.ResponseWriter, r *http.Request) {
	if !machine(w, r) {
		return
	}
	var in coordination.AckInput
	if !decode(w, r, &in) {
		return
	}
	if err := h.service.Ack(r.Context(), runtimeServiceID(r), in); err != nil {
		runtimeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "执行快照确认已记录", map[string]bool{"acknowledged": true})
}
func applicationPath(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "licenses" || parts[3] != "enforcement" {
		return ""
	}
	return parts[4]
}
func (h *RuntimeHandler) Status(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	out, err := h.service.Status(r.Context(), applicationPath(r))
	if err != nil {
		runtimeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "授权执行状态", out)
}
func (h *RuntimeHandler) BeginActivation(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r, true)
	if !ok {
		return
	}
	var in struct {
		ExpectedRevision uint64 `json:"expected_revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := h.service.BeginActivation(r.Context(), applicationPath(r), in.ExpectedRevision, actor); err != nil {
		runtimeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "授权执行正在应用", map[string]bool{"applying": true})
}
