package infrastructure

import "testing"

func TestObjectKeyRejectsTraversalAndEmptyValues(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../secret", `..\secret`, "tenant/../secret", "tenant/../../secret", "bad\x00key"} {
		if value == "bad\\x00key" {
			value = "bad\x00key"
		}
		if _, err := objectKey(value); err == nil {
			t.Fatalf("objectKey(%q) succeeded, want validation error", value)
		}
	}
}

func TestS3ObjectStoreKeyPreservesConfiguredPrefix(t *testing.T) {
	store := &S3ObjectStore{bucket: "files", prefix: "production/files"}
	key, err := store.key("tenant/app/file.pdf")
	if err != nil {
		t.Fatalf("key() error = %v", err)
	}
	if key != "production/files/tenant/app/file.pdf" {
		t.Fatalf("key() = %q", key)
	}
}
