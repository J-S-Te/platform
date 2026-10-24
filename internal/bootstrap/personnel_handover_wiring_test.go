package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"gorm.io/gorm"
)

func handoverConfigForTest(enabled bool, urls string) config.Config {
	return config.Config{Handover: config.HandoverConfig{
		Enabled: enabled, Environment: "prod", MachineToken: "machine-token-value",
		SystemURLs: handoverSystemURLsForTest(urls), Timeout: 5 * time.Second,
	}}
}

type workerFinishBoundary struct{ calls int }

func (c *workerFinishBoundary) Finish(_ context.Context, batch, request, outcome string) error {
	if batch != "batch-1" || request != "request-1" || outcome != "COMPLETED" {
		return errors.New("unexpected finish identity")
	}
	c.calls++
	return nil
}

func TestPersonnelWorkerHandoverCannotStartCancelOrTransfer(t *testing.T) {
	ctx := context.Background()
	worker := personnelWorkerHandover{}
	if _, err := worker.Start(ctx, "tenant", "request", "user"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can start handover")
	}
	if err := worker.CancelByRequest(ctx, "tenant", "request"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can cancel handover")
	}
	boundary := &workerFinishBoundary{}
	client := personnelWorkerFinishClient{finisher: boundary}
	if _, err := client.Freeze(ctx, application.HandoverFreezeRequest{}); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can freeze")
	}
	if _, err := client.Responsibilities(ctx, "request", "", 1); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can discover")
	}
	if _, err := client.Transfer(ctx, application.HandoverTransferRequest{}); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can transfer")
	}
	if _, err := client.Receipt(ctx, "item", "key"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can reconcile transfers")
	}
	if err := client.Finish(ctx, "batch-1", "request-1", "CANCELLED"); !errors.Is(err, application.ErrConflict) {
		t.Fatal("worker can release cancellation")
	}
	if boundary.calls != 0 {
		t.Fatal("denied operation reached machine client")
	}
	if err := client.Finish(ctx, "batch-1", "request-1", "COMPLETED"); err != nil || boundary.calls != 1 {
		t.Fatalf("finish unavailable: calls=%d err=%v", boundary.calls, err)
	}
}

func TestPersonnelWorkerCompletionRequiresConfiguredCredential(t *testing.T) {
	cfg := handoverConfigForTest(true, "contract=https://contract.internal")
	cfg.Handover.MachineToken = ""
	if _, err := buildPersonnelWorkerService(cfg, &gorm.DB{}); err == nil {
		t.Fatal("worker completion silently accepts a missing credential")
	}
}

func handoverSystemURLsForTest(raw string) map[string]string {
	systems := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		code, target, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			continue
		}
		systems[code] = target
	}
	return systems
}

func TestBuildHandoverCoordinatorSkipsWhenDisabled(t *testing.T) {
	coordinator, err := buildHandoverCoordinator(handoverConfigForTest(false, "contract=https://contract.internal"), &gorm.DB{}, inventoryRegistryFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator != nil {
		t.Fatal("disabled handover must not inject a coordinator")
	}
}

func TestBuildHandoverCoordinatorBuildsClientsPerSystem(t *testing.T) {
	clients, err := handoverClientsFromConfig(handoverConfigForTest(true, "Contract=https://contract.internal,crm=https://crm.internal"), func() (string, error) { return "01REQUESTID0000000000000000", nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 || clients["contract"] == nil || clients["crm"] == nil {
		t.Fatalf("clients = %v", clients)
	}
}

func TestBuildHandoverCoordinatorFailsClosedWithoutCredential(t *testing.T) {
	cfg := handoverConfigForTest(true, "contract=https://contract.internal")
	cfg.Handover.MachineToken = ""
	if _, err := handoverClientsFromConfig(cfg, func() (string, error) { return "01REQUESTID0000000000000000", nil }); err == nil {
		t.Fatal("missing machine credential must fail closed even if config validation is bypassed")
	}
}

func TestBuildHandoverCoordinatorRequiresRegistry(t *testing.T) {
	if _, err := buildHandoverCoordinator(handoverConfigForTest(true, "contract=https://contract.internal"), &gorm.DB{}, nil, nil); err == nil {
		t.Fatal("missing application registry must fail closed")
	}
}

func TestBuildHandoverCoordinatorAssemblesOrchestrator(t *testing.T) {
	coordinator, err := buildHandoverCoordinator(handoverConfigForTest(true, "contract=https://contract.internal"), &gorm.DB{}, inventoryRegistryFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator == nil {
		t.Fatal("enabled handover must assemble an orchestrator")
	}
	// 未就绪的授权读取端必须报错（inventory 会归类为 UNREACHABLE），不得静默放行。
	if _, err := (lazyHandoverLicenseReader{}).Status(context.Background(), "contract_management"); err == nil {
		t.Fatal("unresolved license reader must return an error")
	}
}

func TestPersonnelWorkerWiringIncludesHandoverVerifierWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{Handover: config.HandoverConfig{Enabled: enabled}}
		service, err := buildPersonnelWorkerService(cfg, &gorm.DB{})
		if err != nil {
			t.Fatal(err)
		}
		if service.HandoverOrchestrationEnabled() != enabled {
			t.Fatalf("worker cross-system verifier enabled=%v", enabled)
		}
	}
	if _, err := buildPersonnelWorkerService(config.Config{}, nil); err == nil {
		t.Fatal("worker missing handover checker must fail startup")
	}
}
