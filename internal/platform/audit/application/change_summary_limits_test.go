package application

import (
	"errors"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// change_summary 条数上限：超限必须整条拒绝（HTTP 422），不得静默截断。
func TestValidateInputRejectsChangeSummaryExceedingEntryLimit(t *testing.T) {
	input := spoolableInput()
	input.Changes = make([]domain.FieldChange, MaxChangeSummaryEntries+1)
	for index := range input.Changes {
		input.Changes[index] = domain.FieldChange{Field: "status", Before: "A", After: "B"}
	}
	if err := validateInput("tenant-1", input); !errors.Is(err, ErrValidation) {
		t.Fatalf("超条数 err = %v, want ErrValidation", err)
	}
	input.Changes = input.Changes[:MaxChangeSummaryEntries]
	if err := validateInput("tenant-1", input); err != nil {
		t.Fatalf("等于上限的条数应放行：%v", err)
	}
}

// change_summary 单值长度上限：字符串按 rune、嵌套值按序列化长度，两条都不能绕过。
func TestValidateInputRejectsChangeSummaryOversizedValue(t *testing.T) {
	oversized := strings.Repeat("x", MaxChangeSummaryValueRunes+1)
	for name, changes := range map[string][]domain.FieldChange{
		"before":       {{Field: "display_name", Before: oversized}},
		"after":        {{Field: "display_name", After: oversized}},
		"nested value": {{Field: "payload", Before: map[string]any{"note": oversized}}},
	} {
		input := spoolableInput()
		input.Changes = changes
		if err := validateInput("tenant-1", input); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s 超长 err = %v, want ErrValidation", name, err)
		}
	}
	input := spoolableInput()
	input.Changes = []domain.FieldChange{{Field: "display_name", Before: strings.Repeat("x", MaxChangeSummaryValueRunes)}}
	if err := validateInput("tenant-1", input); err != nil {
		t.Fatalf("等于上限的长度应放行：%v", err)
	}
}

// 不可序列化的变更值会破坏审计载荷摘要，必须在写入前拒绝。
func TestValidateChangeSummaryRejectsNonSerializableValue(t *testing.T) {
	err := ValidateChangeSummary([]domain.FieldChange{{Field: "payload", Before: make(chan int)}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("不可序列化值 err = %v, want ErrValidation", err)
	}
}

// 自由文本快照字段（actor_name/resource_name/reason_code/business_id）限长与
// audit_event 列宽对齐：超限 422，而不是留给 DB 报错。
func TestValidateInputRejectsOversizedSnapshotFields(t *testing.T) {
	cases := map[string]func(*EventInput){
		"actor_name":    func(input *EventInput) { input.ActorName = strings.Repeat("名", MaxActorNameRunes+1) },
		"resource_name": func(input *EventInput) { input.ResourceName = strings.Repeat("名", MaxResourceNameRunes+1) },
		"reason_code":   func(input *EventInput) { input.ReasonCode = strings.Repeat("R", MaxReasonCodeRunes+1) },
		"business_id":   func(input *EventInput) { input.BusinessID = strings.Repeat("B", MaxBusinessIDRunes+1) },
	}
	for name, mutate := range cases {
		input := spoolableInput()
		mutate(&input)
		if err := validateInput("tenant-1", input); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s 超长 err = %v, want ErrValidation", name, err)
		}
	}
}

// 限长按字符数（rune）判：中文名称等于列宽时必须放行，不能因为 UTF-8 字节数被拒。
func TestValidateInputAcceptsSnapshotFieldsAtColumnWidth(t *testing.T) {
	input := spoolableInput()
	input.ActorName = strings.Repeat("名", MaxActorNameRunes)
	input.ResourceName = strings.Repeat("名", MaxResourceNameRunes)
	input.ReasonCode = strings.Repeat("R", MaxReasonCodeRunes)
	input.BusinessID = strings.Repeat("B", MaxBusinessIDRunes)
	if err := validateInput("tenant-1", input); err != nil {
		t.Fatalf("等于列宽的取值应放行：%v", err)
	}
}
