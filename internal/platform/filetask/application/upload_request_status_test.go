package application

import (
	"context"
	"testing"
)

func TestUploadRequestResolutionCapabilityRequired(t *testing.T) {
	service := newUploadSessionService(t, newUploadSessionRepositoryStub(), &uploadSessionStoreStub{})
	resolver, ok := any(service).(interface {
		ResolveUploadRequest(context.Context, string, string, string) (string, string, error)
	})
	if !ok {
		t.Fatal("file service cannot resolve a failed upload request without resending bytes")
	}
	if _, _, err := resolver.ResolveUploadRequest(context.Background(), "tenant", "app", ""); err == nil {
		t.Fatal("empty request identity accepted")
	}
}

func TestUploadRequestRetirementCapabilityRequired(t *testing.T) {
	service := newUploadSessionService(t, newUploadSessionRepositoryStub(), &uploadSessionStoreStub{})
	if _, ok := any(service).(interface {
		RetireUploadRequest(context.Context, string, string, string) (string, string, error)
	}); !ok {
		t.Fatal("file service cannot fence a late upload reservation after a missing request")
	}
}
