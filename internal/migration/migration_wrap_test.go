package migration

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestWrapStatementErrorExplainsMySQL1419WithoutHidingCause(t *testing.T) {
	cause := &mysql.MySQLError{Number: 1419, Message: "You do not have the SUPER privilege"}
	err := wrapStatementError(130, "audit_event_append_only", 1, cause)
	message := err.Error()
	for _, want := range []string{"000130_audit_event_append_only", "statement 1", "MySQL 1419", "log_bin_trust_function_creators=1", "不要给业务迁移账号授予 SUPER"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error message missing %q: %s", want, message)
		}
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped migration error must preserve the original MySQL error")
	}
}

func TestWrapStatementErrorKeepsOtherErrorsUnchanged(t *testing.T) {
	cause := errors.New("network closed")
	err := wrapStatementError(7, "example", 2, cause)
	if strings.Contains(err.Error(), "1419") {
		t.Fatalf("non-1419 error should not mention trigger trust: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped migration error must preserve the cause")
	}
}
