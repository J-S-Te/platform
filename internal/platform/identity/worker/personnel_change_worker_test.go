package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type personnelRepoStub struct {
	request  application.PersonnelChangeRequest
	executed int
}

func (r *personnelRepoStub) Create(context.Context, application.PersonnelChangeRequest) (application.PersonnelChangeRequest, error) {
	return application.PersonnelChangeRequest{}, nil
}
func (r *personnelRepoStub) List(_ context.Context, _, status, _, _ string) ([]application.PersonnelChangeRequest, error) {
	if status != domain.PersonnelChangeScheduled {
		return nil, nil
	}
	return []application.PersonnelChangeRequest{r.request}, nil
}
func (r *personnelRepoStub) Get(context.Context, string, string) (application.PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *personnelRepoStub) UpdateStatus(context.Context, application.PersonnelChangeRequest, string, string, time.Time) (application.PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *personnelRepoStub) Execute(_ context.Context, request application.PersonnelChangeRequest, _ string, now time.Time) (application.PersonnelChangeRequest, error) {
	r.executed++
	request.Status = domain.PersonnelChangeExecuted
	request.ExecutedAt = &now
	return request, nil
}
func (r *personnelRepoStub) PreviewPermissions(context.Context, application.PersonnelChangeRequest) (application.PersonnelChangePermissionPreview, error) {
	return application.PersonnelChangePermissionPreview{}, nil
}

type personnelIDStub struct{}

func (personnelIDStub) New(time.Time) (string, error) { return "01J00000000000000000000000", nil }

func TestPersonnelChangeWorkerProcessesDueOnly(t *testing.T) {
	now := time.Now().UTC()
	repo := &personnelRepoStub{request: application.PersonnelChangeRequest{ID: "01J00000000000000000000001", TenantID: "01J00000000000000000000000", UserID: "01J00000000000000000000002", Status: domain.PersonnelChangeScheduled, EffectiveAt: ptr(now.Add(-time.Minute))}}
	service, err := application.NewPersonnelChangeService(repo, personnelIDStub{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewPersonnelChangeWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.ProcessDue(context.Background()); got != 1 || repo.executed != 1 {
		t.Fatalf("processed=%d executed=%d, want 1/1", got, repo.executed)
	}

	repo.request.EffectiveAt = ptr(now.Add(time.Hour))
	if got := w.ProcessDue(context.Background()); got != 0 || repo.executed != 1 {
		t.Fatalf("future processed=%d executed=%d, want 0/1", got, repo.executed)
	}
}

func ptr(t time.Time) *time.Time { return &t }
