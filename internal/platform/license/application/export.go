package application

import (
	"context"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
)

// Export 返回当前与待生效许可证的原始产物，用于许可证文件丢失或换人维护后的续签。
// 厂商工具只能由上一版许可证推导版本号，因此丢失原始文件时无法凭空签发更高版本；
// 平台数据库中的 license_artifact 保留每一版原始 JWS，是受控的恢复来源。
func (s *Service) Export(ctx context.Context) (domain.LicenseExport, error) {
	var out domain.LicenseExport
	err := s.repo.Lock(ctx, func(tx Transaction) error {
		st, e := s.state(tx)
		if e != nil {
			return e
		}
		out.InstanceID = st.InstanceID
		out.Environment = st.Environment
		out.HighestVersion = st.HighestVersion
		if st.CurrentDigest != "" {
			a, e := tx.Artifact(st.CurrentDigest)
			if e != nil {
				return e
			}
			out.Current = &domain.ExportedLicense{Version: a.Version, Digest: a.Digest, RawJWS: a.RawJWS, CreatedAt: a.CreatedAt}
		}
		if st.PendingDigest != "" {
			a, e := tx.Artifact(st.PendingDigest)
			if e != nil {
				return e
			}
			out.Pending = &domain.ExportedLicense{Version: a.Version, Digest: a.Digest, RawJWS: a.RawJWS, CreatedAt: a.CreatedAt}
		}
		return nil
	})
	return out, err
}
