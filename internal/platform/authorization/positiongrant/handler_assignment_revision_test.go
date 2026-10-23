package positiongrant

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func TestAssignmentHandlerReturnsRevisionAndRejectsMissingOrStaleSnapshot(t *testing.T) {
	for _, name := range []string{"get", "matching put", "missing revision", "stale revision"} {
		t.Run(name, func(t *testing.T) {
			scenario := &assignmentRegressionScenario{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "iam_position"):
					if strings.Contains(q, "for update") {
						return regressionRows("id", []driver.Value{"p"}), nil
					}
					return regressionRows("count", []driver.Value{int64(1)}), nil
				case strings.Contains(q, "authz_position_grant_template_assignment"):
					return regressionRows("id"), nil
				default:
					return nil, fmt.Errorf("unexpected SQL %s", q)
				}
			}}
			service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{time.Now().UTC()}}
			handler := &Handler{service: service}
			method := http.MethodPut
			revision := assignmentSetRevision("t", "p", nil)
			payload := map[string]any{"assignments": []any{}, "expected_revision": revision}
			want := 200
			switch name {
			case "get":
				method = http.MethodGet
			case "missing revision":
				delete(payload, "expected_revision")
				want = 422
			case "stale revision":
				payload["expected_revision"] = assignmentSetRevision("t", "other-position", nil)
				want = 409
			}
			raw, _ := json.Marshal(payload)
			request := httptest.NewRequest(method, "/api/v1/positions/p/authorization-templates", bytes.NewReader(raw))
			request.SetPathValue("position_id", "p")
			request = request.WithContext(authctx.WithPrincipal(context.Background(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "t"}, User: authctx.ReferenceName{ID: "u"}}))
			response := httptest.NewRecorder()
			if method == http.MethodGet {
				handler.ListPositionAssignments(response, request)
			} else {
				handler.ReplacePositionAssignments(response, request)
			}
			if response.Code != want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
			}
			if want == 200 {
				var body struct{ Data AssignmentSetView }
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Data.Revision != revision || body.Data.Items == nil || body.Data.Total != 0 {
					t.Fatalf("snapshot shape=%s", response.Body.String())
				}
			}
			if scenario.writes != 0 {
				t.Fatalf("empty or rejected request performed writes: %d", scenario.writes)
			}
		})
	}
}
