package backchannel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSigner 由平台 OIDC JWT 管理器实现；Sender 不接触私钥。
type TokenSigner interface {
	IssueLogoutToken(issuer, audience, subject, session, jti string, now time.Time, ttl time.Duration) (string, error)
}

// HTTPFormSender 按 OIDC Back-Channel Logout 规范以表单提交 logout_token。
type HTTPFormSender struct {
	Client         *http.Client
	Signer         TokenSigner
	Issuer         string
	TTL            time.Duration
	Now            func() time.Time
	AllowLocalHTTP bool
}

// Send 生成短时专用 logout_token 并 POST；响应正文永不写入错误，避免泄漏令牌。
func (s HTTPFormSender) Send(ctx context.Context, message Message) error {
	if s.Signer == nil || strings.TrimSpace(message.URI) == "" || strings.TrimSpace(message.Audience) == "" || strings.TrimSpace(message.JTI) == "" {
		return errors.New("back-channel logout sender is not configured")
	}
	if err := ValidateLogoutURI(message.URI, s.AllowLocalHTTP); err != nil {
		return fmt.Errorf("back-channel logout URI is invalid: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if s.Client != nil {
		clone := *s.Client
		client = &clone
	}
	// 即使调用方注入了 HTTP Client，也必须覆盖重定向策略；否则 30x 会把 logout_token
	// 发送给未登记地址。Transport、Timeout 等其余生产配置仍由浅拷贝保留。
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("back-channel logout redirect is not allowed")
	}
	if s.TTL <= 0 {
		s.TTL = time.Minute
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	token, err := s.Signer.IssueLogoutToken(s.Issuer, message.Audience, message.Subject, message.Session, message.JTI, now, s.TTL)
	if err != nil {
		return fmt.Errorf("issue logout token: %w", err)
	}
	body := strings.NewReader(url.Values{"logout_token": {token}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, message.URI, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send logout notification: %w", err)
	}
	defer response.Body.Close()
	// 响应内容不进入日志，但有界排空可复用 TLS 连接，避免大量注销时耗尽连接。
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("logout notification returned HTTP %d", response.StatusCode)
	}
	return nil
}
