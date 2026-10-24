package bootstrap

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identityinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	coordination "github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
)

// lazyHandoverLicenseReader 让交接编排在授权协调服务构造完成后再解析读取端；
// 解析失败时返回错误，由 inventory 归类为 UNREACHABLE（阻断），绝不静默放行。
type lazyHandoverLicenseReader struct {
	resolve func() handoverLicenseStatusReader
}

// Status 惰性解析授权协调服务的读取端并查询应用许可状态；未就绪时返回错误，
// 由 inventory 归类处理。
func (reader lazyHandoverLicenseReader) Status(ctx context.Context, application string) (coordination.Status, error) {
	if reader.resolve == nil {
		return coordination.Status{}, errors.New("handover license reader is not configured")
	}
	target := reader.resolve()
	if target == nil {
		return coordination.Status{}, errors.New("handover license reader is not ready")
	}
	return target.Status(ctx, application)
}

// handoverClientsFromConfig 按配置为每个子系统构造机器协议客户端。
func handoverClientsFromConfig(cfg config.Config, requestID func() (string, error)) (map[string]application.HandoverSystemClient, error) {
	clients := make(map[string]application.HandoverSystemClient, len(cfg.Handover.SystemURLs))
	for rawSystem, baseURL := range cfg.Handover.SystemURLs {
		system := strings.ToLower(strings.TrimSpace(rawSystem))
		client, err := identityinfrastructure.NewHandoverHTTPClient(identityinfrastructure.HandoverHTTPClientConfig{
			System: system, BaseURL: baseURL, MachineToken: cfg.Handover.MachineToken,
			// 平台是调用方；租户由请求体与机器凭据绑定共同确定，不写死在客户端配置里。
			ApplicationCode: "platform", Environment: cfg.Handover.Environment, Timeout: cfg.Handover.Timeout,
			RequestID: requestID,
		})
		if err != nil {
			return nil, err
		}
		clients[system] = client
	}
	return clients, nil
}

// buildHandoverCoordinator 在启用时组装离职交接编排；未启用时返回 nil，保持既有交接闸门行为。
func buildHandoverCoordinator(cfg config.Config, database *gorm.DB, registry handoverRegistryReader, licenses handoverLicenseStatusReader) (*application.HandoverOrchestrator, error) {
	if !cfg.Handover.Enabled {
		return nil, nil
	}
	if database == nil || registry == nil {
		return nil, errors.New("handover orchestration requires database and application registry")
	}
	repository, err := identityinfrastructure.NewPersonnelHandoverGORMRepository(database)
	if err != nil {
		return nil, err
	}
	inventory := &personnelHandoverInventory{
		registry: registry, licenses: licenses,
		systems: handoverSystemCodes(cfg), environment: cfg.Handover.Environment,
	}
	clients, err := handoverClientsFromConfig(cfg, func() (string, error) { return ulid.Generator{}.New(time.Now().UTC()) })
	if err != nil {
		return nil, err
	}
	return application.NewHandoverOrchestrator(repository, inventory, clients, ulid.Generator{}, application.SystemClock{})
}

// handoverSystemCodes 返回参与交接的系统编码（排序后保证稳定顺序）。
func handoverSystemCodes(cfg config.Config) []string {
	codes := make([]string, 0, len(cfg.Handover.SystemURLs))
	for system := range cfg.Handover.SystemURLs {
		codes = append(codes, strings.ToLower(strings.TrimSpace(system)))
	}
	if len(codes) == 0 {
		return nil
	}
	for i := 1; i < len(codes); i++ {
		for j := i; j > 0 && codes[j] < codes[j-1]; j-- {
			codes[j], codes[j-1] = codes[j-1], codes[j]
		}
	}
	return codes
}

// worker 只能核验交接并完成已执行离职的批次，不能人工选择、冻结、取消或派发。
type personnelWorkerHandover struct {
	application.HandoverCompletionVerifier
	application.HandoverCompletionFinisher
}

func (personnelWorkerHandover) Start(context.Context, string, string, string) (application.HandoverBatch, error) {
	return application.HandoverBatch{}, application.ErrConflict
}
func (personnelWorkerHandover) CancelByRequest(context.Context, string, string) error {
	return application.ErrConflict
}

type personnelWorkerInventory struct{}

func (personnelWorkerInventory) Systems(context.Context, string) ([]application.HandoverSystemState, error) {
	return nil, application.ErrConflict
}

// personnelWorkerFinishClient 在装配边界收窄机器客户端能力，防止 worker 意外扩大为转移端。
type personnelWorkerFinishClient struct {
	finisher interface {
		Finish(context.Context, string, string, string) error
	}
}

func (c personnelWorkerFinishClient) Finish(ctx context.Context, batchID, requestID, outcome string) error {
	if outcome != "COMPLETED" {
		return application.ErrConflict
	}
	return c.finisher.Finish(ctx, batchID, requestID, outcome)
}
func (personnelWorkerFinishClient) Freeze(context.Context, application.HandoverFreezeRequest) (application.HandoverFreezeResponse, error) {
	return application.HandoverFreezeResponse{}, application.ErrConflict
}
func (personnelWorkerFinishClient) Responsibilities(context.Context, string, string, int) (application.HandoverResponsibilityPage, error) {
	return application.HandoverResponsibilityPage{}, application.ErrConflict
}
func (personnelWorkerFinishClient) Transfer(context.Context, application.HandoverTransferRequest) (application.HandoverTransferReceipt, error) {
	return application.HandoverTransferReceipt{}, application.ErrConflict
}
func (personnelWorkerFinishClient) Receipt(context.Context, string, string) (application.HandoverTransferReceipt, error) {
	return application.HandoverTransferReceipt{}, application.ErrConflict
}

func buildPersonnelWorkerService(cfg config.Config, database *gorm.DB) (*application.PersonnelChangeService, error) {
	checker, err := identityinfrastructure.NewPersonnelHandoverGORMChecker(database)
	if err != nil {
		return nil, err
	}
	service, err := application.NewPersonnelChangeService(identityinfrastructure.NewPersonnelChangeGORMRepository(database), ulid.Generator{}, application.SystemClock{}, checker)
	if err != nil {
		return nil, err
	}
	if cfg.Handover.Enabled {
		repository, err := identityinfrastructure.NewPersonnelHandoverGORMRepository(database)
		if err != nil {
			return nil, err
		}
		clients, err := handoverClientsFromConfig(cfg, func() (string, error) { return ulid.Generator{}.New(time.Now().UTC()) })
		if err != nil {
			return nil, err
		}
		for system, client := range clients {
			clients[system] = personnelWorkerFinishClient{finisher: client}
		}
		verifier, err := application.NewHandoverOrchestrator(repository, personnelWorkerInventory{}, clients, ulid.Generator{}, application.SystemClock{})
		if err != nil {
			return nil, err
		}
		service.SetHandoverCoordinator(personnelWorkerHandover{HandoverCompletionVerifier: verifier, HandoverCompletionFinisher: verifier})
	}
	return service, nil
}
