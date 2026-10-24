package positiongrant

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSyncAssignmentMustRepairBindingRoleDrift(t *testing.T) {
	scenario := &assignmentRegressionScenario{}
	scenario.query = func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "authz_position_grant_template_assignment"):
			return regressionRows("id,tenant_id,position_id,template_id,status", []driver.Value{"assignment-1", "tenant-1", "position-1", "template-1", "ACTIVE"}), nil
		case strings.Contains(q, "authz_position_grant_template_role"):
			return regressionRows("id,tenant_id,template_id,application_id,role_id,scope_type,scope_id,status", []driver.Value{"item-1", "tenant-1", "template-1", "platform-app", "role-template", "TENANT", "", "ACTIVE"}), nil
		case strings.Contains(q, "authz_position_grant_template"):
			return regressionRows("id,tenant_id,status", []driver.Value{"template-1", "tenant-1", "ACTIVE"}), nil
		case strings.Contains(q, "authz_role_binding"):
			return regressionRows("id,application_id,role_id,scope_type,scope_id,status,origin_item_id,version", []driver.Value{"binding-1", "platform-app", "role-other", "TENANT", "", "ACTIVE", "item-1", int64(2)}), nil
		default:
			return nil, fmt.Errorf("unexpected SQL: %s", q)
		}
	}
	svc := &Service{db: regressionDB(t, scenario), ids: assignmentRegressionIDs{}}
	apps := map[string]struct{}{}
	positions := map[string]map[string]struct{}{}
	err := svc.syncAssignment(context.Background(), svc.db, "tenant-1", "assignment-1", "operator-1", time.Now().UTC(), apps, positions)
	if !errors.Is(err, errAssignmentFirstWrite) || scenario.writes != 1 {
		t.Fatalf("unexpected: err=%v writes=%d changed=%v", err, scenario.writes, apps)
	}
	t.Log("Repair must start by mutating the displaced binding.")
}

func TestReplaceAssignmentsRejectsMissingRevision(t *testing.T) {
	scenario := &assignmentRegressionScenario{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "iam_position"):
			if strings.Contains(q, "for update") {
				return regressionRows("id", []driver.Value{"position-1"}), nil
			}
			return regressionRows("count", []driver.Value{int64(1)}), nil
		case strings.Contains(q, "authz_position_grant_template_assignment"):
			return regressionRows("id"), nil
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}}
	service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{time.Now().UTC()}, ids: assignmentRegressionIDs{}}
	_, err := service.ReplacePositionAssignments(context.Background(), "tenant-1", "operator-1", "position-1", ReplaceAssignmentsInput{})
	if !errors.Is(err, ErrValidation) || scenario.writes != 0 {
		t.Fatalf("missing revision must fail before writes: err=%v writes=%d", err, scenario.writes)
	}
}

func TestAssignmentSetRevisionDetectsHiddenRowsVersionsAndPosition(t *testing.T) {
	row := assignmentModel{ID: "a-1", TemplateID: "template-1", Status: activeStatus, Version: 1}
	original := assignmentSetRevision("t", "p", []assignmentModel{row})
	for _, tc := range []struct {
		name, tenant, position string
		rows                   []assignmentModel
	}{
		{"empty set", "t", "p", nil},
		{"other tenant", "other", "p", []assignmentModel{row}},
		{"other position", "t", "other", []assignmentModel{row}},
		{"hidden disabled row", "t", "p", []assignmentModel{row, {ID: "hidden", TemplateID: "disabled-template", Status: disabledStatus, Version: 1}}},
		{"version", "t", "p", []assignmentModel{{ID: row.ID, TemplateID: row.TemplateID, Status: row.Status, Version: 2}}},
	} {
		if got := assignmentSetRevision(tc.tenant, tc.position, tc.rows); got == original {
			t.Errorf("%s did not change revision", tc.name)
		}
	}
	rows := []assignmentModel{row, {ID: "b", TemplateID: "two", Status: activeStatus, Version: 1}}
	if assignmentSetRevision("t", "p", rows) != assignmentSetRevision("t", "p", []assignmentModel{rows[1], rows[0]}) {
		t.Fatal("row ordering changes revision")
	}
}

func TestReplaceAssignmentsRejectsStaleSetBeforeDelegationAndWrites(t *testing.T) {
	for _, name := range []string{"concurrent addition", "changed version", "hidden disabled row"} {
		t.Run(name, func(t *testing.T) {
			read := []assignmentModel{{ID: "a", TemplateID: "template-1", Status: activeStatus, Version: 1}}
			current := append([]assignmentModel(nil), read...)
			switch name {
			case "concurrent addition":
				current = append(current, assignmentModel{ID: "new", TemplateID: "template-2", Status: activeStatus, Version: 1})
			case "changed version":
				current[0].Version++
			case "hidden disabled row":
				current = append(current, assignmentModel{ID: "hidden", TemplateID: "gone", Status: disabledStatus, Version: 1})
			}
			locked := false
			scenario := &assignmentRegressionScenario{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "iam_position"):
					if strings.Contains(q, "for update") {
						locked = true
					}
					if strings.Contains(q, "for update") {
						return regressionRows("id", []driver.Value{"position-1"}), nil
					}
					return regressionRows("count", []driver.Value{int64(1)}), nil
				case strings.Contains(q, "authz_position_grant_template_assignment"):
					var values [][]driver.Value
					for _, r := range current {
						values = append(values, []driver.Value{r.ID, r.TemplateID, r.Status, int64(r.Version)})
					}
					return regressionRows("id,template_id,status,version", values...), nil
				default:
					return nil, fmt.Errorf("stale save must stop before delegation: %s", q)
				}
			}}
			service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{time.Now().UTC()}, ids: assignmentRegressionIDs{}}
			_, err := service.ReplacePositionAssignments(context.Background(), "tenant-1", "operator-1", "position-1", ReplaceAssignmentsInput{ExpectedRevision: assignmentSetRevision("tenant-1", "position-1", read)})
			if !errors.Is(err, ErrConflict) || scenario.writes != 0 || !locked {
				t.Fatalf("stale save err=%v writes=%d parentLocked=%v", err, scenario.writes, locked)
			}
		})
	}
}
func TestEmptyAssignmentReadAndSaveReturnAtomicRevision(t *testing.T) {
	scenario := &assignmentRegressionScenario{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "iam_position"):
			if strings.Contains(q, "for update") {
				return regressionRows("id", []driver.Value{"position-1"}), nil
			}
			return regressionRows("count", []driver.Value{int64(1)}), nil
		case strings.Contains(q, "authz_position_grant_template_assignment"):
			return regressionRows("id"), nil
		default:
			return nil, fmt.Errorf("unexpected SQL %s", q)
		}
	}}
	s := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{time.Now().UTC()}}
	first, err := s.ListPositionAssignments(context.Background(), "tenant-1", "position-1")
	if err != nil || len(first.Revision) != 67 || first.Items == nil {
		t.Fatalf("read=%+v err=%v", first, err)
	}
	result, err := s.ReplacePositionAssignments(context.Background(), "tenant-1", "operator-1", "position-1", ReplaceAssignmentsInput{ExpectedRevision: first.Revision})
	if err != nil || result.Revision != first.Revision || result.Items == nil || result.Total != 0 {
		t.Fatalf("save=%+v err=%v", result, err)
	}
}
