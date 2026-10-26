package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

const accessCandidatePrefix = "candidate:v1:"

// SubsystemAccessConfigurationTracker 只保存候选摘要到既有阶段证据，不保存配置或密钥。
// 基线只取成功操作的 DEPLOY 证据；失败或尚在运行的部署不能成为一致性证明。
type SubsystemAccessConfigurationTracker struct {
	database  *gorm.DB
	catalog   SubsystemAccessCatalogSource
	approvals application.RuntimeLicenseApprovalSource
}

func NewSubsystemAccessConfigurationTracker(db *gorm.DB, catalog SubsystemAccessCatalogSource, approvals application.RuntimeLicenseApprovalSource) (*SubsystemAccessConfigurationTracker, error) {
	if db == nil || catalog == nil || approvals == nil {
		return nil, errors.New("configuration tracker requires database, catalog and approvals")
	}
	return &SubsystemAccessConfigurationTracker{database: db, catalog: catalog, approvals: approvals}, nil
}

func accessConfigurationFingerprint(manifest string, components []application.RuntimeLicenseApproval) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(manifest) + "|" + approvalComponentsVersion(components)))
	return hex.EncodeToString(sum[:])
}
func accessCandidateFromEvidence(evidence string) string {
	if !strings.HasPrefix(evidence, accessCandidatePrefix) {
		return ""
	}
	digest := strings.SplitN(strings.TrimPrefix(evidence, accessCandidatePrefix), ";", 2)[0]
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != sha256.Size {
		return ""
	}
	return digest
}

func (tracker *SubsystemAccessConfigurationTracker) Snapshot(ctx context.Context, operation application.SubsystemAccessOperation) (string, error) {
	target, found, err := resolveSubsystemAccessTarget(ctx, tracker.catalog, operation)
	if err != nil {
		return "", err
	}
	if !found {
		return "", application.ErrSubsystemAccessInvalidRequest
	}
	var components []application.RuntimeLicenseApproval
	for _, environment := range subsystemAccessEnvironmentAliases(operation.EnvironmentCode) {
		approved, err := tracker.approvals.ApprovedRuntimeLicenseComponents(ctx, operation.ApplicationCode, environment)
		if err != nil {
			if application.RuntimeLicenseApprovalErrorCode(err) != "" {
				continue
			}
			return "", err
		}
		components = append(components, approved...)
	}
	if len(components) == 0 {
		return "", errors.New("configuration snapshot requires approved images")
	}
	return accessConfigurationFingerprint(target.ManifestChecksum, components), nil
}

func (tracker *SubsystemAccessConfigurationTracker) ReadSubsystemAccessConfigurationDrift(ctx context.Context, tenantID, code, environment string) (bool, []string, string, error) {
	op := application.SubsystemAccessOperation{TenantID: tenantID, ApplicationCode: code, EnvironmentCode: environment}
	candidate, err := tracker.Snapshot(ctx, op)
	if err != nil {
		return false, nil, "", err
	}
	var state struct{ AppliedManifestChecksum string }
	result := tracker.database.WithContext(ctx).Table("subsystem_deployment_state").Select("applied_manifest_checksum").Where("tenant_id = ? AND application_code = ? AND environment_code = ?", tenantID, code, environment).Limit(1).Scan(&state)
	if result.Error != nil {
		return false, nil, "", result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil, candidate, nil
	}
	var baseline struct{ EvidenceRef string }
	result = tracker.database.WithContext(ctx).Table("subsystem_access_operation AS operation").Select("stage.evidence_ref").
		Joins("JOIN subsystem_access_operation_stage AS stage ON stage.operation_id = operation.id AND stage.tenant_id = operation.tenant_id AND stage.stage = ? AND stage.status = ?", "DEPLOY", "SUCCEEDED").
		Where("operation.tenant_id = ? AND operation.application_code = ? AND operation.environment_code = ? AND operation.status = ?", tenantID, code, environment, "SUCCEEDED").
		Order("operation.updated_at DESC, operation.id DESC").Limit(1).Scan(&baseline)
	if result.Error != nil {
		return false, nil, "", result.Error
	}
	applied := accessCandidateFromEvidence(baseline.EvidenceRef)
	target, _, err := resolveSubsystemAccessTarget(ctx, tracker.catalog, op)
	if err != nil {
		return false, nil, "", err
	}
	drift, summary := accessConfigurationDrift(candidate, applied, target.ManifestChecksum, state.AppliedManifestChecksum)
	return drift, summary, candidate + ":" + applied + ":" + state.AppliedManifestChecksum, nil
}

func accessConfigurationDrift(candidate, baseline, manifest, appliedManifest string) (bool, []string) {
	if baseline == "" {
		return true, []string{"缺少已验证的清单与镜像基线，请执行受控更新建立基线"}
	}
	if candidate != baseline {
		return true, []string{"候选清单或批准镜像已变化，需要受控更新"}
	}
	if strings.TrimSpace(manifest) != "" && strings.TrimSpace(manifest) != strings.TrimSpace(appliedManifest) {
		return true, []string{"候选清单与已应用清单不一致"}
	}
	return false, nil
}

// Verify 确保部署之后候选未改变。DEPLOY 证据由 Worker 持久化，支持进程重启后的收口。
func (tracker *SubsystemAccessConfigurationTracker) Verify(ctx context.Context, operation application.SubsystemAccessOperation) (bool, error) {
	candidate, err := tracker.Snapshot(ctx, operation)
	if err != nil {
		return false, err
	}
	var stage struct{ EvidenceRef string }
	result := tracker.database.WithContext(ctx).Table("subsystem_access_operation_stage").Select("evidence_ref").
		Where("tenant_id = ? AND operation_id = ? AND stage = ? AND status = ?", operation.TenantID, operation.ID, "DEPLOY", "SUCCEEDED").Limit(1).Scan(&stage)
	if result.Error != nil {
		return false, fmt.Errorf("read deployed candidate evidence: %w", result.Error)
	}
	return accessCandidateFromEvidence(stage.EvidenceRef) == candidate, nil
}
