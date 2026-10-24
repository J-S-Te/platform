package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConsentDecision 的零记录语义是“需要同意且尚未授权”，避免缺失数据被解释成默认允许。
// 它只描述持久决定，是否展示同意页仍由授权流程结合客户端和请求范围决定。
type ConsentDecision struct {
	Required bool
	Granted  bool
	Scopes   []string
}

// ConsentInput 是一次用户同意判定或记录的入参；Scopes 为本次请求的 scope 集合。
type ConsentInput struct {
	TenantID string
	UserID   string
	ClientID string
	Scopes   []string
}

// ConsentRepository 持久化用户对单个客户端的已授予 scope 集合；实现必须以
// 租户/用户/客户端三元组为边界。
type ConsentRepository interface {
	FindConsent(context.Context, string, string, string, time.Time) ([]string, bool, error)
	GrantConsent(context.Context, string, string, string, []string, time.Time) error
	RevokeConsent(context.Context, string, string, string, time.Time) error
}

// DecideConsent 评估既有的持久同意；绝不在无记录时默认授予。
func (service *Service) DecideConsent(ctx context.Context, input ConsentInput) (ConsentDecision, error) {
	input = normalizedConsentInput(input)
	if input.TenantID == "" || input.UserID == "" || input.ClientID == "" || len(input.Scopes) == 0 {
		return ConsentDecision{}, ErrInvalidRequest
	}
	client, err := service.repository.FindClient(ctx, input.ClientID, service.now())
	if err != nil {
		return ConsentDecision{}, mapClientError(err)
	}
	if client.TenantID != input.TenantID {
		return ConsentDecision{}, ErrInvalidClient
	}
	if _, err = registeredScopes(client, input.Scopes); err != nil {
		return ConsentDecision{}, err
	}
	repository, ok := service.repository.(ConsentRepository)
	if !ok {
		return ConsentDecision{Required: true, Granted: false, Scopes: input.Scopes}, nil
	}
	granted, exists, err := repository.FindConsent(ctx, input.TenantID, input.UserID, client.ID, service.now())
	if err != nil {
		return ConsentDecision{}, fmt.Errorf("find OIDC consent: %w", err)
	}
	if !exists || !scopeSetContains(granted, input.Scopes) {
		return ConsentDecision{Required: true, Granted: false, Scopes: input.Scopes}, nil
	}
	return ConsentDecision{Required: false, Granted: true, Scopes: input.Scopes}, nil
}

// GrantConsent 记录用户对客户端的 scope 授权；scope 必须全部在客户端当前
// 登记范围内。
func (service *Service) GrantConsent(ctx context.Context, input ConsentInput) error {
	input = normalizedConsentInput(input)
	if input.TenantID == "" || input.UserID == "" || input.ClientID == "" || len(input.Scopes) == 0 {
		return ErrInvalidRequest
	}
	client, err := service.repository.FindClient(ctx, input.ClientID, service.now())
	if err != nil {
		return mapClientError(err)
	}
	if client.TenantID != input.TenantID {
		return ErrInvalidClient
	}
	// 持久同意只能覆盖客户端当前登记的 scope；客户端登记被收紧后，旧同意记录不能用来恢复已删除范围。
	if _, err = registeredScopes(client, input.Scopes); err != nil {
		return err
	}
	repository, ok := service.repository.(ConsentRepository)
	if !ok {
		return errors.New("OIDC consent persistence is not configured")
	}
	return repository.GrantConsent(ctx, input.TenantID, input.UserID, client.ID, input.Scopes, service.now())
}

// RevokeConsent 移除用户对单个客户端的全部已存储授权。
func (service *Service) RevokeConsent(ctx context.Context, tenantID, userID, clientID string) error {
	tenantID, userID, clientID = strings.TrimSpace(tenantID), strings.TrimSpace(userID), strings.TrimSpace(clientID)
	if tenantID == "" || userID == "" || clientID == "" {
		return ErrInvalidRequest
	}
	client, err := service.repository.FindClient(ctx, clientID, service.now())
	if err != nil {
		return mapClientError(err)
	}
	if client.TenantID != tenantID {
		return ErrInvalidClient
	}
	repository, ok := service.repository.(ConsentRepository)
	if !ok {
		return errors.New("OIDC consent persistence is not configured")
	}
	err = repository.RevokeConsent(ctx, tenantID, userID, client.ID, service.now())
	if errors.Is(err, ErrNotFound) {
		// 同意撤回是幂等的：授权本就不存在时仍按安全撤销处理。
		return nil
	}
	return err
}

// PostLogoutRedirectRepository 提供客户端专用的 post-logout 重定向登记查询。
type PostLogoutRedirectRepository interface {
	IsRegisteredPostLogoutRedirectURI(context.Context, string, string, time.Time) (bool, error)
}

// IsRegisteredPostLogoutRedirectURI 刻意只查询专用的 post-logout 登记集合，
// 不复用普通 OAuth redirect URI。
func (service *Service) IsRegisteredPostLogoutRedirectURI(ctx context.Context, clientID, redirectURI string) (bool, error) {
	clientID, redirectURI = strings.TrimSpace(clientID), strings.TrimSpace(redirectURI)
	if clientID == "" || redirectURI == "" {
		return false, nil
	}
	repository, ok := service.repository.(PostLogoutRedirectRepository)
	if !ok {
		return false, nil
	}
	registered, err := repository.IsRegisteredPostLogoutRedirectURI(ctx, clientID, redirectURI, service.now())
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return registered, err
}

func normalizedConsentInput(input ConsentInput) ConsentInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.UserID = strings.TrimSpace(input.UserID)
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.Scopes = normalizeScopes(input.Scopes)
	return input
}
func scopeSetContains(granted, requested []string) bool {
	// 同意采用集合包含而非字符串相等：已同意更大集合时可免重复确认，但任何新增 scope 都会重新触发同意。
	values := map[string]struct{}{}
	for _, scope := range granted {
		values[scope] = struct{}{}
	}
	for _, scope := range requested {
		if _, ok := values[scope]; !ok {
			return false
		}
	}
	return true
}
