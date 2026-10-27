package infrastructure

import (
	"context"
	"encoding/json"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"time"
)

type accessRuntimeExpectation struct {
	Approval             application.RuntimeLicenseApproval
	RegistrationProtocol int
	CatalogVersion       int
	ClientID             string
}

// SaveAccessRuntimeExpectations 用途：在派发前保存仅用于自然 ACK 核对的非秘密身份。
// 参数：ctx 为上下文，operation 为任务，credentials 为内存凭据（秘密绝不入库）。
// 返回值：返回绑定或保存错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) SaveAccessRuntimeExpectations(ctx context.Context, operation application.SubsystemAccessOperation, credentials []application.RuntimeLicenseCredential) error {
	expectations := make([]accessRuntimeExpectation, 0, len(credentials))
	for _, c := range credentials {
		expectations = append(expectations, accessRuntimeExpectation{Approval: c.Approval, RegistrationProtocol: c.RegistrationProtocol, CatalogVersion: c.CatalogVersion, ClientID: c.ClientID})
	}
	raw, err := json.Marshal(expectations)
	if err != nil {
		return err
	}
	return reader.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockAccessOperation(ctx, tx, operation, time.Now().UTC()); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		if err := tx.Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
			return err
		}
		if attempt.Phase != "PREPARED" {
			return application.ErrSubsystemAccessOperationConflict
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			return err
		}
		// 已锁定任务并确认阶段；内容完全相同的重入允许零变更行数。
		return tx.Model(&attempt).Update("runtime_expectations", string(raw)).Error
	})
}

// ReadAccessRuntimeExpectations 用途：恢复自然 READY/ACK 验证所需身份，不生成或轮换凭据。
// 参数：ctx 为上下文，operation 为任务。
// 返回值：返回不含 Secret 的期望消费者集合或读取错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) ReadAccessRuntimeExpectations(ctx context.Context, operation application.SubsystemAccessOperation) ([]application.RuntimeLicenseCredential, error) {
	var attempt subsystemAccessDeploymentAttempt
	if err := reader.database.WithContext(ctx).Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
		return nil, err
	}
	var expectations []accessRuntimeExpectation
	if err := json.Unmarshal(attempt.RuntimeExpectations, &expectations); err != nil {
		return nil, err
	}
	credentials := make([]application.RuntimeLicenseCredential, 0, len(expectations))
	for _, c := range expectations {
		credentials = append(credentials, application.RuntimeLicenseCredential{Approval: c.Approval, RegistrationProtocol: c.RegistrationProtocol, CatalogVersion: c.CatalogVersion, ClientID: c.ClientID})
	}
	return credentials, nil
}
