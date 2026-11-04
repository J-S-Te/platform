package credentialcrypto

import (
	"context"
	"errors"
	"testing"
	"time"
)

type challengeRepoStub struct {
	created       Challenge
	consumed      Challenge
	err           error
	deletedCutoff time.Time
	deletedLimit  int
}

func (r *challengeRepoStub) Create(_ context.Context, challenge Challenge) error {
	r.created = challenge
	return r.err
}
func (r *challengeRepoStub) Consume(_ context.Context, challenge Challenge) error {
	r.consumed = challenge
	return r.err
}
func (r *challengeRepoStub) DeleteExpired(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	r.deletedCutoff, r.deletedLimit = cutoff, limit
	return 3, r.err
}

func TestChallengeIssueAndConsumeBindings(t *testing.T) {
	repo := &challengeRepoStub{}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	service := &ChallengeService{Repository: repo, Now: func() time.Time { return now }, TTL: 60 * time.Second}
	binding := HashBinding("preauth-cookie")
	issued, err := service.Issue(context.Background(), "kid-1", PurposeLogin, "", "", binding)
	if err != nil {
		t.Fatal(err)
	}
	if issued.ExpiresAt.Sub(now) != 60*time.Second || len(repo.created.IDHash) != 32 {
		t.Fatal("unexpected challenge")
	}
	if repo.created.ResourceID != "" || repo.created.RouteKey != "auth.login" || repo.created.HTTPMethod != "POST" {
		t.Fatal("binding mismatch")
	}
	if err := service.Consume(context.Background(), issued.Raw, "kid-1", PurposeLogin, "", "", binding); err != nil {
		t.Fatal(err)
	}
	if repo.consumed.RouteKey != "auth.login" || len(repo.consumed.BindingHash) != 32 {
		t.Fatal("consume binding mismatch")
	}
}

func TestChallengeRejectsWrongResourceAndPurpose(t *testing.T) {
	service := &ChallengeService{Repository: &challengeRepoStub{}, Now: time.Now, TTL: 60 * time.Second}
	if _, err := service.Issue(context.Background(), "kid-1", PurposeReset, "", "tenant", HashBinding("x")); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatal("resource-bound purpose accepted empty resource")
	}
	if _, err := service.Issue(context.Background(), "kid-1", PurposeLogin, "account-1", "", HashBinding("x")); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatal("login accepted resource")
	}
}

// 安全审查 M-9：清理点必须晚一个实际发放 TTL，而不是“此刻之前全部过期行”。
// 若 cutoff 回到 now（或任何短于有效 TTL 的值），过期挑战会在客户端重放前被删除，
// 消费路径的错误码就从 CHALLENGE_EXPIRED 退化成 CHALLENGE_INVALID。
func TestChallengeCleanupIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		ttl       time.Duration
		wantGrace time.Duration
	}{
		{name: "default TTL grace period", ttl: 60 * time.Second, wantGrace: DefaultChallengeTTL},
		{name: "configured longer TTL grace period", ttl: MaxChallengeTTL, wantGrace: MaxChallengeTTL},
		{name: "unset TTL falls back to default grace period", ttl: 0, wantGrace: DefaultChallengeTTL},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &challengeRepoStub{}
			service := &ChallengeService{Repository: repo, Now: func() time.Time { return now }, TTL: test.ttl}
			n, err := service.Cleanup(context.Background())
			if err != nil || n != 3 || repo.deletedLimit != MaxCleanupBatch {
				t.Fatalf("unexpected cleanup: n=%d err=%v limit=%d", n, err, repo.deletedLimit)
			}
			if !repo.deletedCutoff.Equal(now.Add(-test.wantGrace)) {
				t.Fatalf("cleanup cutoff = %v, want %v", repo.deletedCutoff, now.Add(-test.wantGrace))
			}
			if !repo.deletedCutoff.Before(now) {
				t.Fatal("cleanup cutoff must keep a grace period instead of deleting every expired challenge")
			}
		})
	}
}
