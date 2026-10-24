package application

import (
	"strings"
	"testing"
)

func TestJobInputRejectsOversizedIdentifiers(t *testing.T) {
	base := JobCreateInput{TenantID: "tenant", JobType: "EXPORT_REPORT", Payload: []byte("{}")}
	cases := []JobCreateInput{
		func() JobCreateInput { item := base; item.AggregateType = strings.Repeat("A", 65); return item }(),
		func() JobCreateInput { item := base; item.AggregateID = strings.Repeat("A", 129); return item }(),
		func() JobCreateInput { item := base; item.IdempotencyKey = strings.Repeat("a", 129); return item }(),
	}
	for index, input := range cases {
		if err := validateJobInput(input); err == nil {
			t.Fatalf("case %d unexpectedly accepted", index)
		}
	}
}
