package coordination

import "time"

// RetiredClient 永久记录已退役的 OAuth client 身份，防止其被任何后续注册复用。
type RetiredClient struct {
	OAuthClientID string `gorm:"primaryKey;column:oauth_client_id;size:128"`
	ServiceID     string
	RetiredAt     time.Time
}

// TableName 绑定 GORM 模型到 license_runtime_retired_client 表。
func (RetiredClient) TableName() string { return "license_runtime_retired_client" }

func memberSpec(m Member) ServiceSpec {
	return ServiceSpec{
		Application: m.Application, Environment: m.Environment, ServiceID: m.ServiceID, OAuthClientID: m.OAuthClientID,
		RegistrationProtocol: normalizeSpecRegistration(ServiceSpec{RegistrationProtocol: m.RegistrationProtocol}).RegistrationProtocol,
		CatalogVersion:       m.CatalogVersion, CredentialGeneration: m.CredentialGeneration,
		CoverageDigest: m.CoverageDigest, ImageDigest: m.ImageDigest,
	}
}
