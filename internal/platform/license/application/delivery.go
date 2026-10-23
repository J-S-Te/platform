package application

import (
	"context"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	core "github.com/J-S-Te/license-core"
)

// DeliveryImportResult 表示交付包已持久化，不代表运行时已生效执行；协调器仍须完成组件
// 注册并收集各组件 ACK 后，授权才真正下发生效。
type DeliveryImportResult struct {
	State      domain.State `json:"state"`
	Status     string       `json:"status"`
	Idempotent bool         `json:"idempotent"`
}

// ImportDelivery 仅用于受控的离线部署引导。签名的客户安装身份可初始化空安装；
// 正常管理端 Initialize 仍由服务端生成。只有单调的期限延长可自动续期，
// 其余变更必须走 Preview/Commit 双阶段确认。
func (s *Service) ImportDelivery(ctx context.Context, raw, expectedEnvironment string, actor domain.Actor) (DeliveryImportResult, error) {
	var out DeliveryImportResult
	if !validActor(actor) || expectedEnvironment == "" {
		return out, domain.ErrInvalid
	}
	if len(s.trusted) == 0 {
		return out, domain.ErrTrustNotConfigured
	}
	l, err := core.Verify(raw, s.trusted)
	if err != nil {
		return out, err
	}
	if l.Environment != expectedEnvironment {
		return out, core.ErrDenied
	}
	digest, err := core.Digest(raw, s.trusted)
	if err != nil {
		return out, err
	}
	var blocked bool
	renewal := false
	err = s.repo.Lock(ctx, func(tx Transaction) error {
		now := s.now().UTC()
		d := tx.Deployment()
		if d != nil {
			if !bound(l, d) {
				return core.ErrDenied
			}
			st, e := s.state(tx)
			if e != nil {
				return e
			}
			if st.CurrentDigest != "" || st.PendingDigest != "" || st.HighestVersion != 0 {
				if digest != st.CurrentDigest && digest != st.PendingDigest {
					if st.Pending != nil || st.Current == nil || l.Version <= st.HighestVersion || !safeDeliveryRenewal(l, *st.Current) {
						return domain.ErrConfirmation
					}
					renewal = true
				} else {
					blocked, e = s.observe(tx, now)
					if e != nil {
						return e
					}
					if blocked {
						out.State, e = s.state(tx)
					} else {
						// 引导重试时挂起的交付包必须就地转为当前包，不能依赖无关的管理端 Read 请求来触发激活。
						out.State, e = s.activate(tx, st, now)
						out.State.HighestObservedAt = tx.Deployment().HighestObservedAt
					}
					out.Idempotent = true
					return e
				}
			}
			blocked, e = s.observe(tx, now)
			if e != nil || blocked {
				return e
			}
		}
		// 绝不自动安装部分已过期的授权集合。过期时间为排他的固定值，与客户实际部署日期无关。
		for _, app := range l.Applications {
			if now.Unix() >= app.ExpiresAt {
				return fmt.Errorf("%w: delivery entitlement %s has expired", core.ErrDenied, app.Code)
			}
		}
		if d == nil {
			d = &domain.Deployment{ID: 1, InstanceID: l.InstanceID, CustomerID: l.CustomerID, Environment: l.Environment, HighestObservedAt: now.Unix(), CreatedAt: now, UpdatedAt: now}
			if e := tx.SaveDeployment(d); e != nil {
				return e
			}
			if e := tx.AddEvent(event("DELIVERY_INITIALIZED", d, digest, l.Version, actor, now)); e != nil {
				return e
			}
		}
		if e := tx.AddArtifact(domain.Artifact{Digest: digest, RawJWS: raw, Version: l.Version, CreatedAt: now}); e != nil {
			return e
		}
		d.HighestVersion = l.Version
		d.Revision++
		d.UpdatedAt = now
		kind := "DELIVERY_IMPORTED_CURRENT"
		if renewal {
			kind = "DELIVERY_RENEWED_CURRENT"
		}
		if now.Unix() < l.NotBefore {
			d.PendingDigest = digest
			kind = "DELIVERY_IMPORTED_PENDING"
		} else {
			d.CurrentDigest = digest
		}
		if e := tx.SaveDeployment(d); e != nil {
			return e
		}
		if e := tx.AddEvent(event(kind, d, digest, l.Version, actor, now)); e != nil {
			return e
		}
		var e error
		out.State, e = s.state(tx)
		return e
	})
	if err != nil {
		return DeliveryImportResult{}, err
	}
	if blocked {
		return out, domain.ErrClock
	}
	out.Status = "IMPORTED"
	now := s.now().UTC().Unix()
	if now < l.NotBefore {
		out.Status = "PENDING_EFFECTIVE_DATE"
	}
	for _, app := range l.Applications {
		if now < app.NotBefore {
			out.Status = "PENDING_EFFECTIVE_DATE"
		}
	}
	for _, app := range l.Applications {
		if now >= app.ExpiresAt {
			out.Status = "EXPIRED"
		}
	}
	return out, nil
}

func safeDeliveryRenewal(next, current core.License) bool {
	if next.NotBefore > current.NotBefore || len(next.Applications) != len(current.Applications) {
		return false
	}
	previous := make(map[string]core.Application, len(current.Applications))
	for _, app := range current.Applications {
		previous[app.Code] = app
	}
	for _, app := range next.Applications {
		old, ok := previous[app.Code]
		if !ok || app.NotBefore > old.NotBefore || app.ExpiresAt < old.ExpiresAt || (old.Kind == "FULL" && app.Kind != "FULL") {
			return false
		}
	}
	return true
}
