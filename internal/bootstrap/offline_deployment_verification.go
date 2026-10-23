package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// OfflineDeploymentVerification 只承载已批准的控制面状态；
// 绝不用进程健康代替成功的受管部署或 ACK。
type OfflineDeploymentVerification struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	Status      string `json:"status"`
	Manifest    string `json:"manifest_checksum"`
}

// VerifyDeployment 读取指定应用在 prod 环境的受控部署状态并返回其清单校验和；
// 仅当应用在离线交付目标清单内时执行。
func (r *OfflineDeliveryRunner) VerifyDeployment(ctx context.Context, app string) (OfflineDeploymentVerification, error) {
	out := OfflineDeploymentVerification{Application: app, Environment: "prod"}
	if _, ok := offlineTargets[app]; !ok {
		return out, errors.New("unsupported deployment application")
	}
	p, err := r.principal(ctx)
	if err != nil {
		return out, err
	}
	data, _, err := r.invoke(ctx, p, http.MethodGet, "/api/v1/subsystem-status?application_code="+url.QueryEscape(app)+"&environment=prod", nil, r.onboarding.GetSubsystemStatus)
	if err != nil {
		return out, err
	}
	var state struct {
		Application string     `json:"application_code"`
		Environment string     `json:"environment"`
		Status      string     `json:"status"`
		Desired     string     `json:"desired_manifest_checksum"`
		Applied     string     `json:"applied_manifest_checksum"`
		Drift       string     `json:"manifest_drift_status"`
		VerifiedAt  *time.Time `json:"manifest_last_verified_at"`
	}
	if json.Unmarshal(data, &state) != nil || state.Application != app || state.Environment != "prod" || state.Status != registry.SubsystemDeploymentStatusReady || state.Desired == "" || state.Desired != state.Applied || state.Drift != "IN_SYNC" || state.VerifiedAt == nil || state.VerifiedAt.IsZero() || state.VerifiedAt.After(time.Now().Add(time.Minute)) {
		return out, errors.New("controlled deployment is not READY with a current verified manifest")
	}
	out.Status, out.Manifest = state.Status, state.Applied
	return out, nil
}
