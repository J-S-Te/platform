package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

// SubsystemAccessCatalogSource 提供"目标是否在审核清单/交付范围内"的事实（按环境的能力清单）。
// 本地由 LocalDockerSubsystemProvisioner、生产由 UnixSocketSubsystemProvisioner 提供。
type SubsystemAccessCatalogSource interface {
	AvailableCapabilities(context.Context) (application.SubsystemProvisioningCapabilities, error)
}

// SubsystemAccessReadinessSource 提供需要与授权/批准/镜像/认证观察期交互的事实。
// 这些事实必须来自既有受控流程；缺失时不得默认"就绪"。
type SubsystemAccessReadinessSource interface {
	ReadSubsystemAccessReadiness(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessReadiness, error)
}

// SubsystemAccessReadiness 是就绪类事实（全部来自既有流程，不由本模块自行判定）。
type SubsystemAccessReadiness struct {
	ApprovalReady          bool
	LicenseReady           bool
	ImagesReady            bool
	AuthObservationPending bool
	LegacyDemo             bool
	ConfigurationDrift     bool
	ChangeSummary          []string
	// Unverified 列出当前无法确认的事实代码（例如 images、auth_observation）；
	// 未验证不等于缺失：它只作为需人工确认的提示项返回。
	Unverified []string
	// Version 是就绪事实的快照标识（例如批准事实/许可证状态/镜像 digest 摘要）。
	Version string
}

// SubsystemAccessFactsAdapter 实现 application.SubsystemAccessFactsSource：
// 清单与登记/部署状态按真实数据读取，就绪类事实委托给受控来源。
type SubsystemAccessFactsAdapter struct {
	database  *gorm.DB
	catalog   SubsystemAccessCatalogSource
	readiness SubsystemAccessReadinessSource
	registry  SubsystemAccessRegistrationLookup
}

// NewSubsystemAccessFactsAdapter 构造适配器；任一依赖缺失即失败（不做静默降级）。
func NewSubsystemAccessFactsAdapter(database *gorm.DB, catalog SubsystemAccessCatalogSource, readiness SubsystemAccessReadinessSource, registry SubsystemAccessRegistrationLookup) (*SubsystemAccessFactsAdapter, error) {
	if database == nil || catalog == nil || readiness == nil || registry == nil {
		return nil, errors.New("subsystem access facts adapter requires database, catalog, readiness and registry sources")
	}
	return &SubsystemAccessFactsAdapter{database: database, catalog: catalog, readiness: readiness, registry: registry}, nil
}

type subsystemAccessRegistryRow struct {
	ApplicationID  string
	EnvironmentID  string
	ApplicationSta string
	EnvironmentSta string
}

// ReadSubsystemAccessFacts 读取判定所需事实；任何一步失败都返回错误（不猜测、不默认就绪）。
func (adapter *SubsystemAccessFactsAdapter) ReadSubsystemAccessFacts(ctx context.Context, tenantID, applicationCode, environmentCode string) (application.SubsystemAccessFacts, application.SubsystemAccessPreviewVersion, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationCode = strings.TrimSpace(applicationCode)
	environmentCode = strings.ToLower(strings.TrimSpace(environmentCode))
	if tenantID == "" || applicationCode == "" || environmentCode == "" {
		return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, fmt.Errorf("facts lookup requires tenant, application and environment: %w", application.ErrSubsystemAccessInvalidRequest)
	}

	capabilities, err := adapter.catalog.AvailableCapabilities(ctx)
	if err != nil {
		return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, fmt.Errorf("read subsystem capability catalog: %w", err)
	}
	facts := application.SubsystemAccessFacts{
		InScope: containsCapability(capabilities.SupportedApplicationCodes, applicationCode) &&
			containsCapability(capabilities.SupportedEnvironments, environmentCode),
	}
	// 清单摘要：提交时必须与预览时一致，否则要求重新预览。
	for _, target := range capabilities.Targets {
		if strings.EqualFold(strings.TrimSpace(target.ApplicationCode), applicationCode) &&
			strings.EqualFold(strings.TrimSpace(target.Environment), environmentCode) {
			facts.ManifestChecksum = strings.TrimSpace(target.ManifestChecksum)
			break
		}
	}

	registry, found, err := adapter.readRegistry(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, err
	}
	if found {
		facts.Registered = true
		facts.ApplicationID = registry.ApplicationID
		facts.EnvironmentID = registry.EnvironmentID
		repository, err := NewSubsystemOnboardingGORMRepository(adapter.database)
		if err != nil {
			return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, err
		}
		provider, err := repository.ResolveEnvironmentIssuerAlias(ctx, tenantID, applicationCode, environmentCode)
		if err != nil {
			return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, err
		}
		facts.AuthenticationProvider = provider

	}

	deployment, found, err := adapter.readDeployment(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, err
	}
	if found {
		facts.ControlledDeployment = true
		facts.LastOperationFailed = deployment.Status == string(application.SubsystemDeploymentStatusFailed)
	}

	readiness, err := adapter.readiness.ReadSubsystemAccessReadiness(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, fmt.Errorf("read subsystem access readiness: %w", err)
	}
	facts.ApprovalReady = readiness.ApprovalReady
	facts.LicenseReady = readiness.LicenseReady
	facts.ImagesReady = readiness.ImagesReady
	facts.Unverified = append([]string(nil), readiness.Unverified...)
	facts.AuthObservationPending = readiness.AuthObservationPending
	facts.LegacyDemo = readiness.LegacyDemo
	facts.ConfigurationDrift = readiness.ConfigurationDrift
	facts.ChangeSummary = append([]string(nil), readiness.ChangeSummary...)

	version, err := subsystemAccessPreviewVersion(facts, capabilities, readiness, deployment)
	if err != nil {
		return application.SubsystemAccessFacts{}, application.SubsystemAccessPreviewVersion{}, err
	}
	return facts, application.SubsystemAccessPreviewVersion{Version: version}, nil
}

// readRegistry 复用目录查询器，保证与端口实现同一口径。
func (adapter *SubsystemAccessFactsAdapter) readRegistry(ctx context.Context, tenantID, applicationCode, environmentCode string) (subsystemAccessRegistryRow, bool, error) {
	applicationID, environmentID, found, err := adapter.registry.LookupSubsystemAccessRegistration(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return subsystemAccessRegistryRow{}, false, err
	}
	if !found {
		return subsystemAccessRegistryRow{}, false, nil
	}
	return subsystemAccessRegistryRow{ApplicationID: applicationID, EnvironmentID: environmentID}, true, nil
}

type subsystemAccessDeploymentRow struct {
	Status     string
	Operation  string
	Generation uint64
	Attempts   uint32
}

func (adapter *SubsystemAccessFactsAdapter) readDeployment(ctx context.Context, tenantID, applicationCode, environmentCode string) (subsystemAccessDeploymentRow, bool, error) {
	var row subsystemAccessDeploymentRow
	result := adapter.database.WithContext(ctx).Table("subsystem_deployment_state").
		Select("status, operation, generation, attempt_count AS attempts").
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?", tenantID, applicationCode, environmentCode).
		Limit(1).Scan(&row)
	if result.Error != nil {
		return subsystemAccessDeploymentRow{}, false, fmt.Errorf("read subsystem deployment state: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return subsystemAccessDeploymentRow{}, false, nil
	}
	return row, true, nil
}

// subsystemAccessPreviewVersion 把事实折算成稳定摘要：任一事实变化都会让旧预览失效。
func subsystemAccessPreviewVersion(facts application.SubsystemAccessFacts, capabilities application.SubsystemProvisioningCapabilities, readiness SubsystemAccessReadiness, deployment subsystemAccessDeploymentRow) (string, error) {
	canonical := map[string]any{
		"in_scope":                facts.InScope,
		"registered":              facts.Registered,
		"authentication_provider": facts.AuthenticationProvider,
		"controlled":              facts.ControlledDeployment,
		"last_failed":             facts.LastOperationFailed,
		"drift":                   facts.ConfigurationDrift,
		"approval_ready":          facts.ApprovalReady,
		"license_ready":           facts.LicenseReady,
		"images_ready":            facts.ImagesReady,
		"observation_pending":     facts.AuthObservationPending,
		"legacy_demo":             facts.LegacyDemo,
		"catalog_mode":            capabilities.Mode,
		"manifest_checksum":       facts.ManifestChecksum,
		"readiness_version":       strings.TrimSpace(readiness.Version),
		"deployment_status":       deployment.Status,
		"deployment_gen":          deployment.Generation,
		"deployment_attempts":     deployment.Attempts,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode subsystem access preview version: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func containsCapability(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}
