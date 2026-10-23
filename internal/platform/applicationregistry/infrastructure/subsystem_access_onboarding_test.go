package infrastructure

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type onboardingServiceStub struct {
	preflightErr  error
	registerErr   error
	registerCalls int
	seenOnboard   application.SubsystemOnboardingInput
	seenDirectory application.SubsystemDirectoryRegistrationInput
}

func (stub *onboardingServiceStub) PreflightValidate(_ context.Context, input application.SubsystemOnboardingInput) error {
	stub.seenOnboard = input
	return stub.preflightErr
}

func (stub *onboardingServiceStub) RegisterSubsystemDirectory(_ context.Context, input application.SubsystemDirectoryRegistrationInput) (application.SubsystemDirectoryRegistrationResult, error) {
	stub.registerCalls++
	stub.seenDirectory = input
	if stub.registerErr != nil {
		return application.SubsystemDirectoryRegistrationResult{}, stub.registerErr
	}
	return application.SubsystemDirectoryRegistrationResult{
		Application: application.Application{ID: "01APP000000000000000000001"},
		Environment: application.Environment{ID: "01ENV000000000000000000001"},
		PublicURL:   "https://settlement.example.com"}, nil
}

type registryLookupStub struct {
	applicationID string
	environmentID string
	found         bool
	err           error
}

func (stub registryLookupStub) LookupSubsystemAccessRegistration(context.Context, string, string, string) (string, string, bool, error) {
	return stub.applicationID, stub.environmentID, stub.found, stub.err
}

func accessCatalogWithTarget() accessCatalogStub {
	return accessCatalogStub{capabilities: application.SubsystemProvisioningCapabilities{
		Mode: "local", SupportedApplicationCodes: []string{"settlement"}, SupportedEnvironments: []string{"dev"},
		Targets: []application.SubsystemProvisioningTarget{{
			ApplicationCode: "settlement", ApplicationName: "结算系统", Environment: "dev",
			UpstreamURL: "http://settlement-api:8080", PathPrefix: "/settlement", ClientType: "confidential",
			ManifestChecksum: "manifest-aaa"}}}}
}

func onboardingAdapterFor(t *testing.T, service *onboardingServiceStub, catalog accessCatalogStub, registry registryLookupStub) *SubsystemAccessOnboardingAdapter {
	t.Helper()
	adapter, err := NewSubsystemAccessOnboardingAdapter(service, catalog, registry)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func accessOperationFixture() application.SubsystemAccessOperation {
	return application.SubsystemAccessOperation{ID: "01SAOP000000000000000000001", TenantID: "tenant-1", OperatorID: "operator-1",
		ApplicationCode: "settlement", EnvironmentCode: "dev", Action: application.SubsystemAccessActionOnboard,
		PublicOrigin: "https://settlement.example.com", AuthProvider: "platform", InitialAdminID: "01USR000000000000000000001"}
}

// 条件检查：命中清单 → 调用既有预检并把清单摘要作为证据。
func TestSubsystemAccessOnboardingPreflightSucceeds(t *testing.T) {
	service := &onboardingServiceStub{}
	adapter := onboardingAdapterFor(t, service, accessCatalogWithTarget(), registryLookupStub{})
	outcome, err := adapter.PreflightSubsystemAccess(context.Background(), accessOperationFixture())
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded || outcome.EvidenceRef != "preflight:manifest-aaa" {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
	if service.seenOnboard.ApplicationName != "结算系统" || service.seenOnboard.UpstreamURL != "http://settlement-api:8080" ||
		service.seenOnboard.InitialAdminUserID != "01USR000000000000000000001" || service.seenOnboard.PublicBaseURL != "https://settlement.example.com" {
		t.Fatalf("onboarding input = %+v", service.seenOnboard)
	}
	if service.seenOnboard.IssuerAlias == nil || *service.seenOnboard.IssuerAlias != "platform" {
		t.Fatalf("issuer alias = %v", service.seenOnboard.IssuerAlias)
	}
}

// 清单未命中 → 范围外失败（不调用既有服务）；既有校验失败 → 预检拒绝；其它错误 → 可重试。
func TestSubsystemAccessOnboardingPreflightFailures(t *testing.T) {
	outOfScope := onboardingAdapterFor(t, &onboardingServiceStub{},
		accessCatalogStub{capabilities: application.SubsystemProvisioningCapabilities{Mode: "local"}}, registryLookupStub{})
	outcome, err := outOfScope.PreflightSubsystemAccess(context.Background(), accessOperationFixture())
	if err != nil || outcome.ErrorCode != "OUT_OF_SCOPE" || outcome.Status != application.SubsystemAccessStatusFailed {
		t.Fatalf("out of scope outcome = %+v err=%v", outcome, err)
	}

	rejected := onboardingAdapterFor(t, &onboardingServiceStub{preflightErr: application.ErrValidation}, accessCatalogWithTarget(), registryLookupStub{})
	outcome, err = rejected.PreflightSubsystemAccess(context.Background(), accessOperationFixture())
	if err != nil || outcome.ErrorCode != "PREFLIGHT_REJECTED" {
		t.Fatalf("rejected outcome = %+v err=%v", outcome, err)
	}

	unknown := onboardingAdapterFor(t, &onboardingServiceStub{preflightErr: errors.New("database unavailable")}, accessCatalogWithTarget(), registryLookupStub{})
	if _, err := unknown.PreflightSubsystemAccess(context.Background(), accessOperationFixture()); err == nil {
		t.Fatal("unknown preflight error must be retryable, not a silent failure")
	}
}

// 目录登记：已登记（或操作已带 ID）→ 复用既有登记，不重复写。
func TestSubsystemAccessOnboardingDirectoryReusesExisting(t *testing.T) {
	service := &onboardingServiceStub{}
	adapter := onboardingAdapterFor(t, service, accessCatalogWithTarget(),
		registryLookupStub{applicationID: "01APP000000000000000000009", environmentID: "01ENV000000000000000000009", found: true})
	outcome, err := adapter.RegisterSubsystemAccessDirectory(context.Background(), accessOperationFixture())
	if err != nil || outcome.EvidenceRef != "directory:existing:01APP000000000000000000009/01ENV000000000000000000009" {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
	if service.registerCalls != 0 {
		t.Fatalf("existing registration must not be rewritten (calls=%d)", service.registerCalls)
	}

	withIDs := accessOperationFixture()
	withIDs.ApplicationID, withIDs.EnvironmentID = "01APP000000000000000000001", "01ENV000000000000000000001"
	outcome, err = adapter.RegisterSubsystemAccessDirectory(context.Background(), withIDs)
	if err != nil || outcome.EvidenceRef != "directory:existing:01APP000000000000000000001/01ENV000000000000000000001" {
		t.Fatalf("operation-carried ids outcome = %+v err=%v", outcome, err)
	}
}

// 目录登记：未登记 → 按清单参数登记，并把应用/环境 ID 作为证据。
func TestSubsystemAccessOnboardingDirectoryRegisters(t *testing.T) {
	service := &onboardingServiceStub{}
	adapter := onboardingAdapterFor(t, service, accessCatalogWithTarget(), registryLookupStub{found: false})
	outcome, err := adapter.RegisterSubsystemAccessDirectory(context.Background(), accessOperationFixture())
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded ||
		outcome.EvidenceRef != "directory:created:01APP000000000000000000001/01ENV000000000000000000001" {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
	if service.registerCalls != 1 || service.seenDirectory.PathPrefix != "/settlement" ||
		service.seenDirectory.PublicBaseURL != "https://settlement.example.com" {
		t.Fatalf("directory input = %+v calls=%d", service.seenDirectory, service.registerCalls)
	}

	rejected := onboardingAdapterFor(t, &onboardingServiceStub{registerErr: application.ErrValidation}, accessCatalogWithTarget(), registryLookupStub{})
	outcome, err = rejected.RegisterSubsystemAccessDirectory(context.Background(), accessOperationFixture())
	if err != nil || outcome.ErrorCode != "DIRECTORY_REJECTED" {
		t.Fatalf("rejected outcome = %+v err=%v", outcome, err)
	}
}
