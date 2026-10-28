package licensehttp

import "time"

const activationStallWindow = 5 * time.Minute

type overviewActivation struct {
	CanActivate       bool       `json:"can_activate"`
	Status            string     `json:"status"`
	TotalCount        int        `json:"total_count"`
	ReadyCount        int        `json:"ready_count"`
	AcknowledgedCount int        `json:"acknowledged_count"`
	WaitingServiceIDs []string   `json:"waiting_service_ids"`
	WaitingSince      *time.Time `json:"waiting_since"`
	WaitSeconds       int64      `json:"wait_seconds"`
	BlockingReasons   []string   `json:"blocking_reasons"`
}

// overviewFresh 用途：使用与协调器相同的60秒新鲜窗口，不把未来时间计为就绪。
// 参数：at 为成员上报时间，now 为服务端当前时间。
// 返回值：返回该信号是否仍新鲜。
func overviewFresh(at *time.Time, now time.Time) bool {
	return at != nil && !at.IsZero() && !at.After(now) && now.Sub(*at) <= 60*time.Second
}

// overviewHasPermission 用途：限制概览中跨模块办理入口的可见性。
// 参数：permissions 为可信主体权限，permission 为所需权限。
// 返回值：返回是否拥有精确权限。
func overviewHasPermission(permissions []string, permission string) bool {
	for _, code := range permissions {
		if code == permission {
			return true
		}
	}
	return false
}

// projectOverviewActivation 用途：投影激活资格与等待诊断，不改变登记、业务权限或执行状态。
// 参数：out 为概览事实，updatedAt 为当前执行修订时间，now 为服务端时间。
// 返回值：返回成员进度、资格及阻塞原因；超时仅告警，不自动回滚或代发确认。
func projectOverviewActivation(out licenseOverview, updatedAt, now time.Time) overviewActivation {
	a := overviewActivation{Status: "NOT_READY", TotalCount: len(out.Enforcement.Members), WaitingServiceIDs: []string{}, BlockingReasons: []string{}}
	for _, m := range out.Enforcement.Members {
		if m.ReadyFresh {
			a.ReadyCount++
		}
		if m.AckCurrent {
			a.AcknowledgedCount++
		}
		if !m.AckCurrent {
			a.WaitingServiceIDs = append(a.WaitingServiceIDs, m.ServiceID)
		}
	}
	if out.Registration.Status != "REGISTERED" {
		a.BlockingReasons = append(a.BlockingReasons, "RUNTIME_NOT_REGISTERED")
	}
	if out.License.Status != "VALID" {
		a.BlockingReasons = append(a.BlockingReasons, "LICENSE_NOT_VALID")
	}
	if a.TotalCount == 0 {
		a.BlockingReasons = append(a.BlockingReasons, "RUNTIME_MEMBERS_MISSING")
	} else if a.ReadyCount != a.TotalCount {
		a.BlockingReasons = append(a.BlockingReasons, "RUNTIME_READY_STALE")
	}
	switch out.Enforcement.Status {
	case "ENFORCED":
		a.Status = "ENFORCED"
	case "APPLYING":
		a.Status = "APPLYING"
		if !updatedAt.IsZero() && !updatedAt.After(now) {
			at := updatedAt.UTC()
			a.WaitingSince = &at
			a.WaitSeconds = int64(now.Sub(at).Seconds())
			if now.Sub(at) >= activationStallWindow {
				a.Status = "STALLED"
			}
		}
	case "PENDING_ENFORCEMENT":
		a.CanActivate = len(a.BlockingReasons) == 0
		if a.CanActivate {
			a.Status = "READY"
		}
	default:
		a.Status = "UNAVAILABLE"
	}
	return a
}
