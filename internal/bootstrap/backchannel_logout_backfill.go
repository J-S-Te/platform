package bootstrap

import (
	"context"
	"errors"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	"gorm.io/gorm"
)

// subsystemAccessBackchannelLogoutPort 把既有 BackchannelLogoutURIRepository 适配为更新路径
// 的存量回填端口（BIZ-XEVT2-07）。写侧复用 UpsertBackchannelLogoutURIWithin 的
// ON DUPLICATE KEY UPDATE 幂等语义与同一套 HTTPS 校验，应用层不直接接触登记表。
type subsystemAccessBackchannelLogoutPort struct {
	repository *applicationregistryinfrastructure.BackchannelLogoutURIRepository
}

// ReadBackchannelLogoutURI 读取已登记端点；未登记（含客户端不在本租户/非 ACTIVE）返回
// found=false 而不是把 gorm.ErrRecordNotFound 逃成更新失败。
func (port subsystemAccessBackchannelLogoutPort) ReadBackchannelLogoutURI(ctx context.Context, tenantID, oauthClientID string) (string, bool, error) {
	if port.repository == nil {
		return "", false, errors.New("back-channel logout URI repository is not configured")
	}
	uri, err := port.repository.Get(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return uri, true, nil
}

// UpsertBackchannelLogoutURI 幂等登记端点；租户边界与协议校验都由仓储执行，此处不复制
// 也不放宽规则。
func (port subsystemAccessBackchannelLogoutPort) UpsertBackchannelLogoutURI(ctx context.Context, tenantID, operatorID, oauthClientID, uri string) error {
	if port.repository == nil {
		return errors.New("back-channel logout URI repository is not configured")
	}
	return port.repository.Set(ctx, applicationregistryapplication.BackchannelLogoutURIUpdate{
		TenantID:      strings.TrimSpace(tenantID),
		OAuthClientID: strings.TrimSpace(oauthClientID),
		OperatorID:    strings.TrimSpace(operatorID),
		URI:           strings.TrimSpace(uri),
	}, time.Now().UTC())
}

var _ applicationregistryapplication.SubsystemUpdateBackchannelLogoutPort = subsystemAccessBackchannelLogoutPort{}
