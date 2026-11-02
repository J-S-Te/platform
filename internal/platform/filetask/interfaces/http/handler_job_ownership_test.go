package filetaskhttp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// A3 回归：应用机器主体只能为自己的应用建任务；请求体 application_id 不能用来
// 冒充其他应用归属，被拒请求不得触达任务服务（不落库）。
// 浏览器主体（带平台用户身份）沿用既有行为，仍可按权限为指定应用建任务。

type recordingJobService struct {
	stubV1JobService
	inputs []application.JobCreateInput
}

func (service *recordingJobService) Enqueue(_ context.Context, input application.JobCreateInput) (domain.Job, error) {
	service.inputs = append(service.inputs, input)
	return domain.Job{
		PublicID: "01K10C00000000000000000JB1", TenantID: input.TenantID, ApplicationID: input.ApplicationID,
		JobType: input.JobType, Status: domain.JobStatusPending, AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}, nil
}

func createJobRequest(body string, principal authctx.Principal) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/async-jobs", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(authctx.WithPrincipal(request.Context(), principal))
}

func browserPrincipal() authctx.Principal {
	return authctx.Principal{
		SessionID: "session-1",
		Tenant:    authctx.ReferenceName{ID: "tenant-1"},
		User:      authctx.ReferenceName{ID: "user-1"},
		Account:   authctx.ReferenceName{ID: "account-1"},
	}
}

func TestCreateJobRejectsForeignApplicationForMachineClient(t *testing.T) {
	jobs := &recordingJobService{}
	handler, err := NewJobHandler(jobs, slog.New(&captureLogger{}))
	if err != nil {
		t.Fatalf("NewJobHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.CreateJob(response, createJobRequest(`{"application_id":"app-other","job_type":"REPORT_EXPORT","payload":{"report_id":"r-1"}}`, machinePrincipal()))

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	// 稳定错误码沿用既有 forbidden 映射（AUTH_FORBIDDEN）。
	if !strings.Contains(response.Body.String(), "AUTH_FORBIDDEN") {
		t.Fatalf("body = %s", response.Body.String())
	}
	if len(jobs.inputs) != 0 {
		t.Fatalf("被拒的跨应用任务不得落库: %+v", jobs.inputs)
	}
}

func TestCreateJobAllowsOwnApplicationForMachineClient(t *testing.T) {
	jobs := &recordingJobService{}
	handler, err := NewJobHandler(jobs, slog.New(&captureLogger{}))
	if err != nil {
		t.Fatalf("NewJobHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.CreateJob(response, createJobRequest(`{"application_id":"app-1","job_type":"REPORT_EXPORT","payload":{"report_id":"r-1"}}`, machinePrincipal()))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(jobs.inputs) != 1 || jobs.inputs[0].ApplicationID != "app-1" {
		t.Fatalf("自有应用任务应落库: %+v", jobs.inputs)
	}
}

// 浏览器主体行为保持现状：带平台用户身份的运营请求不因 application_id 与应用身份
// 不同而被新规则拒绝。
func TestCreateJobKeepsBrowserApplicationSelection(t *testing.T) {
	jobs := &recordingJobService{}
	handler, err := NewJobHandler(jobs, slog.New(&captureLogger{}))
	if err != nil {
		t.Fatalf("NewJobHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.CreateJob(response, createJobRequest(`{"application_id":"app-other","job_type":"REPORT_EXPORT","payload":{"report_id":"r-1"}}`, browserPrincipal()))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(jobs.inputs) != 1 || jobs.inputs[0].ApplicationID != "app-other" {
		t.Fatalf("浏览器按权限选择的应用应保持原样: %+v", jobs.inputs)
	}
}
