package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

type deleteReadTestRepository struct {
	Repository
	tenantID string
	userID   string
	now      time.Time
}

func (r *deleteReadTestRepository) DeleteReadInbox(_ context.Context, tenantID, userID string, now time.Time) (int64, error) {
	r.tenantID, r.userID, r.now = tenantID, userID, now
	return 3, nil
}

// TestServiceDeleteReadTrimsIdentityAndDelegates 锁定删除已读只作用于当前租户/用户，
// 并使用服务时钟，避免把客户端传入的主体或时间透传到仓储。
func TestServiceDeleteReadTrimsIdentityAndDelegates(t *testing.T) {
	repo := &deleteReadTestRepository{}
	service, err := NewService(repo, ingestionTestPolicy{enabled: true}, ingestionTestResolver{}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	count, err := service.DeleteRead(context.Background(), " tenant-a ", " user-1 ")
	if err != nil {
		t.Fatalf("delete read: %v", err)
	}
	if count != 3 {
		t.Fatalf("deleted count = %d, want 3", count)
	}
	if repo.tenantID != "tenant-a" || repo.userID != "user-1" {
		t.Fatalf("repository identity = %q/%q, want tenant-a/user-1", repo.tenantID, repo.userID)
	}
	if !repo.now.Equal(ingestionTestClock{}.Now()) {
		t.Fatalf("repository clock = %s, want service clock", repo.now)
	}
}

// TestServiceDeleteReadRejectsMissingIdentity 断言缺少租户或用户时失败关闭，不触碰仓储。
func TestServiceDeleteReadRejectsMissingIdentity(t *testing.T) {
	repo := &deleteReadTestRepository{}
	service, err := NewService(repo, ingestionTestPolicy{enabled: true}, ingestionTestResolver{}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteRead(context.Background(), " ", "user-1"); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing tenant error = %v, want ErrValidation", err)
	}
	if _, err := service.DeleteRead(context.Background(), "tenant-a", " "); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing user error = %v, want ErrValidation", err)
	}
}
