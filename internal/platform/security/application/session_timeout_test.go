package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/security/domain"
)

type sessionTimeoutRepositoryStub struct {
	policy  domain.LoginPolicy
	err     error
	updates int
}

func (stub *sessionTimeoutRepositoryStub) GetLoginPolicy(context.Context, string) (domain.LoginPolicy, error) {
	return stub.policy, stub.err
}

// UpdateLoginPolicy 记录服务层是否接受并写入测试策略。
// 参数：ctx为请求上下文；input为管理员输入；now为审计记录时间。
// 返回值：模拟持久化后的策略与注入错误，不连接数据库。
func (stub *sessionTimeoutRepositoryStub) UpdateLoginPolicy(ctx context.Context, input LoginPolicyUpdateInput, now time.Time) (domain.LoginPolicy, error) {
	stub.updates++
	return domain.LoginPolicy{TenantID: input.TenantID, IdleTimeoutSeconds: input.IdleTimeoutSeconds}, stub.err
}

func (stub *sessionTimeoutRepositoryStub) RecordFailedLogin(context.Context, LoginFailureInput, domain.LoginPolicy, time.Time) (LoginFailureResult, error) {
	return LoginFailureResult{}, errors.New("not implemented")
}

func (stub *sessionTimeoutRepositoryStub) ListLockedAccounts(context.Context, string, PageRequest, time.Time) (PageResult[domain.LockedAccount], error) {
	return PageResult[domain.LockedAccount]{}, errors.New("not implemented")
}

func (stub *sessionTimeoutRepositoryStub) UnlockAccount(context.Context, UnlockInput, time.Time) (domain.LockedAccount, error) {
	return domain.LockedAccount{}, errors.New("not implemented")
}

type sessionTimeoutClockStub struct{}

func (sessionTimeoutClockStub) Now() time.Time {
	return time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
}

func TestSessionIdleTimeoutUsesPersistedPolicyAndDefault(t *testing.T) {
	t.Parallel()

	repository := &sessionTimeoutRepositoryStub{policy: domain.LoginPolicy{TenantID: "tenant-1", IdleTimeoutSeconds: 900}}
	service, err := NewService(repository, sessionTimeoutClockStub{})
	if err != nil {
		t.Fatalf("construct security service: %v", err)
	}

	timeout, err := service.SessionIdleTimeout(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("read persisted idle timeout: %v", err)
	}
	if timeout != 15*time.Minute {
		t.Fatalf("persisted idle timeout = %s, want 15m", timeout)
	}

	repository.err = ErrNotFound
	timeout, err = service.SessionIdleTimeout(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("read default idle timeout: %v", err)
	}
	if timeout != 30*time.Minute {
		t.Fatalf("default idle timeout = %s, want 30m", timeout)
	}
}

// TestUpdateLoginPolicyRejectsUnsafeIdleTimeout 保证保存端不能写入中央执行器拒绝的空闲窗口。
// 参数：t为测试上下文，覆盖下界、上界以及原来允许的一小时和一天。
// 返回值：无；越界值到达持久化层或未返回校验错误时失败。
func TestUpdateLoginPolicyRejectsUnsafeIdleTimeout(t *testing.T) {
	t.Parallel()
	for _, candidate := range []struct {
		name    string
		seconds uint
	}{
		{"below_minimum", 59}, {"above_maximum", 1801}, {"one_hour", 3600}, {"one_day", 86400},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			repository := &sessionTimeoutRepositoryStub{}
			service, err := NewService(repository, sessionTimeoutClockStub{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.UpdateLoginPolicy(context.Background(), LoginPolicyUpdateInput{
				TenantID: "tenant-1", OperatorID: "admin-1", MaxFailedAttempts: 5,
				LockoutDurationSeconds: 900, FailureResetWindowSeconds: 1800,
				IdleTimeoutSeconds: candidate.seconds,
			})
			if !errors.Is(err, ErrValidation) || repository.updates != 0 {
				t.Fatalf("unsafe policy: error=%v writes=%d", err, repository.updates)
			}
		})
	}
}

// TestUpdateLoginPolicyAcceptsThirtyMinutes 保证目标三十分钟窗口可以正常持久化。
// 参数：t为测试上下文。
// 返回值：无；服务拒绝目标值或未调用持久化层时失败。
func TestUpdateLoginPolicyAcceptsThirtyMinutes(t *testing.T) {
	t.Parallel()
	repository := &sessionTimeoutRepositoryStub{}
	service, err := NewService(repository, sessionTimeoutClockStub{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.UpdateLoginPolicy(context.Background(), LoginPolicyUpdateInput{
		TenantID: "tenant-1", OperatorID: "admin-1", MaxFailedAttempts: 5,
		LockoutDurationSeconds: 900, FailureResetWindowSeconds: 1800, IdleTimeoutSeconds: 1800,
	})
	if err != nil || result.IdleTimeoutSeconds != 1800 || repository.updates != 1 {
		t.Fatalf("target policy: error=%v writes=%d timeout=%d", err, repository.updates, result.IdleTimeoutSeconds)
	}
}
