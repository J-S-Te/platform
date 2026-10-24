// Package domain 定义应用集成认证所需的运行时快照，管理面明文密钥不会进入该模型。
package domain

import "time"

// OAuthClient 仅包含签发和校验 Client Credentials 令牌所需的活跃登记边界；租户、应用、
// 环境必须整体匹配。凭据摘要由仓储留在校验路径，不通过该模型向调用方扩散。
type OAuthClient struct {
	ID                    string
	TenantID              string
	ApplicationID         string
	ApplicationCode       string
	EnvironmentID         string
	EnvironmentCode       string
	ClientID              string
	TokenAuthMethod       string
	AccessTokenTTLSeconds uint
	GrantTypes            map[string]struct{}
	Scopes                map[string]struct{}
}

// ClientCredential 表示当前可用的密钥哈希，刻意只保存
// 从 platform_oauth_client_credential 读取的单向哈希。
type ClientCredential struct {
	SecretHash []byte
	ValidUntil *time.Time
}
