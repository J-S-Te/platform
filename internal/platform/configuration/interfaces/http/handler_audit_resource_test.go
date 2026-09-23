package http

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// 安全审查 SEC-D5：配置创建接口（config/namespaces、config/items、config/releases）
// 必须把被创建对象 id 登记给审计中间件槽位（task-38 拆分出的 configuration 侧接线，task-55 归属）。

// configAuditRepositoryStub 嵌入接口占位：未覆写的方法一旦被调用即 nil panic，
// 防止用例在未察觉的情况下走过未桩接的路径。
type configAuditRepositoryStub struct {
	application.Repository
}

func (stub *configAuditRepositoryStub) CreateNamespace(_ context.Context, input application.NamespaceCreateInput, _ string, _ time.Time) (domain.Namespace, error) {
	return domain.Namespace{ID: "cfgns_created_1", Code: input.Code, Name: input.Name}, nil
}

func (stub *configAuditRepositoryStub) CreateItem(_ context.Context, input application.ItemCreateInput, _ string, _ time.Time) (domain.Item, error) {
	return domain.Item{ID: "cfgitm_created_1", Key: input.Key, ValueType: input.ValueType}, nil
}

func (stub *configAuditRepositoryStub) CreateRelease(_ context.Context, input application.ReleaseCreateInput, _ string, _ time.Time) (domain.Release, error) {
	return domain.Release{ID: "cfgrel_created_1", VersionNo: 1, Status: "PUBLISHED"}, nil
}

type configAuditIDGenerator struct{}

func (configAuditIDGenerator) New(time.Time) (string, error) { return "generated-1", nil }

func newConfigAuditHandler(t *testing.T) *Handler {
	t.Helper()
	service, err := application.NewService(&configAuditRepositoryStub{}, configAuditIDGenerator{}, application.SystemClock{})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	handler, err := NewHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func configAuditRequest(path, payload string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(payload))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(middleware.AttachAuditResourceTarget(request.Context()))
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
}

func TestCreateNamespaceRegistersAuditResourceID(t *testing.T) {
	handler := newConfigAuditHandler(t)
	request := configAuditRequest("/api/v1/config/namespaces", `{"application_code":"contract_management","code":"cfg_ns","name":"配置命名空间"}`)
	recorder := httptest.NewRecorder()

	handler.CreateNamespace(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "cfgns_created_1" {
		t.Fatalf("audit resource id = %q, want cfgns_created_1", got)
	}
}

func TestCreateItemRegistersAuditResourceID(t *testing.T) {
	handler := newConfigAuditHandler(t)
	request := configAuditRequest("/api/v1/config/items", `{"namespace_id":"ns-1","key":"feature_flag","value_type":"STRING","value":"on"}`)
	recorder := httptest.NewRecorder()

	handler.CreateItem(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "cfgitm_created_1" {
		t.Fatalf("audit resource id = %q, want cfgitm_created_1", got)
	}
}

func TestCreateReleaseRegistersAuditResourceID(t *testing.T) {
	handler := newConfigAuditHandler(t)
	request := configAuditRequest("/api/v1/config/releases", `{"namespace_id":"ns-1","item_versions":[{"item_id":"itm-1","version":1}],"comment":"首个发布"}`)
	recorder := httptest.NewRecorder()

	handler.CreateRelease(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", recorder.Code, recorder.Body.String())
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "cfgrel_created_1" {
		t.Fatalf("audit resource id = %q, want cfgrel_created_1", got)
	}
}
