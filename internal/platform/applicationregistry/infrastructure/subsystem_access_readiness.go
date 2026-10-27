package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

// SubsystemAccessImageReadinessReader 读取"所需镜像是否齐备"。该事实在部署 Agent 侧
// （本机镜像与批准固定记录），因此实现由组合根注入；未接线时报告为"未验证"而不是"就绪"。
type SubsystemAccessImageReadinessReader interface {
	// verified=false 表示无法确认（例如旧版 Agent 未上报该字段）：调用方必须按「未验证」处理，
	// 既不能当作就绪，也不能当作缺失。
	ReadSubsystemAccessImageReadiness(ctx context.Context, tenantID, applicationCode, environmentCode string) (ready bool, version string, verified bool, err error)
}

// SubsystemAccessAuthObservationReader 读取"认证观察期是否未结束"（Keycloak 切换/观察期）。
type SubsystemAccessAuthObservationReader interface {
	ReadSubsystemAccessAuthObservation(ctx context.Context, tenantID, applicationCode, environmentCode string) (pending bool, version string, err error)
}

// SubsystemAccessReadinessAdapter 组装就绪类事实：
//   - 运行批准：既有 RuntimeLicenseApprovalSource（服务端已有）；
//   - 商业授权：license_artifact 是否存在 + 该环境的 license_deployment 未被时钟阻断；
//   - 镜像 / 认证观察期：由注入的读取器提供，未接线时记入 Unverified；
//   - 历史演示与配置漂移：无读取器时同样记入 Unverified（不默认"无漂移"）。
//
// 任一必需来源失败即返回错误：不猜测、不默认就绪。
type SubsystemAccessConfigurationDriftReader interface {
	ReadSubsystemAccessConfigurationDrift(context.Context, string, string, string) (bool, []string, string, error)
}

type SubsystemAccessReadinessAdapter struct {
	configuration SubsystemAccessConfigurationDriftReader
	builtin       application.RuntimeComponentCatalogSource
	database      *gorm.DB
	approvals     application.RuntimeLicenseApprovalSource
	images        SubsystemAccessImageReadinessReader
	observation   SubsystemAccessAuthObservationReader
}

// NewSubsystemAccessReadinessAdapter 构造适配器；数据库与批准来源必需。
func NewSubsystemAccessReadinessAdapter(database *gorm.DB, approvals application.RuntimeLicenseApprovalSource, images SubsystemAccessImageReadinessReader, observation SubsystemAccessAuthObservationReader) (*SubsystemAccessReadinessAdapter, error) {
	if database == nil || approvals == nil {
		return nil, errors.New("subsystem access readiness adapter requires database and runtime approval source")
	}
	return &SubsystemAccessReadinessAdapter{database: database, approvals: approvals, images: images, observation: observation}, nil
}

// ReadSubsystemAccessReadiness 读取就绪事实。
func (adapter *SubsystemAccessReadinessAdapter) ReadSubsystemAccessReadiness(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessReadiness, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationCode = strings.TrimSpace(applicationCode)
	environmentCode = strings.ToLower(strings.TrimSpace(environmentCode))
	if applicationCode == "" || environmentCode == "" {
		return SubsystemAccessReadiness{}, fmt.Errorf("readiness lookup requires application and environment: %w", application.ErrSubsystemAccessInvalidRequest)
	}

	readiness := SubsystemAccessReadiness{}
	environments := subsystemAccessEnvironmentAliases(environmentCode)
	approvalVersion := ""
	if adapter.builtin != nil {
		var err error
		approvalVersion, err = builtinAccessCatalogVersion(ctx, adapter.builtin, applicationCode, environmentCode)
		if err != nil {
			return SubsystemAccessReadiness{}, fmt.Errorf("read builtin runtime components: %w", err)
		}
		readiness.ApprovalReady = true
	} else {
		var components []application.RuntimeLicenseApproval
		for _, candidate := range environments {
			approved, approvalErr := adapter.approvals.ApprovedRuntimeLicenseComponents(ctx, applicationCode, candidate)
			if approvalErr != nil {
				// 批准域的诊断（不可用 / 不匹配 / 启动校验失败 / 运行时不可用 / 未初始化）
				// 都是「未就绪」的正常结果，不是传输失败：记为未就绪并继续尝试其它别名，
				// 不能因此把整个预览打成 500。
				if application.RuntimeLicenseApprovalErrorCode(approvalErr) != "" {
					continue
				}
				return SubsystemAccessReadiness{}, fmt.Errorf("read approved runtime license components: %w", approvalErr)
			}
			components = append(components, approved...)
		}
		readiness.ApprovalReady = len(components) > 0
		approvalVersion = approvalComponentsVersion(components)

	}

	var artifacts int64
	if err := adapter.database.WithContext(ctx).Table("license_artifact").Count(&artifacts).Error; err != nil {
		return SubsystemAccessReadiness{}, fmt.Errorf("count license artifacts: %w", err)
	}
	var deployments int64
	if err := adapter.database.WithContext(ctx).Table("license_deployment").
		Where("environment IN ? AND clock_blocked = ?", environments, false).Count(&deployments).Error; err != nil {
		return SubsystemAccessReadiness{}, fmt.Errorf("count license deployments: %w", err)
	}
	readiness.LicenseReady = artifacts > 0 && deployments > 0
	licenseVersion := fmt.Sprintf("artifacts=%d;deployments=%d", artifacts, deployments)

	versions := []string{approvalVersion, licenseVersion}
	if adapter.images != nil {
		ready, version, verified, err := adapter.images.ReadSubsystemAccessImageReadiness(ctx, tenantID, applicationCode, environmentCode)
		if err != nil {
			return SubsystemAccessReadiness{}, fmt.Errorf("read image readiness: %w", err)
		}
		if !verified {
			// 无法确认（旧版 Agent 未上报等）：按未验证处理，不谎报就绪或缺失。
			readiness.Unverified = append(readiness.Unverified, "images")
		} else {
			readiness.ImagesReady = ready
			versions = append(versions, version)
		}
	} else {
		readiness.Unverified = append(readiness.Unverified, "images")
	}
	if adapter.observation != nil {
		pending, version, err := adapter.observation.ReadSubsystemAccessAuthObservation(ctx, tenantID, applicationCode, environmentCode)
		if err != nil {
			return SubsystemAccessReadiness{}, fmt.Errorf("read auth observation state: %w", err)
		}
		readiness.AuthObservationPending = pending
		versions = append(versions, version)
	} else {
		readiness.Unverified = append(readiness.Unverified, "auth_observation")
	}
	// 无批准时仍沿用既有缺项提示，不将缺批准转换成预览 500。
	if adapter.configuration != nil && readiness.ApprovalReady && (adapter.builtin == nil || readiness.ImagesReady) {
		drift, summary, version, err := adapter.configuration.ReadSubsystemAccessConfigurationDrift(ctx, tenantID, applicationCode, environmentCode)
		if err != nil {
			return SubsystemAccessReadiness{}, fmt.Errorf("read configuration drift: %w", err)
		}
		readiness.ConfigurationDrift, readiness.ChangeSummary = drift, summary
		versions = append(versions, version)
	} else {
		readiness.Unverified = append(readiness.Unverified, "configuration_drift")
	}
	readiness.Unverified = append(readiness.Unverified, "legacy_demo")
	sort.Strings(readiness.Unverified)

	readiness.Version = readinessVersion(versions, readiness.Unverified)
	return readiness, nil
}

func approvalComponentsVersion(components []application.RuntimeLicenseApproval) string {
	entries := make([]string, 0, len(components))
	for _, component := range components {
		entries = append(entries, strings.Join([]string{strings.TrimSpace(component.ServiceID),
			strings.TrimSpace(component.CoverageDigest), strings.TrimSpace(component.ImageDigest)}, "|"))
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, ";")))
	return "approvals=" + hex.EncodeToString(sum[:8])
}

func readinessVersion(versions, unverified []string) string {
	payload := strings.Join(append(append([]string(nil), versions...), unverified...), "|")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:16])
}

// 平台既有数据使用 development/production 等写法，而统一接入接口使用 dev/prod；
// 查询时做别名展开，避免把「已授权/已批准」误判为缺失。
func subsystemAccessEnvironmentAliases(environment string) []string {
	normalized := strings.ToLower(strings.TrimSpace(environment))
	aliasSets := map[string][]string{
		"dev":         {"dev", "development"},
		"development": {"development", "dev"},
		"test":        {"test", "testing"},
		"testing":     {"testing", "test"},
		"staging":     {"staging", "stage"},
		"stage":       {"stage", "staging"},
		"prod":        {"prod", "production"},
		"production":  {"production", "prod"},
	}
	if values, ok := aliasSets[normalized]; ok {
		return values
	}
	return []string{normalized}
}

func (adapter *SubsystemAccessReadinessAdapter) SetConfigurationDriftReader(reader SubsystemAccessConfigurationDriftReader) {
	adapter.configuration = reader
}

func (adapter *SubsystemAccessReadinessAdapter) SetBuiltinRuntimeSource(source application.RuntimeComponentCatalogSource) {
	adapter.builtin = source
}
