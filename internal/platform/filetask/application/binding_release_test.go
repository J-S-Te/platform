package application

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

type releaseRepository struct {
	*bindingRepositoryStub
	releasedBinding domain.FileBinding
	releaseError    error
}

func (r *releaseRepository) ReleaseBinding(_ context.Context, b domain.FileBinding) error {
	if r.releaseError != nil {
		return r.releaseError
	}
	r.releasedBinding = b
	return nil
}

func TestReleaseBindingValidatesCoordinatesAndPreservesFailure(t *testing.T) {
	r := &releaseRepository{bindingRepositoryStub: &bindingRepositoryStub{}}
	s, err := NewFileService(r, bindingStoreStub{}, fixedIDGenerator{id: "release-id"}, fixedClock{}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	input := BindingInput{TenantID: "tenant-1", ApplicationID: "app-1", FileID: "file-1", ResourceType: "source", ResourceID: "generation-1", BindingType: "SOURCE", OperatorUserID: "app-1"}
	if err := s.ReleaseResourceBinding(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if b := r.releasedBinding; b.TenantID != "tenant-1" || b.ApplicationID != "app-1" || b.FileID != "file-1" || b.ResourceID != "generation-1" || b.Status != "DISABLED" {
		t.Fatalf("incorrect retirement coordinates: %+v", b)
	}
	input.ResourceID = ""
	if err := s.ReleaseResourceBinding(context.Background(), input); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing resource err=%v", err)
	}
	input.ResourceID = "generation-1"
	r.releaseError = ErrStorage
	if err := s.ReleaseResourceBinding(context.Background(), input); !errors.Is(err, ErrStorage) {
		t.Fatalf("storage error swallowed: %v", err)
	}
}
