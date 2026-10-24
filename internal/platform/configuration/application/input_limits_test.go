package application

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestConfigurationNamespaceAndItemLengthLimits(t *testing.T) {
	service := &Service{}
	_, err := service.CreateNamespace(context.Background(), NamespaceCreateInput{
		TenantID: "tenant", OperatorID: "operator", ApplicationCode: "app", Code: "namespace",
		Name: strings.Repeat("名", 129),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong namespace name error = %v", err)
	}
	_, err = service.CreateNamespace(context.Background(), NamespaceCreateInput{
		TenantID: "tenant", OperatorID: "operator", ApplicationCode: "app", Code: "namespace",
		Name: "名称", Description: strings.Repeat("说", 1001),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong namespace description error = %v", err)
	}
	if err := validateItem("namespace", "key", "STRING", strings.Repeat("值", 20001), false); !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong string item error = %v", err)
	}
}
