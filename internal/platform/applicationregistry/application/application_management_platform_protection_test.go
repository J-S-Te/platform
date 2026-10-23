package application

import (
	"context"
	"errors"
	"testing"
)

// 内置平台应用是登录与授权控制面的根节点。DELETE 一直拒绝退役它，但 PATCH 曾经
// 直接接受 SUSPENDED/RETIRED：任何拥有应用更新权限、能提供合法版本号的人都可以
// 让平台应用不再 ACTIVE，随后登录目标与机器客户端一起失效。
func TestUpdateApplicationRejectsNonActiveStatusForBuiltinPlatform(t *testing.T) {
	for _, status := range []string{"SUSPENDED", "RETIRED", "DRAFT"} {
		t.Run(status, func(t *testing.T) {
			repository := &applicationDeleteRepositoryStub{current: Application{
				ID: "01J00000000000000000000001", TenantID: "tenant-1", Code: builtInApplicationCode,
				Name: "基础能力平台", ApplicationType: "web", Status: "ACTIVE", Version: 3,
			}}
			service := newApplicationDeleteService(t, repository)

			_, err := service.UpdateApplication(context.Background(), ApplicationUpdateInput{
				TenantID: "tenant-1", OperatorID: "operator-1", ApplicationID: "01J00000000000000000000001",
				Name: "基础能力平台", ApplicationType: "web", Status: status, Version: 3,
			})
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("status=%s err = %v, want ErrConflict", status, err)
			}
			if repository.updateCalls != 0 {
				t.Fatalf("status=%s 生命周期变更仍写入了仓储（updateCalls=%d）", status, repository.updateCalls)
			}
		})
	}
}

func TestUpdateApplicationStillAllowsPlatformActivedetailUpdates(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: Application{
		ID: "01J00000000000000000000001", TenantID: "tenant-1", Code: builtInApplicationCode,
		Name: "基础能力平台", ApplicationType: "web", Status: "ACTIVE", Version: 3,
	}}
	service := newApplicationDeleteService(t, repository)

	updated, err := service.UpdateApplication(context.Background(), ApplicationUpdateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", ApplicationID: "01J00000000000000000000001",
		Name: "基础能力平台（新名称）", ApplicationType: "web", Status: "ACTIVE", Version: 3,
	})
	if err != nil {
		t.Fatalf("ACTIVE 状态下的普通更新被拒绝: %v", err)
	}
	if repository.updateCalls != 1 || updated.Name != "基础能力平台（新名称）" {
		t.Fatalf("updateCalls=%d updated=%+v", repository.updateCalls, updated)
	}
}

// 普通业务应用的退役仍然走 DELETE 的确认码流程，PATCH 的既有行为保持不变。
func TestUpdateApplicationStillAllowsBusinessApplicationRetirement(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: Application{
		ID: "01M00000000000000000000001", TenantID: "tenant-1", Code: "contract_management",
		Name: "合同管理系统", ApplicationType: "web", Status: "ACTIVE", Version: 7,
	}}
	service := newApplicationDeleteService(t, repository)

	updated, err := service.UpdateApplication(context.Background(), ApplicationUpdateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", ApplicationID: "01M00000000000000000000001",
		Name: "合同管理系统", ApplicationType: "web", Status: "SUSPENDED", Version: 7,
	})
	if err != nil {
		t.Fatalf("业务应用的暂停被拒绝: %v", err)
	}
	if updated.Status != "SUSPENDED" {
		t.Fatalf("status = %q, want SUSPENDED", updated.Status)
	}
}
