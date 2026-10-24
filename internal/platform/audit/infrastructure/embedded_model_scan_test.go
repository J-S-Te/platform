package infrastructure

import (
	"reflect"
	"testing"
)

// database/sql 只能写回可设置的字段。匿名嵌入未导出模型时，通过该嵌入字段提升出来的
// 字段不可设置，Scan 会静默跳过它们：归档事件和接收回执只剩应用/环境元数据，事件本体字段
// 全部为空。这里用反射直接验证"事件/回执字段可写回"，任何退回匿名未导出嵌入的改动都会失败。

func assertRowFieldsSettable(t *testing.T, sample any, fields ...string) {
	t.Helper()
	value := reflect.ValueOf(sample)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Struct {
		t.Fatalf("样例必须是结构体指针，实际 %T", sample)
	}
	element := value.Elem()
	for index := 0; index < element.NumField(); index++ {
		field := element.Type().Field(index)
		if !element.Field(index).CanSet() {
			t.Errorf("字段 %s 不可写：Scan 会静默丢弃对应的列", field.Name)
		}
	}
	for _, name := range fields {
		field := element.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("缺少字段 %s", name)
			continue
		}
		if !field.CanSet() {
			t.Errorf("字段 %s 不可写：Scan 会静默丢弃对应的列", name)
		}
	}
}

// assertNestedFieldsSettable 验证命名嵌入字段（gorm:"embedded"）自身及其内部列字段可写。
func assertNestedFieldsSettable(t *testing.T, sample any, nested string, fields ...string) {
	t.Helper()
	element := reflect.ValueOf(sample).Elem()
	container := element.FieldByName(nested)
	if !container.IsValid() {
		t.Fatalf("缺少命名嵌入字段 %s", nested)
	}
	if !container.CanSet() {
		t.Errorf("命名嵌入字段 %s 不可写", nested)
	}
	if container.Kind() != reflect.Struct {
		t.Fatalf("%s 必须是结构体，实际 %s", nested, container.Kind())
	}
	for _, name := range fields {
		field := container.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("%s 缺少列字段 %s", nested, name)
			continue
		}
		if !field.CanSet() {
			t.Errorf("%s.%s 不可写：Scan 会静默丢弃对应的列", nested, name)
		}
	}
}

func TestRetentionEventRowKeepsEventFieldsWritable(t *testing.T) {
	assertRowFieldsSettable(t, &retentionEventRow{}, "Event", "ApplicationCode", "ApplicationName", "EnvironmentCode")
	// 归档写出的 JSON 依赖这些事件字段；它们必须能真正被写回。
	assertNestedFieldsSettable(t, &retentionEventRow{}, "Event",
		"ID", "OccurredMonth", "EventID", "TenantID", "ApplicationID", "EnvironmentID",
		"EventCategory", "EventType", "OccurredAt", "ReceivedAt")
}

func TestIngestionReceiptRowKeepsReceiptFieldsWritable(t *testing.T) {
	assertRowFieldsSettable(t, &ingestionReceiptRow{}, "Receipt", "ApplicationCode", "ApplicationName", "EnvironmentCode")
	assertNestedFieldsSettable(t, &ingestionReceiptRow{}, "Receipt",
		"TenantID", "ApplicationID", "EnvironmentID", "ClientID", "RequestID", "TraceID",
		"CorrelationID", "EventCount", "AcceptedCount", "DuplicateCount", "Status", "ReceivedAt")
}
