package infrastructure

import (
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

func TestMapManagementErrorMapsTranslatedDuplicateKey(t *testing.T) {
	if got := mapManagementError(gorm.ErrDuplicatedKey); !errors.Is(got, application.ErrConflict) {
		t.Fatalf("mapManagementError(gorm.ErrDuplicatedKey) = %v, want ErrConflict", got)
	}
}

func TestMapOAuthClientManagementErrorMapsTranslatedDuplicateKey(t *testing.T) {
	if got := mapOAuthClientManagementError(gorm.ErrDuplicatedKey); !errors.Is(got, application.ErrManagementConflict) {
		t.Fatalf("mapOAuthClientManagementError(gorm.ErrDuplicatedKey) = %v, want ErrManagementConflict", got)
	}
}
