package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// SubsystemAccessRegistryGORMReader 查询目标是否已在平台目录中登记（应用 + 环境）。
// 事实适配器与本查询共用同一实现，避免多处口径不一致。
type SubsystemAccessRegistryGORMReader struct {
	database *gorm.DB
}

// NewSubsystemAccessRegistryGORMReader 构造查询器。
func NewSubsystemAccessRegistryGORMReader(database *gorm.DB) (*SubsystemAccessRegistryGORMReader, error) {
	if database == nil {
		return nil, errors.New("subsystem access registry reader requires a database")
	}
	return &SubsystemAccessRegistryGORMReader{database: database}, nil
}

// LookupSubsystemAccessRegistration 返回已登记的应用与环境 ID；未登记返回 found=false（不是错误）。
func (reader *SubsystemAccessRegistryGORMReader) LookupSubsystemAccessRegistration(ctx context.Context, tenantID, applicationCode, environmentCode string) (string, string, bool, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationCode = strings.TrimSpace(applicationCode)
	environmentCode = strings.ToLower(strings.TrimSpace(environmentCode))
	if tenantID == "" || applicationCode == "" || environmentCode == "" {
		return "", "", false, fmt.Errorf("registry lookup requires tenant, application and environment")
	}
	var row struct {
		ApplicationID string
		EnvironmentID string
	}
	result := reader.database.WithContext(ctx).Table("platform_application AS application").
		Select("application.id AS application_id, environment.id AS environment_id").
		Joins("JOIN platform_application_environment AS environment ON environment.application_id = application.id AND environment.tenant_id = application.tenant_id").
		Where("application.tenant_id = ? AND application.code = ? AND environment.environment = ?", tenantID, applicationCode, environmentCode).
		Limit(1).Scan(&row)
	if result.Error != nil {
		return "", "", false, fmt.Errorf("read subsystem registry: %w", result.Error)
	}
	if result.RowsAffected == 0 || strings.TrimSpace(row.ApplicationID) == "" {
		return "", "", false, nil
	}
	return row.ApplicationID, row.EnvironmentID, true, nil
}
