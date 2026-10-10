package application

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
)

type Transaction interface {
	Deployment() *domain.Deployment
	SaveDeployment(*domain.Deployment) error
	Artifact(string) (domain.Artifact, error)
	AddArtifact(domain.Artifact) error
	AddEvent(domain.Event) error
	ConsumeRecovery(domain.ClockRecovery) error
}
type Repository interface {
	Lock(context.Context, func(Transaction) error) error
	Events(context.Context, int, int) (domain.EventPage, error)
}
