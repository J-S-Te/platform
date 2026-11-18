package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

// inboxScopeTestRepository 只实现本次新增的读路径，其余接口方法由嵌入的 nil 接口兜底。
type inboxScopeTestRepository struct {
	Repository
	tenantID string
	userID   string
	page     PageRequest
	now      time.Time
	updated  int64
	batchIDs []string
	bySource []UnreadSource
	badgeErr error
}

func (r *inboxScopeTestRepository) CountUnread(_ context.Context, tenantID, userID string, page PageRequest, now time.Time) (int64, error) {
	r.tenantID, r.userID, r.page, r.now = tenantID, userID, page, now
	return 7, nil
}

func (r *inboxScopeTestRepository) CountUnreadBySource(_ context.Context, tenantID, userID string, now time.Time) ([]UnreadSource, error) {
	r.tenantID, r.userID, r.now = tenantID, userID, now
	return r.bySource, r.badgeErr
}

func (r *inboxScopeTestRepository) MarkReadBatch(_ context.Context, tenantID, userID string, deliveryIDs []string, now time.Time) (int64, error) {
	r.tenantID, r.userID, r.batchIDs, r.now = tenantID, userID, deliveryIDs, now
	return r.updated, nil
}

func newInboxScopeService(t *testing.T, repo Repository) *Service {
	t.Helper()
	service, err := NewService(repo, ingestionTestPolicy{enabled: true}, ingestionTestResolver{}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// TestServiceCountUnreadUsesInboxScope 锁定未读计数与列表共用 source/exclude 口径，
// 且主体标识在应用层裁剪后再传给仓储。
func TestServiceCountUnreadUsesInboxScope(t *testing.T) {
	repo := &inboxScopeTestRepository{}
	service := newInboxScopeService(t, repo)
	count, err := service.CountUnread(context.Background(), " tenant-a ", " user-1 ", PageRequest{SourceApplication: " contract_management "})
	if err != nil {
		t.Fatalf("count unread: %v", err)
	}
	if count != 7 {
		t.Fatalf("count = %d, want 7", count)
	}
	if repo.tenantID != "tenant-a" || repo.userID != "user-1" {
		t.Fatalf("identity = %q/%q", repo.tenantID, repo.userID)
	}
	if repo.page.SourceApplication != "contract_management" {
		t.Fatalf("source = %q, want contract_management", repo.page.SourceApplication)
	}
	if !repo.now.Equal(ingestionTestClock{}.Now()) {
		t.Fatalf("clock = %s, want service clock", repo.now)
	}
}

// TestServiceCountUnreadRejectsConflictingScope 断言“本系统”和“跨系统”互斥，
// 不允许静默取其中一个而让徽标口径偏离列表。
func TestServiceCountUnreadRejectsConflictingScope(t *testing.T) {
	service := newInboxScopeService(t, &inboxScopeTestRepository{})
	_, err := service.CountUnread(context.Background(), "tenant-a", "user-1", PageRequest{SourceApplication: "a", ExcludeSourceApplication: "b"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("conflicting scope error = %v, want ErrValidation", err)
	}
	if _, err := service.CountUnread(context.Background(), " ", "user-1", PageRequest{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing tenant error = %v, want ErrValidation", err)
	}
}

// TestServiceUnreadBadgeSumsSources 断言徽标总数等于分来源之和，空来源返回空数组而不是 nil。
func TestServiceUnreadBadgeSumsSources(t *testing.T) {
	repo := &inboxScopeTestRepository{bySource: []UnreadSource{{SourceApplication: "contract_management", UnreadCount: 2}, {SourceApplication: "project_management", UnreadCount: 3}}}
	service := newInboxScopeService(t, repo)
	badge, err := service.UnreadBadge(context.Background(), "tenant-a", "user-1")
	if err != nil {
		t.Fatalf("badge: %v", err)
	}
	if badge.Total != 5 || len(badge.BySource) != 2 {
		t.Fatalf("badge = %+v, want total 5 and 2 sources", badge)
	}

	emptyRepo := &inboxScopeTestRepository{}
	emptyService := newInboxScopeService(t, emptyRepo)
	empty, err := emptyService.UnreadBadge(context.Background(), "tenant-a", "user-1")
	if err != nil {
		t.Fatalf("empty badge: %v", err)
	}
	if empty.Total != 0 || empty.BySource == nil || len(empty.BySource) != 0 {
		t.Fatalf("empty badge = %+v, want non-nil empty slice", empty)
	}
}

// TestServiceMarkReadBatchDeduplicatesAndBounds 断言批量已读去重、裁剪空白，
// 并在空集合或超过上限时失败关闭，绝不把客户端数组直接传给仓储。
func TestServiceMarkReadBatchDeduplicatesAndBounds(t *testing.T) {
	repo := &inboxScopeTestRepository{updated: 2}
	service := newInboxScopeService(t, repo)
	updated, err := service.MarkReadBatch(context.Background(), " tenant-a ", " user-1 ", []string{" d-1 ", "d-1", "d-2"})
	if err != nil {
		t.Fatalf("batch read: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2", updated)
	}
	if len(repo.batchIDs) != 2 || repo.batchIDs[0] != "d-1" || repo.batchIDs[1] != "d-2" {
		t.Fatalf("batch ids = %#v, want deduplicated d-1,d-2", repo.batchIDs)
	}

	if _, err := service.MarkReadBatch(context.Background(), "tenant-a", "user-1", nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty batch error = %v, want ErrValidation", err)
	}
	if _, err := service.MarkReadBatch(context.Background(), "tenant-a", "user-1", []string{"d-1", " "}); !errors.Is(err, ErrValidation) {
		t.Fatalf("blank id error = %v, want ErrValidation", err)
	}
	tooMany := make([]string, maximumBatchReadSize+1)
	for i := range tooMany {
		tooMany[i] = time.Duration(i).String()
	}
	if _, err := service.MarkReadBatch(context.Background(), "tenant-a", "user-1", tooMany); !errors.Is(err, ErrValidation) {
		t.Fatalf("oversized batch error = %v, want ErrValidation", err)
	}
}
