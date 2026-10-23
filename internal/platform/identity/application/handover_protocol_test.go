package application

import (
	"testing"
	"time"
)

func TestHandoverSystemDispositionClassifiesCapability(t *testing.T) {
	cases := map[string]struct{ blocking, notApplicable bool }{
		HandoverSystemApplicable:    {false, false},
		HandoverSystemNotInstalled:  {false, true},
		HandoverSystemUnreachable:   {true, false},
		HandoverSystemIncompatible:  {true, false},
		HandoverSystemLicenseDenied: {true, false},
		"UNKNOWN":                   {true, false},
	}
	for capability, want := range cases {
		blocking, notApplicable := HandoverSystemDisposition(capability)
		if blocking != want.blocking || notApplicable != want.notApplicable {
			t.Fatalf("capability %s = (%v,%v), want (%v,%v)", capability, blocking, notApplicable, want.blocking, want.notApplicable)
		}
	}
}

func TestHandoverIdempotencyKeyIsStable(t *testing.T) {
	first := HandoverIdempotencyKey("batch-1", "item-1")
	if first != HandoverIdempotencyKey(" batch-1 ", "item-1") {
		t.Fatal("idempotency key must ignore surrounding whitespace")
	}
	if first == HandoverIdempotencyKey("batch-1", "item-2") {
		t.Fatal("different items must not share a key")
	}
}

func TestHandoverRetryBackoffIsBounded(t *testing.T) {
	if got := HandoverRetryBackoff(1); got != 10*time.Second {
		t.Fatalf("attempt 1 = %s, want 10s", got)
	}
	if got := HandoverRetryBackoff(20); got != 10*time.Minute {
		t.Fatalf("attempt 20 = %s, want the 10m cap", got)
	}
	if got := HandoverRetryBackoff(0); got != 5*time.Second {
		t.Fatalf("attempt 0 = %s, want the 5s base", got)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if got := NextHandoverRetryAt(2, now); !got.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("next retry = %s, want now+20s", got)
	}
}

func TestHandoverLeaseExpiryAndReceiptTerminality(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if HandoverLeaseExpired(now.Add(time.Minute), now) {
		t.Fatal("lease must still be held before its deadline")
	}
	if !HandoverLeaseExpired(now, now) {
		t.Fatal("lease must be expired at its deadline")
	}
	for status, want := range map[string][2]bool{
		HandoverReceiptApplied:        {true, true},
		HandoverReceiptAlreadyApplied: {true, true},
		HandoverReceiptRejected:       {true, false},
		HandoverReceiptRetryable:      {false, false},
	} {
		terminal, success := HandoverReceiptTerminal(status)
		if terminal != want[0] || success != want[1] {
			t.Fatalf("receipt %s = (%v,%v), want (%v,%v)", status, terminal, success, want[0], want[1])
		}
	}
}

func TestHandoverBatchCompletable(t *testing.T) {
	transferred := []HandoverItem{{Status: HandoverItemTransferred}, {Status: HandoverItemSkipped}}
	applicable := []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}

	if !HandoverBatchCompletable(transferred, applicable) {
		t.Fatal("all-resolved items with applicable systems must be completable")
	}
	if HandoverBatchCompletable([]HandoverItem{{Status: HandoverItemPending}}, applicable) {
		t.Fatal("pending items must block completion")
	}
	if HandoverBatchCompletable([]HandoverItem{{Status: HandoverItemFailed}}, applicable) {
		t.Fatal("failed items must block completion")
	}
	if !HandoverBatchCompletable(transferred, []HandoverSystemState{{System: "portal", Capability: HandoverSystemNotInstalled}}) {
		t.Fatal("not-installed systems must not block completion")
	}
	for _, capability := range []string{HandoverSystemUnreachable, HandoverSystemIncompatible, HandoverSystemLicenseDenied} {
		if HandoverBatchCompletable(transferred, []HandoverSystemState{{System: "contract", Capability: capability}}) {
			t.Fatalf("capability %s must block completion even when items look resolved", capability)
		}
	}
}
