package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 旧 Secret 退役策略（RUNTIME-05）：登记新密钥时旧值不能永久可用，但默认不得
// 自动吊销现网密钥。本文件固定三条契约：
//  1. 未配置时策略为零值 KEEP，签发路径行为与既有“只追加”完全一致；
//  2. 显式开启 REVOKE/GRACE 后，策略随输入透传到仓储（由仓储在同一事务内执行）；
//  3. 非法策略/宽限窗口在服务层被拒绝，不会静默降级为“永久可用”。

func TestParseOAuthClientSecretRetirement(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		grace   time.Duration
		want    OAuthClientSecretRetirement
		wantErr bool
	}{
		{name: "unset keeps legacy behaviour", policy: "", grace: 0, want: OAuthClientSecretRetirement{}},
		{name: "explicit keep", policy: " keep ", grace: 0, want: OAuthClientSecretRetirement{}},
		{name: "revoke", policy: "revoke", grace: 0, want: OAuthClientSecretRetirement{Policy: OAuthClientSecretRetirementRevoke}},
		{name: "grace window", policy: "GRACE", grace: 24 * time.Hour, want: OAuthClientSecretRetirement{Policy: OAuthClientSecretRetirementGrace, Grace: 24 * time.Hour}},
		{name: "grace requires window", policy: "grace", grace: 0, wantErr: true},
		{name: "grace within cap", policy: "grace", grace: maxRotationOverlap, want: OAuthClientSecretRetirement{Policy: OAuthClientSecretRetirementGrace, Grace: maxRotationOverlap}},
		{name: "grace beyond cap", policy: "grace", grace: maxRotationOverlap + time.Second, wantErr: true},
		{name: "revoke rejects grace", policy: "revoke", grace: time.Hour, wantErr: true},
		{name: "keep rejects grace", policy: "keep", grace: time.Hour, wantErr: true},
		{name: "unset rejects grace", policy: "", grace: time.Hour, wantErr: true},
		{name: "unknown policy", policy: "rotate-forever", grace: 0, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseOAuthClientSecretRetirement(test.policy, test.grace)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got %+v", got)
				}
				if !errors.Is(err, ErrManagementValidation) {
					t.Fatalf("error = %v, want ErrManagementValidation", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ParseOAuthClientSecretRetirement(%q, %s) = %+v, %v", test.policy, test.grace, got, err)
			}
		})
	}
}

func TestCreateOAuthClientSecretDefaultsToKeepingPreviousSecret(t *testing.T) {
	repository := &secretRetirementRepository{}
	service, err := NewOAuthClientManagementService(repository, fixedManagementIDs{}, staticManagementClock{}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CreateOAuthClientSecret(context.Background(), OAuthClientSecretCreateInput{TenantID: "tenant-1", OAuthClientID: "01CLI000000000000000000001", OperatorID: "operator-1"})
	if err != nil || result.PlaintextSecret == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if repository.lastCreate.PreviousSecretPolicy != OAuthClientSecretRetirementKeep || repository.lastCreate.PreviousSecretGrace != 0 {
		t.Fatalf("default retirement = %q/%s, want KEEP with no grace window", repository.lastCreate.PreviousSecretPolicy, repository.lastCreate.PreviousSecretGrace)
	}
}

func TestCreateOAuthClientSecretRetiresPreviousSecretWhenConfigured(t *testing.T) {
	for _, test := range []struct {
		name        string
		retirement  OAuthClientSecretRetirement
		input       OAuthClientSecretCreateInput
		wantPolicy  OAuthClientSecretRetirementPolicy
		wantGrace   time.Duration
		wantRefused bool
	}{
		{
			name:       "service level revoke",
			retirement: OAuthClientSecretRetirement{Policy: OAuthClientSecretRetirementRevoke},
			wantPolicy: OAuthClientSecretRetirementRevoke,
		},
		{
			name:       "service level grace",
			retirement: OAuthClientSecretRetirement{Policy: OAuthClientSecretRetirementGrace, Grace: time.Hour},
			wantPolicy: OAuthClientSecretRetirementGrace,
			wantGrace:  time.Hour,
		},
		{
			name:       "explicit request overrides keep default",
			input:      OAuthClientSecretCreateInput{PreviousSecretPolicy: OAuthClientSecretRetirementGrace, PreviousSecretGrace: 30 * time.Minute},
			wantPolicy: OAuthClientSecretRetirementGrace,
			wantGrace:  30 * time.Minute,
		},
		{
			name:        "explicit request cannot disable configured retirement implicitly",
			retirement:  OAuthClientSecretRetirement{Policy: OAuthClientSecretRetirementRevoke},
			input:       OAuthClientSecretCreateInput{PreviousSecretPolicy: OAuthClientSecretRetirementGrace},
			wantRefused: true,
		},
		{
			name:        "unknown explicit policy refused",
			input:       OAuthClientSecretCreateInput{PreviousSecretPolicy: OAuthClientSecretRetirementPolicy("ALWAYS")},
			wantRefused: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &secretRetirementRepository{}
			service, err := NewOAuthClientManagementService(repository, fixedManagementIDs{}, staticManagementClock{}, RedirectURIValidationPolicy{}, WithOAuthClientSecretRetirement(test.retirement))
			if err != nil {
				t.Fatal(err)
			}
			input := test.input
			input.TenantID, input.OAuthClientID, input.OperatorID = "tenant-1", "01CLI000000000000000000001", "operator-1"
			_, err = service.CreateOAuthClientSecret(context.Background(), input)
			if test.wantRefused {
				if err == nil {
					t.Fatal("expected the retirement policy to be refused")
				}
				if repository.createCalls != 0 {
					t.Fatal("rejected policy must not reach the repository")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repository.lastCreate.PreviousSecretPolicy != test.wantPolicy || repository.lastCreate.PreviousSecretGrace != test.wantGrace {
				t.Fatalf("retirement = %q/%s, want %q/%s", repository.lastCreate.PreviousSecretPolicy, repository.lastCreate.PreviousSecretGrace, test.wantPolicy, test.wantGrace)
			}
		})
	}
}

// 非法服务级策略必须在构造期失败，而不是拖到第一次签发（那时旧密钥可能已被
// 误判为“继续可用”）。
func TestOAuthClientManagementServiceRejectsInvalidRetirementAtConstruction(t *testing.T) {
	for _, retirement := range []OAuthClientSecretRetirement{
		{Policy: OAuthClientSecretRetirementGrace},
		{Policy: OAuthClientSecretRetirementGrace, Grace: maxRotationOverlap + time.Second},
		{Policy: OAuthClientSecretRetirementPolicy("ALWAYS")},
	} {
		if _, err := NewOAuthClientManagementService(&secretRetirementRepository{}, fixedManagementIDs{}, staticManagementClock{}, RedirectURIValidationPolicy{}, WithOAuthClientSecretRetirement(retirement)); !errors.Is(err, ErrManagementValidation) {
			t.Fatalf("retirement %+v must be rejected at construction, err=%v", retirement, err)
		}
	}
}

type staticManagementClock struct{}

func (staticManagementClock) Now() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }

type fixedManagementIDs struct{}

func (fixedManagementIDs) New(time.Time) (string, error) { return "01CRED00000000000000000001", nil }

// secretRetirementRepository 只记录 CreateOAuthClientSecret 的入参，其余聚合写入
// 均不被这些用例触达，返回零值即可。
type secretRetirementRepository struct {
	createCalls int
	lastCreate  OAuthClientSecretCreateInput
}

func (repository *secretRetirementRepository) CreateOAuthClient(context.Context, OAuthClientCreateInput, string, *SecretWrite, time.Time) (OAuthClientView, error) {
	return OAuthClientView{}, nil
}

func (repository *secretRetirementRepository) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return nil, nil
}

func (repository *secretRetirementRepository) GetOAuthClient(context.Context, string, string) (OAuthClientView, error) {
	return OAuthClientView{}, nil
}

func (repository *secretRetirementRepository) GetOAuthClientByClientID(context.Context, string, string) (OAuthClientView, error) {
	return OAuthClientView{}, nil
}

func (repository *secretRetirementRepository) ReplaceOAuthClientScopes(context.Context, OAuthClientScopesUpdateInput, time.Time) (OAuthClientView, error) {
	return OAuthClientView{}, nil
}

func (repository *secretRetirementRepository) ReplaceOAuthClientRedirectURIs(context.Context, OAuthClientRedirectURIsUpdateInput, time.Time) (OAuthClientView, error) {
	return OAuthClientView{}, nil
}

func (repository *secretRetirementRepository) GetOAuthClientPostLogoutRedirectURIs(context.Context, string, string) (OAuthClientPostLogoutRedirectURIsView, error) {
	return OAuthClientPostLogoutRedirectURIsView{}, nil
}

func (repository *secretRetirementRepository) ReplaceOAuthClientPostLogoutRedirectURIs(context.Context, OAuthClientPostLogoutRedirectURIsUpdateInput, time.Time) (OAuthClientPostLogoutRedirectURIsView, error) {
	return OAuthClientPostLogoutRedirectURIsView{}, nil
}

func (repository *secretRetirementRepository) GetOAuthClientJWKs(context.Context, string, string) (OAuthClientJWKsView, error) {
	return OAuthClientJWKsView{}, nil
}

func (repository *secretRetirementRepository) ReplaceOAuthClientJWKs(context.Context, OAuthClientJWKsUpdateInput, time.Time) (OAuthClientJWKsView, error) {
	return OAuthClientJWKsView{}, nil
}

func (repository *secretRetirementRepository) DisableOAuthClient(context.Context, OAuthClientDisableInput, time.Time) (OAuthClientView, error) {
	return OAuthClientView{}, nil
}

func (repository *secretRetirementRepository) CreateOAuthClientSecret(_ context.Context, input OAuthClientSecretCreateInput, write SecretWrite, now time.Time) (OAuthClientCredentialView, error) {
	repository.createCalls++
	repository.lastCreate = input
	return OAuthClientCredentialView{ID: write.CredentialID, Fingerprint: write.Fingerprint, ValidFrom: now, ValidUntil: write.ValidUntil, Status: "ACTIVE"}, nil
}

func (repository *secretRetirementRepository) RotateOAuthClientSecret(context.Context, OAuthClientSecretRotateInput, SecretWrite, time.Time) (OAuthClientCredentialView, error) {
	return OAuthClientCredentialView{}, nil
}

func (repository *secretRetirementRepository) DisableOAuthClientSecret(context.Context, OAuthClientSecretDisableInput, time.Time) error {
	return nil
}
