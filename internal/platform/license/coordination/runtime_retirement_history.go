package coordination

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"gorm.io/gorm"
)

// RuntimeRetirementNotRequired is internal deployment evidence, not public
// Status. Tombstones deliberately remain in this query: retrying an interrupted
// deployment must not turn an unfinished retirement into permission to skip it.
func (s *Service) RuntimeRetirementNotRequired(ctx context.Context, app string, revision uint64, selected []string) (bool, error) {
	if !validApplication(app) || revision == 0 || len(selected) == 0 || len(selected) > 64 {
		return false, ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range selected {
		if !identifier.MatchString(id) || seen[id] {
			return false, ErrInvalid
		}
		seen[id] = true
	}
	safe := false
	err := s.lock(ctx, func(tx *gorm.DB, _ *domain.Deployment, _ domain.State) error {
		var current Application
		if err := tx.First(&current, "application = ?", app).Error; err != nil {
			return err
		}
		if current.Revision != revision {
			return ErrConflict
		}
		var removed int64
		if err := tx.Model(&Member{}).Where("application = ? AND service_id NOT IN ?", app, selected).Count(&removed).Error; err != nil {
			return err
		}
		safe = removed == 0
		return nil
	})
	return safe, err
}
