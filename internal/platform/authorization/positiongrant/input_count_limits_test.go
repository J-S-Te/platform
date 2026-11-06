package positiongrant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func templateRoleInputs(count int) []TemplateRoleInput {
	items := make([]TemplateRoleInput, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, TemplateRoleInput{
			ApplicationID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", index%100),
			RoleID:        fmt.Sprintf("role-%03d", index),
			ScopeType:     "TENANT",
			Status:        activeStatus,
		})
	}
	return items
}

func assignmentInputs(count int) []AssignmentInput {
	items := make([]AssignmentInput, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, AssignmentInput{TemplateID: fmt.Sprintf("template-%03d", index), Status: activeStatus})
	}
	return items
}

func TestTemplateRolesAreLimitedPerTemplate(t *testing.T) {
	allowed := TemplateInput{Name: "模板", Roles: templateRoleInputs(maxTemplateRoles)}
	if err := normalizeTemplateInput(&allowed); err != nil {
		t.Fatalf("template with %d roles rejected: %v", maxTemplateRoles, err)
	}

	rejected := TemplateInput{Name: "模板", Roles: templateRoleInputs(maxTemplateRoles + 1)}
	err := normalizeTemplateInput(&rejected)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("template with %d roles error = %v, want ErrValidation", maxTemplateRoles+1, err)
	}
}

func TestPositionAssignmentsAreLimitedPerPosition(t *testing.T) {
	allowed, err := normalizeAssignments(assignmentInputs(maxPositionTemplateAssignments))
	if err != nil {
		t.Fatalf("assignments %d rejected: %v", maxPositionTemplateAssignments, err)
	}
	if len(allowed) != maxPositionTemplateAssignments {
		t.Fatalf("normalized assignments = %d, want %d", len(allowed), maxPositionTemplateAssignments)
	}

	if _, err := normalizeAssignments(assignmentInputs(maxPositionTemplateAssignments + 1)); !errors.Is(err, ErrValidation) {
		t.Fatalf("assignments %d error = %v, want ErrValidation", maxPositionTemplateAssignments+1, err)
	}
}

// 超限属于纯输入契约：服务必须在解析快照、加锁与任何写入之前拒绝，因此可以用
// 无数据库依赖的 Service 直接覆盖。
func TestReplacePositionAssignmentsRejectsOversizedSetBeforeWrites(t *testing.T) {
	service := &Service{}
	_, err := service.ReplacePositionAssignments(context.Background(), "tenant-1", "operator-1", "position-1", ReplaceAssignmentsInput{
		Assignments:      assignmentInputs(maxPositionTemplateAssignments + 1),
		ExpectedRevision: "v1:" + fmt.Sprintf("%064d", 0),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("oversized assignment set error = %v, want ErrValidation", err)
	}
}

func TestTemplateHandlerRejectsTooManyRolesWithValidationStatus(t *testing.T) {
	service := &Service{}
	handler := &Handler{service: service}
	payload := map[string]any{
		"name":   "模板",
		"status": activeStatus,
		"roles":  templateRoleInputs(maxTemplateRoles + 1),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/position-authorization-templates", bytes.NewReader(raw))
	request = request.WithContext(authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.Create(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
}

func TestAssignmentHandlerRejectsTooManyAssignmentsWithValidationStatus(t *testing.T) {
	service := &Service{}
	handler := &Handler{service: service}
	payload := map[string]any{
		"assignments":       assignmentInputs(maxPositionTemplateAssignments + 1),
		"expected_revision": "v1:" + fmt.Sprintf("%064d", 0),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/positions/position-1/authorization-templates", bytes.NewReader(raw))
	request.SetPathValue("position_id", "position-1")
	request = request.WithContext(authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.ReplacePositionAssignments(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
}
