package identityhttp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// auditResourceServiceStub 嵌入接口占位：本用例只触达被测创建方法，其余方法一旦被
// 调用即触发 nil 指针 panic，防止用例在未察觉的情况下走过未桩接的路径。
type auditResourceServiceStub struct {
	managementApplicationService
}

func (stub *auditResourceServiceStub) CreateUser(context.Context, application.UserCreateInput) (application.UserView, error) {
	return application.UserView{ID: "usr_created_1", DisplayName: "审计目标用户"}, nil
}

func (stub *auditResourceServiceStub) CreateMembership(context.Context, application.MembershipCreateInput) (domain.Membership, error) {
	return domain.Membership{ID: "mbr_created_1"}, nil
}

type allowAllScopeAuthorizer struct{}

func (allowAllScopeAuthorizer) Resolve(context.Context, managementscope.Subject, string) (managementscope.Scope, error) {
	return managementscope.Scope{Unrestricted: true}, nil
}

func (allowAllScopeAuthorizer) ResourceContext(context.Context, string, string, string) (managementscope.ResourceContext, error) {
	return managementscope.ResourceContext{}, nil
}

func auditResourceTestHandler(t *testing.T) *ManagementHandler {
	t.Helper()
	handler, err := NewManagementHandler(
		&auditResourceServiceStub{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowAllScopeAuthorizer{},
	)
	if err != nil {
		t.Fatalf("NewManagementHandler() error = %v", err)
	}
	return handler
}

// 安全审查 SEC-D5：users 创建接口必须把被创建对象 id 登记给审计中间件槽位。
func TestCreateUserRegistersAuditResourceID(t *testing.T) {
	handler := auditResourceTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString("{\"display_name\":\"审计目标用户\"}"))
	request = request.WithContext(middleware.AttachAuditResourceTarget(request.Context()))
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	recorder := httptest.NewRecorder()

	handler.CreateUser(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "usr_created_1" {
		t.Fatalf("audit resource id = %q, want usr_created_1", got)
	}
}

// 安全审查 SEC-D5：memberships 创建接口同样接线（原验收四类之一，identity 侧归属）。
func TestCreateMembershipRegistersAuditResourceID(t *testing.T) {
	handler := auditResourceTestHandler(t)
	payload := "{\"user_id\":\"usr-1\",\"org_unit_id\":\"org-1\",\"position_id\":\"pos-1\",\"membership_type\":\"FULL_TIME\",\"effective_from\":\"2026-01-01\"}"
	request := httptest.NewRequest(http.MethodPost, "/api/v1/memberships", bytes.NewBufferString(payload))
	request = request.WithContext(middleware.AttachAuditResourceTarget(request.Context()))
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	recorder := httptest.NewRecorder()

	handler.CreateMembership(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "mbr_created_1" {
		t.Fatalf("audit resource id = %q, want mbr_created_1", got)
	}
}
