package infrastructure

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) (*Repository, error) {
	if db == nil {
		return nil, domain.ErrInvalid
	}
	return &Repository{db: db}, nil
}

type transaction struct {
	db         *gorm.DB
	deployment *domain.Deployment
}

func (r *Repository) Lock(ctx context.Context, fn func(application.Transaction) error) error {
	err := r.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		var d domain.Deployment
		t := &transaction{db: db}
		err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", 1).Take(&d).Error
		if err == nil {
			t.deployment = &d
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return fn(t)
	})
	return classify(err)
}
func classify(err error) error {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1062 || mysqlErr.Number == 1213 || mysqlErr.Number == 1205) {
		return domain.ErrConflict
	}
	return err
}
func (t *transaction) Deployment() *domain.Deployment { return t.deployment }
func (t *transaction) SaveDeployment(d *domain.Deployment) error {
	var err error
	if t.deployment == nil {
		err = t.db.Create(d).Error
	} else {
		err = t.db.Model(&domain.Deployment{}).Where("id = ?", 1).Select("*").Updates(d).Error
	}
	if err == nil {
		t.deployment = d
	}
	return err
}
func (t *transaction) Artifact(digest string) (domain.Artifact, error) {
	var a domain.Artifact
	err := t.db.Where("digest = ?", digest).Take(&a).Error
	return a, err
}
func (t *transaction) AddArtifact(a domain.Artifact) error { return t.db.Create(&a).Error }
func (t *transaction) AddEvent(e domain.Event) error       { return t.db.Create(&e).Error }
func (t *transaction) ConsumeRecovery(r domain.ClockRecovery) error {
	return classify(t.db.Create(&r).Error)
}
func (r *Repository) Events(ctx context.Context, page, size int) (domain.EventPage, error) {
	out := domain.EventPage{Items: []domain.Event{}, Page: page, PageSize: size}
	db := r.db.WithContext(ctx).Model(&domain.Event{})
	if err := db.Count(&out.Total).Error; err != nil {
		return out, err
	}
	err := db.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&out.Items).Error
	return out, err
}

var _ application.Repository = (*Repository)(nil)
