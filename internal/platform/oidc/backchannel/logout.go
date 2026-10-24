// Package backchannel 实现 OIDC Back-Channel Logout 1.0 的协议校验与投递边界。
package backchannel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// EventURI 是 Back-Channel Logout 规范固定的事件标识。
const EventURI = "http://schemas.openid.net/event/backchannel-logout"

var (
	ErrInvalidToken = errors.New("invalid OIDC back-channel logout token")
	ErrReplay       = errors.New("OIDC back-channel logout token was already consumed")
)

// Claims 是 logout_token 在 JWT 验签之后仍需校验的声明集合。
type Claims struct {
	Issuer   string
	Audience []string
	Subject  string
	Session  string
	JTI      string
	IssuedAt time.Time
	Expires  time.Time
	Events   map[string]any
	Nonce    string
}

// ValidateClaims 强制执行 Back-Channel Logout 1.0 的声明集与短 TTL 约束；
// JWT 签名/issuer/audience 的密码学校验仍由调用方负责。
func ValidateClaims(claims Claims, expectedIssuer, expectedAudience string, now time.Time, maxTTL time.Duration) error {
	if strings.TrimSpace(claims.Issuer) != strings.TrimSpace(expectedIssuer) || strings.TrimSpace(expectedAudience) == "" || !contains(claims.Audience, expectedAudience) {
		return ErrInvalidToken
	}
	if claims.JTI == "" || claims.Subject == "" && claims.Session == "" || claims.Nonce != "" || claims.IssuedAt.IsZero() || claims.Expires.IsZero() || !claims.Expires.After(now) || claims.IssuedAt.After(now.Add(time.Minute)) {
		return ErrInvalidToken
	}
	if maxTTL <= 0 || claims.Expires.Sub(claims.IssuedAt) > maxTTL || now.Sub(claims.IssuedAt) > maxTTL {
		return ErrInvalidToken
	}
	if claims.Events == nil {
		return ErrInvalidToken
	}
	if _, ok := claims.Events[EventURI]; !ok {
		return ErrInvalidToken
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Delivery 是最小的持久化 outbox 依赖。实现必须在发送前持久化 JTI（防重放），
// 并以原子方式领取待重试消息。
type Delivery interface {
	Claim(context.Context, time.Time, int) ([]Message, error)
	Complete(context.Context, string, time.Time) error
	Fail(context.Context, string, string, time.Time) error
}

// Message 是一条绑定 audience、待签名投递的登出通知。
type Message struct {
	ID, Audience, Subject, Session, JTI, URI string
	Attempt                                  int
}

// Sender 把签名后的 logout_token 投递到一个已注册的 RP URI。
type Sender interface {
	Send(context.Context, Message) error
}

// Dispatcher 持续排空持久化的登出通知；失败消息保留在队列中等待重试。
type Dispatcher struct {
	Queue      Delivery
	Sender     Sender
	BatchSize  int
	RetryDelay time.Duration
}

// Run 持续处理持久队列；退出时尊重 context，未完成消息留给下一实例接管。
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := d.RunOnce(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
			// 错误已经由队列保留为可重试状态；继续轮询避免单次 RP 故障拖垮 Worker。
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce 发送一批有上限的消息；发送失败的消息绝不丢弃，而是按退避重新排队。
func (d *Dispatcher) RunOnce(ctx context.Context, now time.Time) (int, error) {
	if d == nil || d.Queue == nil || d.Sender == nil || d.BatchSize <= 0 {
		return 0, fmt.Errorf("back-channel logout dispatcher is not configured")
	}
	items, err := d.Queue.Claim(ctx, now, d.BatchSize)
	if err != nil {
		return 0, err
	}
	if d.RetryDelay <= 0 {
		d.RetryDelay = time.Minute
	}
	var joined error
	for _, item := range items {
		if err = d.Sender.Send(ctx, item); err != nil {
			joined = errors.Join(joined, err)
			delay := RetryDelay(item.Attempt)
			if delay < d.RetryDelay {
				delay = d.RetryDelay
			}
			_ = d.Queue.Fail(ctx, item.ID, err.Error(), now.Add(delay))
			continue
		}
		if err = d.Queue.Complete(ctx, item.ID, now); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return len(items), joined
}

// ValidateLogoutURI 仅允许绝对 HTTPS URI（或在显式开启时允许 localhost/127.0.0.1
// 的 HTTP URI），拒绝携带用户信息、查询串或片段的地址。
func ValidateLogoutURI(raw string, allowLocalHTTP bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidToken
	}
	if u.Scheme != "https" && !(allowLocalHTTP && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return ErrInvalidToken
	}
	return nil
}
