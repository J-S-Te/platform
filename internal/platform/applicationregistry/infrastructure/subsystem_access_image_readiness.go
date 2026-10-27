package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// SubsystemAccessImageReader 是「读取批准清单镜像本机存在性」的窄接口（由部署 Agent 客户端实现）。
type SubsystemAccessImageReader interface {
	ReadSubsystemAccessImages(ctx context.Context, applicationCode, environment string) ([]application.SubsystemImageReadiness, bool, error)
}

// SubsystemAccessImageReadinessAdapter 把 Agent 上报的镜像明细折算为就绪三态：
// 未上报（旧 Agent）→ verified=false（未验证）；全部存在 → 就绪；有缺失 → 不就绪（上层据此报缺项 IMAGES_MISSING）。
type SubsystemAccessImageReadinessAdapter struct {
	reader SubsystemAccessImageReader
}

// NewSubsystemAccessImageReadinessAdapter 构造适配器。
func NewSubsystemAccessImageReadinessAdapter(reader SubsystemAccessImageReader) (*SubsystemAccessImageReadinessAdapter, error) {
	if reader == nil {
		return nil, errors.New("subsystem access image readiness adapter requires an image reader")
	}
	return &SubsystemAccessImageReadinessAdapter{reader: reader}, nil
}

// ReadSubsystemAccessImageReadiness 返回（是否就绪, 版本标记, 是否已确认, 错误）。
func (adapter *SubsystemAccessImageReadinessAdapter) ReadSubsystemAccessImageReadiness(ctx context.Context, _ string, applicationCode, environmentCode string) (bool, string, bool, error) {
	images, reported, err := adapter.reader.ReadSubsystemAccessImages(ctx, applicationCode, environmentCode)
	if err != nil {
		return false, "", false, err
	}
	if !reported || len(images) == 0 {
		// 旧版 Agent 未上报该字段：保持未验证。
		return false, "unreported", false, nil
	}
	missing := make([]string, 0, len(images))
	for _, image := range images {
		if !image.Present {
			missing = append(missing, strings.TrimSpace(image.ServiceID))
		}
	}
	if len(missing) > 0 {
		return false, "missing:" + strings.Join(missing, ","), true, nil
	}
	return true, fmt.Sprintf("images:%d", len(images)), true, nil
}
