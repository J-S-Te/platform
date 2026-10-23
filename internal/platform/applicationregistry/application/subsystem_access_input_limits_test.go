package application

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSubsystemAccessSubmitRejectsOversizedFields(t *testing.T) {
	service := &SubsystemAccessService{}
	base := SubsystemAccessSubmitRequest{
		TenantID: "tenant", ApplicationCode: "app", EnvironmentCode: "dev",
		OperatorID: "operator", IdempotencyKey: "idem-1", PreviewVersion: "v1",
	}
	cases := []SubsystemAccessSubmitRequest{
		func() SubsystemAccessSubmitRequest {
			item := base
			item.PublicOrigin = "https://example.com/" + strings.Repeat("a", 500)
			return item
		}(),
		func() SubsystemAccessSubmitRequest {
			item := base
			item.AuthProvider = strings.Repeat("a", 65)
			return item
		}(),
		func() SubsystemAccessSubmitRequest { item := base; item.InitialAdminID = "not-an-ulid"; return item }(),
		func() SubsystemAccessSubmitRequest {
			item := base
			item.IdempotencyKey = strings.Repeat("a", 129)
			return item
		}(),
	}
	for index, input := range cases {
		if _, err := service.Submit(context.Background(), input); !errors.Is(err, ErrSubsystemAccessInvalidRequest) {
			t.Fatalf("case %d error = %v, want ErrSubsystemAccessInvalidRequest", index, err)
		}
	}
}
