package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalStoreStagesBeforeAtomicPublish(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("validated-content")
	stageID, size, digest, err := store.Stage(context.Background(), bytes.NewReader(content), 1024)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(content)
	if size != uint64(len(content)) || !bytes.Equal(digest, wantDigest[:]) {
		t.Fatalf("stage metadata mismatch: size=%d digest=%x", size, digest)
	}
	finalRelative := "project/report/tenant/2026/09/file/version/content"
	if _, err := os.Stat(filepath.Join(root, finalRelative)); !os.IsNotExist(err) {
		t.Fatalf("formal file exists before validation/publish: %v", err)
	}
	staged, err := store.OpenStaged(stageID)
	if err != nil {
		t.Fatal(err)
	}
	_ = staged.Close()
	if err := store.PublishStaged(stageID, finalRelative); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, finalRelative))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("file mode=%o want=640", info.Mode().Perm())
	}
}

func TestLocalStoreRejectsAuxiliaryAndStageEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if _, err := NewLocalStoreWithPaths(root, outside, filepath.Join(root, "quarantine")); err == nil {
		t.Fatal("temporary root outside storage root was accepted")
	}
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, stageID := range []string{"", "../escape.uploading", "/tmp/escape.uploading", "not-staged"} {
		if _, err := store.OpenStaged(stageID); err == nil {
			t.Fatalf("unsafe stage id %q was accepted", stageID)
		}
	}
}

func TestLocalStoreQuarantinesRejectedStagedContent(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	stageID, _, _, err := store.Stage(context.Background(), bytes.NewReader([]byte("rejected")), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineStaged(stageID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenStaged(stageID); !os.IsNotExist(err) {
		t.Fatalf("stage still exists after quarantine: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".rejected" {
		t.Fatalf("unexpected quarantine contents: %#v", entries)
	}
}

func TestLocalStoreRejectsHardLinkedFormalFile(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	relative := "crm/opportunity-attachment/tenant/2026/09/file/version/content"
	target := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, target); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := store.OpenVerified(relative); err == nil {
		t.Fatal("hard-linked file was accepted")
	}
}

func TestLocalStoreValidatesCapacityRejectionThreshold(t *testing.T) {
	root := t.TempDir()
	if _, err := NewLocalStoreWithCapacityLimit(root, "", "", 0); err == nil {
		t.Fatal("zero capacity threshold was accepted")
	}
	store, err := NewLocalStoreWithCapacityLimit(root, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := store.UsagePercent()
	if err != nil || usage > 100 {
		t.Fatalf("usage=%d err=%v", usage, err)
	}
}
