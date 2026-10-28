package licensehttp

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	core "github.com/J-S-Te/license-core"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// licenseStateReader 是 overview 需要的许可证域状态只读入口。
type licenseStateReader interface {
	Read(context.Context) (domain.State, error)
}

// RuntimeObserver 是普通运行观察的窄接口；不得复用需要批准材料的正式证据采集器。
type RuntimeObserver interface {
	ObserveLicenseRuntime(ctx context.Context, application, environment string) (licenseevidence.RuntimeObservation, error)
}

type overviewLicense struct {
	Initialized bool   `json:"initialized"`
	Status      string `json:"status"`
	Revision    uint64 `json:"revision"`
	Version     uint64 `json:"version"`
	NotBefore   int64  `json:"not_before"`
	ExpiresAt   int64  `json:"expires_at"`
}

type overviewRegistration struct {
	Status         string `json:"status"`
	MemberCount    int    `json:"member_count"`
	CatalogVersion int    `json:"catalog_version"`
}

type overviewMember struct {
	ReadyFresh     bool       `json:"ready_fresh"`
	AckCurrent     bool       `json:"ack_current"`
	ServiceID      string     `json:"service_id"`
	ReadyAt        *time.Time `json:"ready_at"`
	IssuedRevision uint64     `json:"issued_revision"`
	AckRevision    uint64     `json:"ack_revision"`
	AckAt          *time.Time `json:"ack_at"`
}

type overviewEnforcement struct {
	Status             string           `json:"status"`
	Revision           uint64           `json:"revision"`
	DeploymentRevision uint64           `json:"deployment_revision"`
	SnapshotCurrent    bool             `json:"snapshot_current"`
	Members            []overviewMember `json:"members"`
}

type licenseOverview struct {
	Activation           overviewActivation                 `json:"activation"`
	Application          string                             `json:"application"`
	Environment          string                             `json:"environment"`
	License              overviewLicense                    `json:"license"`
	Runtime              licenseevidence.RuntimeObservation `json:"runtime"`
	Registration         overviewRegistration               `json:"registration"`
	Enforcement          overviewEnforcement                `json:"enforcement"`
	PreparationOperation any                                `json:"preparation_operation"`
	BlockingReasons      []string                           `json:"blocking_reasons"`
	AllowedActions       []string                           `json:"allowed_actions"`
	CheckedAt            time.Time                          `json:"checked_at"`
}

// ConfigureOverview 用途：挂接许可证事实读取和部署环境。
// 参数：state 为可信状态读取器，environment 为已配置环境。
// 返回值：无。
func (h *RuntimeHandler) ConfigureOverview(state licenseStateReader, environment string) {
	h.state = state
	if environment != "" {
		h.environment = environment
	}
}

// ConfigureObserver 挂接只读运行观察；nil 表示尚未接入。
func (h *RuntimeHandler) ConfigureObserver(observer RuntimeObserver) {
	h.observer = observer
}

// Overview 用途：分别读取许可证、登记、运行及激活事实。
// 参数：w 为响应，r 为带权限的读取请求。
// 返回值：无；通过响应返回事实，存储失败不伪造状态。
func (h *RuntimeHandler) Overview(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r, false); !ok {
		return
	}
	if h.state == nil {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("LICENSE_OVERVIEW_UNSUPPORTED", "尚未配置许可证状态读取入口", nil))
		return
	}
	application := applicationPath(r)
	if application == "" {
		writeError(w, r, domain.ErrInvalid)
		return
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	checkedAt := now().UTC()
	out := licenseOverview{
		Application: application,
		Environment: h.environment,
		Runtime: licenseevidence.RuntimeObservation{
			Status: "UNKNOWN", Available: false, Message: "尚未接入运行观察", CheckedAt: checkedAt,
		},
		Registration:         overviewRegistration{Status: "UNKNOWN"},
		Enforcement:          overviewEnforcement{Status: "UNKNOWN", Members: []overviewMember{}},
		PreparationOperation: nil,
		BlockingReasons:      []string{},
		AllowedActions:       []string{},
		CheckedAt:            checkedAt,
	}
	status, err := h.service.Status(r.Context(), application)
	switch {
	case err == nil:
		out.Registration = overviewRegistration{
			Status:         "REGISTERED",
			MemberCount:    len(status.Members),
			CatalogVersion: status.CatalogVersion,
		}
		members := make([]overviewMember, 0, len(status.Members))
		for _, member := range status.Members {
			members = append(members, overviewMember{
				ServiceID: member.ServiceID, ReadyAt: member.ReadyAt,
				IssuedRevision: member.IssuedRevision, AckRevision: member.AckRevision, AckAt: member.AckAt,
				ReadyFresh: overviewFresh(member.ReadyAt, checkedAt),
				AckCurrent: status.SnapshotCurrent && member.IssuedRevision == status.Revision && member.AckRevision == status.Revision && overviewFresh(member.ReadyAt, checkedAt) && overviewFresh(member.AckAt, checkedAt),
			})
		}
		enforcementState := string(status.State)
		if enforcementState == "" {
			enforcementState = "UNKNOWN"
		}
		out.Enforcement = overviewEnforcement{
			Status: enforcementState, Revision: status.Revision,
			DeploymentRevision: status.DeploymentRevision, SnapshotCurrent: status.SnapshotCurrent,
			Members: members,
		}
	case errors.Is(err, coordination.ErrNotReady):
		out.Registration = overviewRegistration{Status: "UNREGISTERED"}
		out.Enforcement = overviewEnforcement{Status: "UNREGISTERED", Members: []overviewMember{}}
	case errors.Is(err, domain.ErrNotInitialized), errors.Is(err, domain.ErrClock), errors.Is(err, domain.ErrCorrupt):
		// 不能从协调器暂不可读反推未登记；许可证事实仍可准确展示初始化/时钟状态。
	case errors.Is(err, coordination.ErrInvalid):
		writeError(w, r, domain.ErrInvalid)
		return
	default:
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("LICENSE_OVERVIEW_UNAVAILABLE", "运行时授权状态暂时无法读取", nil))
		return
	}

	licenseFact, licenseErr := readOverviewLicense(r.Context(), h.state, application, now().UTC())
	if licenseErr != nil {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("LICENSE_OVERVIEW_UNAVAILABLE", "授权状态暂时无法读取", nil))
		return
	}
	out.License = licenseFact
	if status.DeploymentRevision != licenseFact.Revision {
		out.Enforcement.SnapshotCurrent = false
		for i := range out.Enforcement.Members {
			out.Enforcement.Members[i].AckCurrent = false
		}
	}
	out.Activation = projectOverviewActivation(out, status.UpdatedAt, checkedAt)
	if out.License.Status == "INVALID" {
		out.BlockingReasons = append(out.BlockingReasons, "LICENSE_STATE_INVALID")
	}
	switch out.License.Status {
	case "NO_LICENSE", "NOT_PURCHASED":
		out.BlockingReasons = append(out.BlockingReasons, "LICENSE_MISSING")
	case "NOT_EFFECTIVE":
		out.BlockingReasons = append(out.BlockingReasons, "LICENSE_NOT_EFFECTIVE")
	case "EXPIRED":
		out.BlockingReasons = append(out.BlockingReasons, "LICENSE_EXPIRED")
	}
	if out.Registration.Status == "UNREGISTERED" {
		out.BlockingReasons = append(out.BlockingReasons, "RUNTIME_NOT_REGISTERED")
	}
	if out.License.Status == "NO_LICENSE" || out.License.Status == "NOT_PURCHASED" || out.License.Status == "EXPIRED" {
		out.AllowedActions = append(out.AllowedActions, "IMPORT_LICENSE")
	}
	if p, ok := authctx.PrincipalFromContext(r.Context()); ok && overviewHasPermission(p.PermissionCodes, "platform:application:read") &&
		(out.Registration.Status == "UNREGISTERED" || out.Activation.Status == "STALLED") {
		out.AllowedActions = append(out.AllowedActions, "OPEN_ACCESS_WIZARD")
	}
	if h.observer != nil {
		observation, observeErr := h.observer.ObserveLicenseRuntime(r.Context(), application, h.environment)
		if observeErr != nil {
			out.Runtime = licenseevidence.RuntimeObservation{
				Status: "UNKNOWN", Available: false, Message: "运行状态暂时无法确认", CheckedAt: checkedAt,
			}
		} else {
			if observation.CheckedAt.IsZero() {
				observation.CheckedAt = checkedAt
			}
			out.Runtime = observation
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(w, r, http.StatusOK, "运行时授权概览", out)
}

// readOverviewLicense 用途：按生效时间和最高观测时间投影许可证事实。
// 参数：ctx 为上下文，reader 为可信读取器，application 为系统，now 为时间。
// 返回值：返回当前或待生效许可事实；存储读取失败返回错误。
func readOverviewLicense(ctx context.Context, reader licenseStateReader, application string, now time.Time) (overviewLicense, error) {
	state, err := reader.Read(ctx)
	switch {
	case errors.Is(err, domain.ErrNotInitialized):
		return overviewLicense{Status: "NO_LICENSE"}, nil
	case errors.Is(err, domain.ErrClock), errors.Is(err, domain.ErrCorrupt):
		return overviewLicense{Status: "INVALID", Initialized: true}, nil
	case err != nil:
		return overviewLicense{}, err
	}
	effective := now.Unix()
	if state.HighestObservedAt > effective {
		effective = state.HighestObservedAt
	}
	selected := state.Current
	if overviewApplicationLicense(selected, application) == nil && overviewApplicationLicense(state.Pending, application) != nil {
		selected = state.Pending
	}
	out := overviewLicense{Initialized: true, Revision: state.Revision, Version: state.CurrentVersion}
	if selected == nil {
		out.Status = "NO_LICENSE"
		return out, nil
	}
	out.Version, out.NotBefore = selected.Version, selected.NotBefore
	licensed := overviewApplicationLicense(selected, application)
	if licensed == nil {
		out.Status = "NOT_PURCHASED"
		return out, nil
	}
	out.NotBefore, out.ExpiresAt = licensed.NotBefore, licensed.ExpiresAt
	if selected.NotBefore > out.NotBefore {
		out.NotBefore = selected.NotBefore
	}
	switch {
	case effective < out.NotBefore:
		out.Status = "NOT_EFFECTIVE"
	case effective >= licensed.ExpiresAt:
		out.Status = "EXPIRED"
	default:
		out.Status = "VALID"
	}
	return out, nil
}

// overviewApplicationLicense 用途：按系统编码查找已验签许可证的权益。
// 参数：license 为已验签许可，application 为系统编码。
// 返回值：返回匹配权益；不存在时返回 nil。
func overviewApplicationLicense(license *core.License, application string) *core.Application {
	if license == nil {
		return nil
	}
	for i := range license.Applications {
		if license.Applications[i].Code == application {
			return &license.Applications[i]
		}
	}
	return nil
}
