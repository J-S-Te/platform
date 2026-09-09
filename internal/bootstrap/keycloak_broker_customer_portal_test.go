package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// brokerRepositoryStub 只实现 Broker 补齐路径需要的只读查询；其余方法不属于该路径。
type brokerRepositoryStub struct {
	applications []application.Application
	environments map[string][]application.Environment
}

func (stub brokerRepositoryStub) ListApplications(context.Context, string, application.PageRequest) (application.PageResult[application.Application], error) {
	return application.PageResult[application.Application]{Items: stub.applications}, nil
}

func (stub brokerRepositoryStub) ListEnvironments(_ context.Context, _ string, applicationID string, _ application.PageRequest) (application.PageResult[application.Environment], error) {
	return application.PageResult[application.Environment]{Items: stub.environments[applicationID]}, nil
}

func (brokerRepositoryStub) CreateApplication(context.Context, application.ApplicationCreateInput, string, time.Time) (application.Application, error) {
	return application.Application{}, errors.New("unexpected CreateApplication call")
}

func (stub brokerRepositoryStub) GetApplication(_ context.Context, _ string, id string) (application.Application, error) {
	for _, item := range stub.applications {
		if item.ID == id {
			return item, nil
		}
	}
	return application.Application{}, errors.New("unexpected GetApplication call")
}

func (brokerRepositoryStub) UpdateApplication(context.Context, application.ApplicationUpdateInput, time.Time) (application.Application, error) {
	return application.Application{}, errors.New("unexpected UpdateApplication call")
}

func (brokerRepositoryStub) CreateEnvironment(context.Context, application.EnvironmentCreateInput, string, time.Time) (application.Environment, error) {
	return application.Environment{}, errors.New("unexpected CreateEnvironment call")
}

func (brokerRepositoryStub) GetEnvironment(context.Context, string, string, string) (application.Environment, error) {
	return application.Environment{}, errors.New("unexpected GetEnvironment call")
}

func (brokerRepositoryStub) UpdateEnvironment(context.Context, application.EnvironmentUpdateInput, time.Time) (application.Environment, error) {
	return application.Environment{}, errors.New("unexpected UpdateEnvironment call")
}

func (brokerRepositoryStub) DeleteEnvironment(context.Context, application.EnvironmentDeleteInput) (application.Environment, error) {
	return application.Environment{}, errors.New("unexpected DeleteEnvironment call")
}

func newBrokerRegistrarForTest(t *testing.T, stub brokerRepositoryStub) keycloakBrokerRegistrar {
	t.Helper()
	service, err := application.NewManagementService(stub, ulid.Generator{}, application.SystemClock{})
	if err != nil {
		t.Fatalf("NewManagementService() error = %v", err)
	}
	return keycloakBrokerRegistrar{applications: service, environment: "dev"}
}

// TestEnsureCustomerPortalBrokerTreatsUnregisteredTargetAsSkippable 固定一个回归：
// 客户门户尚未接入（应用或 dev 环境未注册）时，Broker 补齐必须返回可识别的哨兵错误，
// 让 Worker 选择跳过而不是让进程退出——否则同一容器内的 API 会被一并终止并反复重启。
func TestEnsureCustomerPortalBrokerTreatsUnregisteredTargetAsSkippable(t *testing.T) {
	tests := []struct {
		name string
		stub brokerRepositoryStub
	}{
		{
			name: "customer_portal application missing",
			stub: brokerRepositoryStub{environments: map[string][]application.Environment{}},
		},
		{
			name: "dev environment missing",
			stub: brokerRepositoryStub{
				applications: []application.Application{{ID: "app-customer-portal", Code: "customer_portal"}},
				environments: map[string][]application.Environment{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registrar := newBrokerRegistrarForTest(t, tt.stub)
			if _, _, err := registrar.EnsureCustomerPortalBroker(context.Background(), "tenant"); !errors.Is(err, errBrokerTargetNotRegistered) {
				t.Fatalf("EnsureCustomerPortalBroker() error = %v, want wrapping %v", err, errBrokerTargetNotRegistered)
			}
		})
	}
}
