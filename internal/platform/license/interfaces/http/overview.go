package licensehttp

import (
	"context"
	"errors"
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
	Status    string `json:"status"`
	Revision  uint64 `json:"revision"`
	Version   uint64 `json:"version"`
	NotBefore int64  `json:"not_before"`
	ExpiresAt int64  `json:"expires_at"`
}

type overviewRegistration struct {
	Status         string `json:"status"`
	MemberCount    int    `json:"member_count"`
	CatalogVersion int    `json:"catalog_version"`
}

type overviewMember struct {
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

// ConfigureOverview 挂接许可证状态读取与普通运行观察。environment 与正式证据共用，
// 但观察本身不依赖批准材料；未配置观察时页面显示 UNKNOWN，不得解释为已停止。
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
	if out.License.Status != "VALID" {
		out.AllowedActions = append(out.AllowedActions, "IMPORT_LICENSE")
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

func readOverviewLicense(ctx context.Context, reader licenseStateReader, application string, now time.Time) (overviewLicense, error) {
	state, err := reader.Read(ctx)
	switch {
	case errors.Is(err, domain.ErrNotInitialized):
		return overviewLicense{Status: "NO_LICENSE"}, nil
	case errors.Is(err, domain.ErrClock), errors.Is(err, domain.ErrCorrupt):
		return overviewLicense{Status: "INVALID"}, nil
	case err != nil:
		return overviewLicense{}, err
	}
	out := overviewLicense{Revision: state.Revision, Version: state.CurrentVersion}
	if state.Current == nil {
		out.Status = "NO_LICENSE"
		return out, nil
	}
	out.Version = state.Current.Version
	out.NotBefore = state.Current.NotBefore
	for _, licensed := range state.Current.Applications {
		if licensed.Code != application {
			continue
		}
		out.NotBefore = licensed.NotBefore
		out.ExpiresAt = licensed.ExpiresAt
		if now.Unix() < state.Current.NotBefore || now.Unix() < licensed.NotBefore {
			out.Status = "NOT_EFFECTIVE"
		} else if now.Unix() >= licensed.ExpiresAt {
			out.Status = "EXPIRED"
		} else {
			out.Status = "VALID"
		}
		return out, nil
	}
	out.Status = "NOT_PURCHASED"
	return out, nil
}
