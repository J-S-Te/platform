package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestLocalStoreCleanupTemporaryOnlyRemovesUploadFragments 锁定边界：通用清理只删除
// 临时目录内的 .uploading/.part- 上传碎片；正式命名空间文件、其它名称临时文件与
// 符号链接必须保持字节不变（不跟随、不误删）。
func TestLocalStoreCleanupTemporaryOnlyRemovesUploadFragments(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile := func(path string, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stageFragment := filepath.Join(store.temporaryRoot, "01STAGE000000000000000001.uploading")
	partFragment := filepath.Join(store.temporaryRoot, "blob.part-7")
	keepTemp := filepath.Join(store.temporaryRoot, "keep.txt")
	formal := filepath.Join(root, "namespace", "report.bin")
	writeFile(stageFragment, "stage")
	writeFile(partFragment, "part")
	writeFile(keepTemp, "keep")
	writeFile(formal, "formal-bytes")
	link := filepath.Join(store.temporaryRoot, "linked.uploading")
	if err := os.Symlink(formal, link); err != nil {
		t.Fatal(err)
	}

	removed, err := store.CleanupTemporary(time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want exactly the two upload fragments", removed)
	}
	for _, path := range []string{stageFragment, partFragment} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("fragment %s still exists: %v", filepath.Base(path), err)
		}
	}
	if content, err := os.ReadFile(keepTemp); err != nil || string(content) != "keep" {
		t.Fatalf("non-fragment temp file changed: %q err=%v", content, err)
	}
	if content, err := os.ReadFile(formal); err != nil || string(content) != "formal-bytes" {
		t.Fatalf("formal file changed: %q err=%v", content, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink entry must be left untouched: %v", err)
	}
}

