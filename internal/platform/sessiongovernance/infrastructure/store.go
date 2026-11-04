// Package infrastructure 持久化中央会话，所有消费和续期均由数据库事务串行裁决。
package infrastructure

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// VerifiedIdentity 只接受经过签名、issuer、audience及nonce验证的上游声明。
type VerifiedIdentity struct {
	Issuer, SID, TenantID, IdentityID, Subject string
	AuthenticatedAt                            time.Time
}

// Client 是从有效应用登记中读取的精确客户端范围。
type Client struct{ ID, TenantID, ApplicationCode, EnvironmentCode string }

// Context 是不含秘密或权限的中央生命周期视图。
type Context struct {
	SessionID           string    `json:"session_id"`
	TenantID            string    `json:"tenant_id"`
	SubjectID           string    `json:"subject_id"`
	ClientID            string    `json:"client_id"`
	ApplicationCode     string    `json:"application_code"`
	EnvironmentCode     string    `json:"environment_code"`
	PolicyVersion       uint64    `json:"policy_version"`
	IdleTimeoutSeconds  int64     `json:"idle_timeout_seconds"`
	IdleRemainingMS     int64     `json:"idle_remaining_ms"`
	AbsoluteExpiresAt   time.Time `json:"absolute_expires_at"`
	AbsoluteRemainingMS int64     `json:"absolute_remaining_ms"`
}

// Binding 的原文秘密仅返回给RP服务器一次，数据库只保留摘要。
type Binding struct {
	Token   string  `json:"binding_token"`
	Context Context `json:"session_context"`
}

// Store 是中央生命周期存储；原会话及sid失效记录不会被续期覆盖。
type Store struct{ db *gorm.DB }

type ticketRow struct {
	TokenHash, ClientID, TenantID, ApplicationCode, EnvironmentCode, NonceHash, ParentID string
	ExpiresAt                                                                            time.Time
	ConsumedAt                                                                           *time.Time
}
type bindingRow struct{ TokenHash, SessionID, ClientID, TenantID, ApplicationCode, EnvironmentCode string }
type upstreamRow struct {
	UpstreamHash, Issuer, Subject, TenantID, IdentityID string
	SID                                                 string `gorm:"column:sid"`
	AuthenticatedAt                                     time.Time
	SessionID                                           *string
}
type platformLink struct {
	IAMSessionID string `gorm:"column:iam_session_id"`
	SessionID    string
}

// NewStore 建立中央存储，不执行隐式建表或迁移。
// 参数：db为组合根注入的数据库连接。
// 返回值：存储；缺数据库时返回配置错误。
func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("central session database is required")
	}
	return &Store{db: db}, nil
}

// secret 生成不可预测的短票据或服务器绑定秘密。
// 参数：无。
// 返回值：256位随机值的URL安全编码；系统随机源失败时返回错误。
func secret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// digest 将秘密或带分隔符的可信上游标识转换为索引摘要。
// 参数：value为原值；调用方不得把秘密原文写入数据库或日志。
// 返回值：固定长度SHA256十六进制摘要。
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Begin 登记受精确客户端和nonce约束的两分钟一次性票据。
// 参数：ctx为请求，client为登记范围，nonceHash为摘要，parent为当前中央锚点或空，now为时钟。
// 返回值：票据原文；参数不完整或存储失败返回错误。
func (s *Store) Begin(ctx context.Context, client Client, nonceHash, parent string, now time.Time) (string, error) {
	if client.ID == "" || client.TenantID == "" || client.ApplicationCode == "" || client.EnvironmentCode == "" || nonceHash == "" {
		return "", domain.ErrValidation
	}
	token, err := secret()
	if err != nil {
		return "", err
	}
	row := ticketRow{TokenHash: digest(token), ClientID: client.ID, TenantID: client.TenantID,
		ApplicationCode: client.ApplicationCode, EnvironmentCode: client.EnvironmentCode,
		NonceHash: nonceHash, ParentID: parent, ExpiresAt: now.UTC().Add(2 * time.Minute)}
	err = s.db.WithContext(ctx).Table("sgov_ticket").Create(&row).Error
	return token, err
}

// policy 读取同租户现有平台安全策略，绝对期限由中央固定为八小时。
// 参数：tx为当前事务，tenant为可信租户。
// 返回值：不超过本轮目标的策略；数据库或非法配置返回错误。
func policy(tx *gorm.DB, tenant string) (domain.Policy, error) {
	p := domain.DefaultPolicy()
	var row struct {
		IdleTimeoutSeconds uint
		Version            uint64
	}
	err := tx.Table("sec_login_policy").Where("tenant_id = ?", tenant).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.IdleTimeout = time.Duration(row.IdleTimeoutSeconds) * time.Second
	p.Version = row.Version
	return p, p.Validate()
}

// Complete 原子核对并消费票据，将同一上游sid关联至同一个固定期限锚点。
// 参数：ctx为请求，ticket/nonceHash/clientID为已核验材料，identity为签名身份，now为时钟。
// 返回值：一次性服务器绑定；重放、过期、身份或锚点冲突分别返回明确错误。
func (s *Store) Complete(ctx context.Context, ticket, nonceHash, clientID string, identity VerifiedIdentity, now time.Time) (Binding, error) {
	if ticket == "" || identity.Issuer == "" || identity.SID == "" || identity.Subject == "" ||
		identity.TenantID == "" || identity.IdentityID == "" || identity.AuthenticatedAt.IsZero() {
		return Binding{}, domain.ErrValidation
	}
	var result Binding
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ticketRecord ticketRow
		if err := tx.Table("sgov_ticket").Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", digest(ticket)).Take(&ticketRecord).Error; err != nil {
			return inactiveMissing(err)
		}
		if ticketRecord.ConsumedAt != nil || !ticketRecord.ExpiresAt.After(now) || ticketRecord.ClientID != clientID || ticketRecord.NonceHash != nonceHash {
			return domain.ErrInactive
		}
		if ticketRecord.TenantID != identity.TenantID {
			return domain.ErrBindingConflict
		}
		p, err := policy(tx, identity.TenantID)
		if err != nil {
			return err
		}
		anchor, err := resolveIdentity(tx, identity, ticketRecord.ParentID, now, p)
		if err != nil {
			return err
		}
		token, err := secret()
		if err != nil {
			return err
		}
		binding := bindingRow{TokenHash: digest(token), SessionID: anchor.ID, ClientID: clientID, TenantID: identity.TenantID,
			ApplicationCode: ticketRecord.ApplicationCode, EnvironmentCode: ticketRecord.EnvironmentCode}
		if err = tx.Table("sgov_binding").Create(&binding).Error; err != nil {
			return err
		}
		if err = tx.Table("sgov_ticket").Where("token_hash = ?", ticketRecord.TokenHash).Update("consumed_at", now.UTC()).Error; err != nil {
			return err
		}
		result = Binding{Token: token, Context: view(anchor, binding, p, now)}
		return nil
	})
	if err != nil {
		return Binding{}, err
	}
	return result, nil
}

// resolveIdentity 在上游唯一行锁内选择锚点，两个已存在周期绝不自动合并。
// 参数：tx为事务，identity为签名声明，parent为浏览器平台锚点，now/p为中央时钟及策略。
// 返回值：已锁定有效锚点；任何身份或期限不一致返回错误。
func resolveIdentity(tx *gorm.DB, identity VerifiedIdentity, parent string, now time.Time, p domain.Policy) (domain.Session, error) {
	if err := activeIdentity(tx, identity.TenantID, identity.IdentityID, now); err != nil {
		return domain.Session{}, err
	}
	upstream := upstreamRow{UpstreamHash: digest(identity.Issuer + "\x00" + identity.SID), Issuer: identity.Issuer,
		SID: identity.SID, Subject: identity.Subject, TenantID: identity.TenantID, IdentityID: identity.IdentityID,
		AuthenticatedAt: identity.AuthenticatedAt.UTC()}
	if err := tx.Table("sgov_upstream").Clauses(clause.OnConflict{DoNothing: true}).Create(&upstream).Error; err != nil {
		return domain.Session{}, err
	}
	var row upstreamRow
	if err := tx.Table("sgov_upstream").Clauses(clause.Locking{Strength: "UPDATE"}).Where("upstream_hash = ?", upstream.UpstreamHash).Take(&row).Error; err != nil {
		return domain.Session{}, err
	}
	if row.Issuer != identity.Issuer || row.SID != identity.SID || row.Subject != identity.Subject ||
		row.TenantID != identity.TenantID || row.IdentityID != identity.IdentityID || !row.AuthenticatedAt.Equal(identity.AuthenticatedAt) {
		return domain.Session{}, domain.ErrBindingConflict
	}
	linked := ""
	if row.SessionID != nil {
		linked = *row.SessionID
	}
	id := linked
	if id == "" {
		id = parent
	}
	var anchor domain.Session
	if id != "" {
		if err := tx.Table("sgov_session").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&anchor).Error; err != nil {
			return anchor, inactiveMissing(err)
		}
		if anchor.TenantID != identity.TenantID || anchor.IdentityID != identity.IdentityID {
			return anchor, domain.ErrBindingConflict
		}
		if row.SessionID == nil {
			var previous upstreamRow
			err := tx.Table("sgov_upstream").Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("session_id = ? AND issuer = ?", id, identity.Issuer).Take(&previous).Error
			if err == nil && previous.UpstreamHash != row.UpstreamHash {
				return anchor, domain.ErrBindingConflict
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return anchor, err
			}
		}
		if !anchor.ValidAt(now, p) {
			return anchor, fmt.Errorf("%w: %w", domain.ErrInactive, domain.ErrUpstreamInactive)
		}
		// 旧上游到期应精确终止其sid；只有两个有效锚点才属于绑定冲突。
		if err := domain.CheckAnchorBinding(parent, linked); err != nil {
			return anchor, err
		}
		// 绑定新上游时只能收窄，不能重设中央认证起点或绝对截止。
		candidate := identity.AuthenticatedAt.Add(p.AbsoluteTTL)
		if candidate.Before(anchor.ExpiresAt) {
			anchor.ExpiresAt = candidate
		}
		if identity.AuthenticatedAt.Before(anchor.AuthenticatedAt) {
			anchor.AuthenticatedAt = identity.AuthenticatedAt
		}
		if !anchor.ValidAt(now, p) {
			return anchor, domain.ErrInactive
		}
		if err := tx.Table("sgov_session").Where("id = ?", id).Updates(map[string]any{"authenticated_at": anchor.AuthenticatedAt, "expires_at": anchor.ExpiresAt}).Error; err != nil {
			return anchor, err
		}
	} else {
		var err error
		id, err = (ulid.Generator{}).New(now)
		if err != nil {
			return anchor, err
		}
		anchor, err = domain.NewSession(id, identity.TenantID, identity.IdentityID, identity.AuthenticatedAt, now, p, nil)
		if err != nil {
			if errors.Is(err, domain.ErrInactive) {
				return anchor, fmt.Errorf("%w: %w", domain.ErrInactive, domain.ErrUpstreamInactive)
			}
			return anchor, err
		}
		if err = tx.Table("sgov_session").Create(&anchor).Error; err != nil {
			return anchor, err
		}
	}
	if row.SessionID == nil {
		if err := tx.Table("sgov_upstream").Where("upstream_hash = ?", row.UpstreamHash).Update("session_id", id).Error; err != nil {
			return anchor, err
		}
	}
	return anchor, nil
}

// State 只在显式交互时推进中央窗口，绑定秘密不承担业务授权。
// 参数：ctx为请求，token为服务器秘密，now为时钟，interact表示已通过RP同源校验的真实活动。
// 返回值：权威期限；绑定未知或锚点失效时返回未认证。
func (s *Store) State(ctx context.Context, token string, now time.Time, interact bool) (Context, error) {
	if token == "" {
		return Context{}, domain.ErrInactive
	}
	var result Context
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var binding bindingRow
		if err := tx.Table("sgov_binding").Where("token_hash = ?", digest(token)).Take(&binding).Error; err != nil {
			return inactiveMissing(err)
		}
		anchor, p, err := activeAnchor(tx, binding.SessionID, now)
		if err != nil {
			return err
		}
		if anchor.TenantID != binding.TenantID {
			return domain.ErrBindingConflict
		}
		if interact {
			anchor, err = anchor.Interact(now, p)
			if err != nil {
				return err
			}
			if err = tx.Table("sgov_session").Where("id = ?", anchor.ID).Updates(map[string]any{"last_interactive_at": anchor.LastInteractiveAt, "version": anchor.Version}).Error; err != nil {
				return err
			}
		}
		result = view(anchor, binding, p, now)
		return nil
	})
	return result, err
}

// activeAnchor 获取行锁后核验平台当前策略，防止活动与撤销并发复活。
// 参数：tx为事务，id为中央锚点，now为时钟。
// 返回值：有效锚点和策略；缺失、失效或数据库失败时返回错误。
func activeAnchor(tx *gorm.DB, id string, now time.Time) (domain.Session, domain.Policy, error) {
	var anchor domain.Session
	// 人员共享锁先于中央行锁，防止与全局撤销交换顺序或在撤销后创建新周期。
	if err := tx.Table("sgov_session").Where("id = ?", id).Take(&anchor).Error; err != nil {
		return anchor, domain.Policy{}, inactiveMissing(err)
	}
	if err := activeIdentity(tx, anchor.TenantID, anchor.IdentityID, now); err != nil {
		return anchor, domain.Policy{}, err
	}
	if err := tx.Table("sgov_session").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&anchor).Error; err != nil {
		return anchor, domain.Policy{}, inactiveMissing(err)
	}
	p, err := policy(tx, anchor.TenantID)
	if err != nil {
		return anchor, p, err
	}
	if !anchor.ValidAt(now, p) {
		return anchor, p, domain.ErrInactive
	}
	return anchor, p, nil
}

// view 生成不暴露上游材料、绑定秘密和业务权限的生命周期表示。
// 参数：anchor为有效锚点，binding为客户端范围，p为策略，now为时钟。
// 返回值：受固定绝对截止约束的中央期限。
func view(anchor domain.Session, binding bindingRow, p domain.Policy, now time.Time) Context {
	return Context{SessionID: anchor.ID, TenantID: anchor.TenantID, SubjectID: anchor.IdentityID,
		ClientID: binding.ClientID, ApplicationCode: binding.ApplicationCode, EnvironmentCode: binding.EnvironmentCode,
		PolicyVersion: p.Version, IdleTimeoutSeconds: int64(p.IdleTimeout / time.Second), IdleRemainingMS: anchor.IdleRemaining(now, p).Milliseconds(),
		AbsoluteExpiresAt: anchor.ExpiresAt.UTC(), AbsoluteRemainingMS: anchor.ExpiresAt.Sub(now).Milliseconds()}
}

// inactiveMissing 保留数据库依赖失败，只把不存在映射成未认证。
// 参数：err为数据库结果。
// 返回值：不存在时ErrInactive，否则保留原错误。
func inactiveMissing(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrInactive
	}
	return err
}

// TicketContext 只读提取未消费票据的验证约束，不提前消费登录事务。
// 参数：ctx为请求，token为随机票据，now为中央时钟。
// 返回值：精确客户端及nonce摘要；未知、到期或已消费票据返回未认证。
func (s *Store) TicketContext(ctx context.Context, token string, now time.Time) (string, string, error) {
	var row ticketRow
	err := s.db.WithContext(ctx).Table("sgov_ticket").Where("token_hash = ?", digest(token)).Take(&row).Error
	if err != nil {
		return "", "", inactiveMissing(err)
	}
	if row.ConsumedAt != nil || !row.ExpiresAt.After(now) {
		return "", "", domain.ErrInactive
	}
	return row.ClientID, row.NonceHash, nil
}
