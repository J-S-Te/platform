package positiongrant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestListAuthorizationPositionsReturnsActiveOrganizationContext(t *testing.T) {
	t.Parallel()

	scenario := &authorizationPositionScenario{
		rows: [][]driver.Value{{
			"position-sales", "sales", "销售人员",
			"org-sales", "sales-department", "销售部", "/org-root/org-sales/", uint64(2), int64(20),
		}},
	}
	service := newAuthorizationPositionTestService(t, scenario)

	got, err := service.ListAuthorizationPositions(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("list authorization positions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("position count = %d, want 1", len(got))
	}
	want := AuthorizationPositionView{
		PositionID: "position-sales", PositionCode: "sales", PositionName: "销售人员",
		OrgUnitID: "org-sales", OrgUnitCode: "sales-department", OrgUnitName: "销售部",
		OrgUnitPath: "/org-root/org-sales/", OrgUnitDepth: 2, OrgUnitSortOrder: 20,
	}
	if got[0] != want {
		t.Fatalf("position = %#v, want %#v", got[0], want)
	}

	query := strings.ToLower(scenario.query)
	for _, fragment := range []string{
		"from iam_position as position",
		"join iam_org_unit as organization",
		"organization.tenant_id=position.tenant_id",
		"organization.id=position.org_unit_id",
		"organization.status=?",
		"position.tenant_id=? and position.status=?",
		"order by organization.path asc, organization.sort_order asc, organization.code asc, position.name asc, position.code asc, position.id asc",
	} {
		if !strings.Contains(query, fragment) {
			t.Errorf("authorization position query is missing %q: %s", fragment, scenario.query)
		}
	}
	wantArgs := []any{activeStatus, "tenant-1", activeStatus}
	if len(scenario.args) != len(wantArgs) {
		t.Fatalf("query argument count = %d, want %d: %#v", len(scenario.args), len(wantArgs), scenario.args)
	}
	for index, wantArg := range wantArgs {
		if scenario.args[index].Value != wantArg {
			t.Errorf("query argument %d = %#v, want %#v", index, scenario.args[index].Value, wantArg)
		}
	}
}

type authorizationPositionScenario struct {
	query string
	args  []driver.NamedValue
	rows  [][]driver.Value
}

type authorizationPositionDriver struct {
	scenario *authorizationPositionScenario
}
type authorizationPositionConn struct {
	scenario *authorizationPositionScenario
}
type authorizationPositionRows struct {
	values [][]driver.Value
	index  int
}

var authorizationPositionDriverCounter uint64

func newAuthorizationPositionTestService(t *testing.T, scenario *authorizationPositionScenario) *Service {
	t.Helper()
	driverName := fmt.Sprintf("positiongrant-authorization-position-%d", atomic.AddUint64(&authorizationPositionDriverCounter, 1))
	sql.Register(driverName, &authorizationPositionDriver{scenario: scenario})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open authorization position test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open authorization position gorm database: %v", err)
	}
	return &Service{db: database}
}

func (d *authorizationPositionDriver) Open(string) (driver.Conn, error) {
	return &authorizationPositionConn{scenario: d.scenario}, nil
}

func (c *authorizationPositionConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepared statements are not supported")
}
func (c *authorizationPositionConn) Close() error { return nil }
func (c *authorizationPositionConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("transactions are not supported")
}
func (c *authorizationPositionConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.scenario.query = query
	c.scenario.args = append([]driver.NamedValue(nil), args...)
	return &authorizationPositionRows{values: c.scenario.rows}, nil
}

func (*authorizationPositionRows) Columns() []string {
	return []string{
		"position_id", "position_code", "position_name",
		"org_unit_id", "org_unit_code", "org_unit_name", "org_unit_path", "org_unit_depth", "org_unit_sort_order",
	}
}
func (*authorizationPositionRows) Close() error { return nil }
func (r *authorizationPositionRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}
