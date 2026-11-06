package application

import (
	"context"
	"errors"
	"testing"
)

// TestSettlementOptionalResolverNeverWeakensRegisteredIdentity 验证窄策略只放宽缺失来源。
// 参数：t为测试上下文，使用既有目录和客户端元数据替身。
// 返回值：无；读取故障、漂移、重复或半身份不能变成可选成功。
func TestSettlementOptionalResolverNeverWeakensRegisteredIdentity(t *testing.T) {
	policy := SubsystemRelatedServicePolicy{SettlementContractOptionalEnvironment: "dev"}
	for _, mode := range []string{"missing_app", "missing_client", "ready", "registry_failure", "blank_identity", "drifted", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			registry := &relatedRegistryStub{applicationID: "01APP000000000000000000002", environmentID: "01ENV000000000000000000001"}
			clients := []OAuthClientView{canonicalRelatedClient()}
			switch mode {
			case "missing_app":
				registry.err = ErrNotFound
			case "missing_client":
				clients = nil
			case "registry_failure":
				registry.err = errors.New("storage failed")
			case "blank_identity":
				registry.applicationID = ""
			case "drifted":
				clients[0].Scopes = []string{"*"}
			case "duplicate":
				clients = append(clients, clients[0])
			}
			resolver := relatedResolverFor(t, registry, relatedClientListerStub{clients: clients})
			found, err := resolver.ResolveWithPolicy(context.Background(), "tenant-1", "settlement", "dev", policy)
			if mode == "missing_app" || mode == "missing_client" {
				if err != nil || len(found) != 0 {
					t.Fatalf("missing optional source=%v err=%v", found, err)
				}
			} else if mode == "ready" {
				if err != nil || len(found) != 1 {
					t.Fatalf("valid registered source rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("registered or failed source was silently ignored")
			}
		})
	}
}

// TestSettlementOptionalResolverRejectsWrongPolicyTarget 拒绝跨应用、错环境和未批准环境策略。
// 参数：t为测试上下文，缺失来源使非法放宽会表现为错误成功。
// 返回值：无；默认Resolve始终保留缺依赖失败。
func TestSettlementOptionalResolverRejectsWrongPolicyTarget(t *testing.T) {
	resolver := relatedResolverFor(t, &relatedRegistryStub{err: ErrNotFound}, relatedClientListerStub{})
	for _, value := range []struct{ app, environment, selected string }{
		{"project_management", "dev", "dev"}, {"settlement", "prod", "dev"}, {"settlement", "ANY", "ANY"},
	} {
		_, err := resolver.ResolveWithPolicy(context.Background(), "tenant-1", value.app, value.environment,
			SubsystemRelatedServicePolicy{SettlementContractOptionalEnvironment: value.selected})
		if err == nil {
			t.Fatalf("wrong policy accepted: %+v", value)
		}
	}
	if _, err := resolver.Resolve(context.Background(), "tenant-1", "settlement", "dev"); err == nil {
		t.Fatal("default missing contract accepted")
	}
}

// TestSettlementCapabilityProjectionIsBoundToApprovedEnvironment 拒绝重复或非结算目标的Agent投影。
// 参数：t为测试上下文，不接受浏览器运行参数或context注入。
// 返回值：无；只有可用目标的精确环境能形成可选策略。
func TestSettlementCapabilityProjectionIsBoundToApprovedEnvironment(t *testing.T) {
	valid := SubsystemProvisioningCapabilities{Enabled: true, Mode: "production",
		SupportedApplicationCodes: []string{"settlement"}, SupportedEnvironments: []string{"prod"},
		Targets:                                []SubsystemProvisioningTarget{{ApplicationCode: "settlement", Environment: "prod"}},
		SettlementContractOptionalEnvironments: []string{"prod"}}
	policy, err := RelatedServicePolicyFromCapabilities(valid, "settlement", "prod")
	if err != nil || policy.SettlementContractOptionalEnvironment != "prod" {
		t.Fatal("valid Agent policy rejected")
	}
	other, err := RelatedServicePolicyFromCapabilities(valid, "project_management", "prod")
	if err != nil || other.SettlementContractOptionalEnvironment != "" {
		t.Fatal("projection applied to another application")
	}
	for _, mutation := range []func(*SubsystemProvisioningCapabilities){
		func(c *SubsystemProvisioningCapabilities) {
			c.SettlementContractOptionalEnvironments = []string{"prod", "prod"}
		},
		func(c *SubsystemProvisioningCapabilities) { c.SettlementContractOptionalEnvironments = []string{"dev"} },
		func(c *SubsystemProvisioningCapabilities) {
			c.Targets = []SubsystemProvisioningTarget{{ApplicationCode: "project_management", Environment: "prod"}}
		},
		func(c *SubsystemProvisioningCapabilities) {
			c.Targets = append(append([]SubsystemProvisioningTarget{}, c.Targets...), c.Targets[0])
		},
		func(c *SubsystemProvisioningCapabilities) { c.Enabled = false },
	} {
		changed := valid
		mutation(&changed)
		if err := ValidateRelatedServiceCapabilityProjection(changed); err == nil {
			t.Fatal("malformed Agent policy accepted")
		}
	}
}
