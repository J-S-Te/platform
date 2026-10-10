package infrastructure

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/schema"
)

// Guard both aggregate read paths against silently dropping the model fields.
func TestDictionaryProjectionMapsDictionaryAndAggregateColumns(t *testing.T) {
	parsed, err := schema.Parse(&dictionaryProjection{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	values := map[string]any{
		"id": "dictionary-id", "tenant_id": "tenant-a", "code": "business-kind",
		"name": "业务类型", "description": "受控目录", "status": "ACTIVE",
		"version": uint64(7), "created_at": now, "updated_at": now, "item_count": int64(3),
	}
	var projection dictionaryProjection
	for column, value := range values {
		field := parsed.FieldsByDBName[column]
		if field == nil {
			t.Fatalf("projection has no scan mapping for %s", column)
		}
		if err := field.Set(context.Background(), reflect.ValueOf(&projection).Elem(), value); err != nil {
			t.Fatalf("set %s: %v", column, err)
		}
	}
	result := dictionaryToDomain(projection.Dictionary, projection.ItemCount)
	if result.ID != "dictionary-id" || result.Code != "business-kind" || result.Name != "业务类型" ||
		string(result.Status) != "ACTIVE" || result.Version != 7 || result.ItemCount != 3 {
		t.Fatalf("projection lost persisted fields: %#v", result)
	}
}
