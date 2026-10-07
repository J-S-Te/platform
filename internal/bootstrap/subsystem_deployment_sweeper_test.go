package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type sweepFakeStore struct {
	states      []application.SubsystemDeploymentState
	transitions []string
}

func (store *sweepFakeStore) ListInFlightSubsystemDeployments(context.Context) ([]application.SubsystemDeploymentState, error) {
	return store.states, nil
}

func (store *sweepFakeStore) TransitionSubsystemDeployment(_ context.Context, _, _, _ string, status, operation, errorCode, _ string, _ time.Time) error {
	store.transitions = append(store.transitions, status+"|"+operation+"|"+errorCode)
	return nil
}

func (store *sweepFakeStore) ClaimSubsystemDeployment(context.Context, string, string, string, string, time.Time) (uint64, error) {
	return 0, nil
}
func (store *sweepFakeStore) CompleteSubsystemDeployment(context.Context, string, string, string, uint64, string, string, string, string, time.Time) error {
	return nil
}
func (store *sweepFakeStore) RecoverStaleSubsystemDeployment(ctx context.Context, state application.SubsystemDeploymentState, cutoff, now time.Time) (bool, error) {
	operation := state.Operation
	if operation == "" {
		operation = "ONBOARD"
	}
	return true, store.TransitionSubsystemDeployment(ctx, state.TenantID, state.ApplicationCode, state.Environment, application.SubsystemDeploymentStatusFailed, operation, "DEPLOYMENT_INTERRUPTED", "", now)
}

func (store *sweepFakeStore) DiscardFailedSubsystemDeployment(context.Context, string, string, string, time.Time) error {
	return nil
}

func (store *sweepFakeStore) GetSubsystemDeploymentState(context.Context, string, string, string) (application.SubsystemDeploymentState, error) {
	return application.SubsystemDeploymentState{}, nil
}

func (store *sweepFakeStore) GetSubsystemDeploymentContext(context.Context, string, string, string) (application.SubsystemDeploymentState, error) {
	return application.SubsystemDeploymentState{}, nil
}

func (store *sweepFakeStore) MarkSubsystemInitialAccessAssigned(context.Context, string, string, string, string, time.Time) error {
	return nil
}

// 看护任务只收口超过 stale 阈值的在途行；仍在窗口内的编排绝不能被误杀。
func TestSubsystemDeploymentSweeperClosesOnlyStaleRows(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	stale := now.Add(-25 * time.Minute)
	store := &sweepFakeStore{states: []application.SubsystemDeploymentState{
		{TenantID: "t1", ApplicationCode: "contract_management", Environment: "prod", Status: application.SubsystemDeploymentStatusUpdating, Operation: "UPDATE", StartedAt: &stale},
		{TenantID: "t1", ApplicationCode: "customer_and_opportunity", Environment: "dev", Status: application.SubsystemDeploymentStatusUpdating, StartedAt: &now},
	}}
	runner, err := newSubsystemDeploymentSweeper(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, application.SubsystemDeploymentStaleAfter)
	if err != nil {
		t.Fatalf("construct sweeper: %v", err)
	}
	runner.sweep(context.Background())

	if len(store.transitions) != 1 {
		t.Fatalf("transitions = %#v, want exactly one closure", store.transitions)
	}
	if store.transitions[0] != application.SubsystemDeploymentStatusFailed+"|UPDATE|DEPLOYMENT_INTERRUPTED" {
		t.Fatalf("unexpected closure = %q", store.transitions[0])
	}
}
