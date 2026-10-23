package application

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/license-core/catalog"
)

// WaitForRuntimeConfirmation 只观察消费方上报的确认，绝不代其发送 READY/ACK、
// 激活购买或改变 enforcement 口径。最长等待 45 秒；超时或读取失败返回错误，
// 由调用方决定是否重试，不得据此回写审批状态。
func (s *RuntimeLicenseEnrollmentService) WaitForRuntimeConfirmation(ctx context.Context, app, env string, credentials []RuntimeLicenseCredential) error {
	reader, ok := s.registrar.(interface {
		Status(context.Context, string) (coordination.Status, error)
	})
	if !ok || len(credentials) == 0 {
		return errors.New("commercial runtime confirmation reader unavailable")
	}
	expected := map[string]RuntimeLicenseCredential{}
	for _, c := range credentials {
		if _, exists := expected[c.Approval.ServiceID]; exists || c.ClientID == "" {
			return ErrValidation
		}
		expected[c.Approval.ServiceID] = c
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		status, err := reader.Status(ctx, app)
		if err != nil {
			return errors.New("commercial runtime confirmation lookup failed")
		}
		if runtimeConfirmationCurrent(status, app, env, expected, time.Now().UTC()) {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("commercial runtime consumers have not confirmed the current revision")
		case <-timer.C:
		}
	}
}

// runtimeConfirmationCurrent 判定协调器快照是否构成对当前修订版的完整确认：
// 成员集合与预期凭据一一对应、READY/ACK 均落在当前修订版、且时间戳都在
// 1 分钟新鲜窗口内——窗口之外的确认视为过期，防止用陈旧状态冒充就绪。
func runtimeConfirmationCurrent(status coordination.Status, app, env string, expected map[string]RuntimeLicenseCredential, now time.Time) bool {
	if status.Application.Application != app || !status.SnapshotCurrent || status.Revision == 0 || len(status.Members) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, m := range status.Members {
		c, ok := expected[m.ServiceID]
		if !ok || seen[m.ServiceID] || m.Application != app || m.Environment != env || m.RetiredAt != nil || m.OAuthClientID != c.ClientID || m.ReadyAt == nil || m.AckAt == nil || m.IssuedRevision != status.Revision || m.AckRevision != status.Revision {
			return false
		}
		if c.RegistrationProtocol == catalog.RegistrationProtocolV2 {
			if m.RegistrationProtocol != catalog.RegistrationProtocolV2 || m.CatalogVersion != c.CatalogVersion || m.CatalogVersion != status.CatalogVersion {
				return false
			}
		} else if m.ImageDigest != c.Approval.ImageDigest || m.CoverageDigest != c.Approval.CoverageDigest {
			return false
		}
		if now.Sub(*m.ReadyAt) > time.Minute || now.Sub(*m.AckAt) > time.Minute || m.ReadyAt.After(now.Add(time.Second)) || m.AckAt.After(now.Add(time.Second)) {
			return false
		}
		seen[m.ServiceID] = true
	}
	return true
}
