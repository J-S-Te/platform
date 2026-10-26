package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 只运行生产查询和映射，不连接业务数据库；拒绝缺少租户或成功状态条件的 SQL。
type accessConfigurationSQLDriver struct {
	t                *testing.T
	digest, manifest string
	missing, fail    bool
	queries          int
}
type accessConfigurationSQLConn struct{ store *accessConfigurationSQLDriver }
type accessConfigurationSQLRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

var accessConfigurationSQLID uint64

func (d *accessConfigurationSQLDriver) Open(string) (driver.Conn, error) {
	return &accessConfigurationSQLConn{d}, nil
}
func (*accessConfigurationSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare forbidden")
}
func (*accessConfigurationSQLConn) Close() error { return nil }
func (*accessConfigurationSQLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("writes forbidden")
}
func (r *accessConfigurationSQLRows) Columns() []string { return r.columns }
func (*accessConfigurationSQLRows) Close() error        { return nil }
func (r *accessConfigurationSQLRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}
func (c *accessConfigurationSQLConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	d := c.store
	d.queries++
	if d.fail {
		return nil, errors.New("database unavailable")
	}
	if !strings.Contains(q, "tenant_id = ?") {
		d.t.Fatalf("missing tenant scope: %s", q)
	}
	hasTenant := false
	hasSuccess := false
	for _, arg := range args {
		if arg.Value == "tenant-1" {
			hasTenant = true
		}
		if arg.Value == "SUCCEEDED" {
			hasSuccess = true
		}
	}
	if !hasTenant {
		d.t.Fatalf("missing tenant parameter: %v", args)
	}
	if strings.Contains(q, "subsystem_deployment_state") {
		return &accessConfigurationSQLRows{columns: []string{"applied_manifest_checksum"}, values: []driver.Value{d.manifest}}, nil
	}
	if !hasSuccess || !strings.Contains(q, "status = ?") {
		d.t.Fatalf("missing successful evidence filter: %s %v", q, args)
	}
	if strings.Contains(q, "JOIN") && (!strings.Contains(q, "stage.tenant_id = operation.tenant_id") || !strings.Contains(q, "operation.status = ?")) {
		d.t.Fatalf("invalid baseline scope: %s", q)
	}
	return &accessConfigurationSQLRows{columns: []string{"evidence_ref"}, values: []driver.Value{accessCandidatePrefix + d.digest + ";update:manifest-aaa"}, done: d.missing}, nil
}
func TestSubsystemAccessConfigurationSQLRecovery(t *testing.T) {
	ctx := context.Background()
	approvals := accessApprovalSourceStub{components: []application.RuntimeLicenseApproval{{ServiceID: "api", ImageDigest: "sha256:one"}}}
	for _, scenario := range []string{"same", "changed", "missing", "database failure"} {
		t.Run(scenario, func(t *testing.T) {
			store := &accessConfigurationSQLDriver{t: t, manifest: "manifest-aaa"}
			name := fmt.Sprintf("access-configuration-%d", atomic.AddUint64(&accessConfigurationSQLID, 1))
			sql.Register(name, store)
			connection, err := sql.Open(name, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { connection.Close() })
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}
			tracker, err := NewSubsystemAccessConfigurationTracker(db, accessCatalogWithTarget(), approvals)
			if err != nil {
				t.Fatal(err)
			}
			op := onboardingOperation()
			store.digest, err = tracker.Snapshot(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "changed":
				store.digest = strings.Repeat("a", 64)
			case "missing":
				store.missing = true
			case "database failure":
				store.fail = true
			}
			drift, _, _, err := tracker.ReadSubsystemAccessConfigurationDrift(ctx, op.TenantID, op.ApplicationCode, op.EnvironmentCode)
			if scenario == "database failure" {
				if err == nil {
					t.Fatal("database error suppressed")
				}
				return
			}
			if err != nil || drift != (scenario != "same") {
				t.Fatalf("drift=%v err=%v", drift, err)
			}
			// 新 tracker 没有任何内存部署产物，仍从既有阶段证据恢复 Verify。
			recovered, _ := NewSubsystemAccessConfigurationTracker(db, accessCatalogWithTarget(), approvals)
			verified, err := recovered.Verify(ctx, op)
			if err != nil || verified != (scenario == "same") {
				t.Fatalf("verified=%v err=%v", verified, err)
			}
			if store.queries != 3 {
				t.Fatalf("queries=%d", store.queries)
			}
		})
	}
}
