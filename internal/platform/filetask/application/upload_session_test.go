package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

type uploadSessionRepositoryStub struct {
	*bindingRepositoryStub
	mutex  sync.Mutex
	stored domain.StoredFile
	status string
	writes int
}

func newUploadSessionRepositoryStub() *uploadSessionRepositoryStub {
	return &uploadSessionRepositoryStub{bindingRepositoryStub: &bindingRepositoryStub{}}
}

func (stub *uploadSessionRepositoryStub) ReserveUpload(_ context.Context, file domain.File, version domain.FileVersion) (domain.StoredFile, bool, error) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	if stub.status == "" {
		stub.status = "WRITING"
		stub.stored = domain.StoredFile{File: file, Version: version}
		return stub.stored, true, nil
	}
	if !bytes.Equal(stub.stored.Version.UploadRequestHash, version.UploadRequestHash) {
		return domain.StoredFile{}, false, ErrConflict
	}
	if stub.status != "READY" {
		return domain.StoredFile{}, false, ErrConflict
	}
	return stub.stored, false, nil
}

func (stub *uploadSessionRepositoryStub) MarkValidating(_ context.Context, _ string, _ string, size uint64, digest []byte, _ time.Time) error {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.stored.Version.SizeBytes = size
	stub.stored.Version.SHA256 = append([]byte(nil), digest...)
	stub.stored.File.Status = domain.FileStatusValidating
	stub.stored.Version.Status = domain.FileVersionStatusValidating
	return nil
}

func (stub *uploadSessionRepositoryStub) MarkReady(context.Context, string, string, time.Time) error {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.status = "READY"
	stub.stored.File.Status = domain.FileStatusReady
	stub.stored.Version.Status = domain.FileVersionStatusReady
	return nil
}

func (stub *uploadSessionRepositoryStub) MarkFailed(context.Context, string, string, time.Time) error {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.status = "FAILED"
	stub.stored.File.Status = domain.FileStatusFailed
	stub.stored.Version.Status = domain.FileVersionStatusFailed
	return nil
}

type uploadSessionStoreStub struct {
	mutex   sync.Mutex
	content []byte
	writes  int
	fail    bool
}

func (stub *uploadSessionStoreStub) WriteAtomically(_ context.Context, _ string, source io.Reader, _ int64) (uint64, []byte, error) {
	content, err := io.ReadAll(source)
	if err != nil {
		return 0, nil, err
	}
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.writes++
	if stub.fail {
		return 0, nil, errors.New("object store unavailable")
	}
	stub.content = append([]byte(nil), content...)
	digest := sha256.Sum256(content)
	return uint64(len(content)), digest[:], nil
}

func (stub *uploadSessionStoreStub) OpenVerified(string) (io.ReadSeekCloser, error) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	return &readSeekCloser{Reader: bytes.NewReader(append([]byte(nil), stub.content...))}, nil
}

func (*uploadSessionStoreStub) Remove(string) error                     { return nil }
func (*uploadSessionStoreStub) CleanupTemporary(time.Time) (int, error) { return 0, nil }

func newUploadSessionService(t *testing.T, repository *uploadSessionRepositoryStub, store *uploadSessionStoreStub) *FileService {
	t.Helper()
	service, err := NewFileService(repository, store, fixedIDGenerator{id: "01K00000000000000000000000"}, fixedClock{value: time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func uploadSessionInput(content string) UploadInput {
	return UploadInput{
		TenantID: "01TENANT000000000000000000", ApplicationID: "01APP000000000000000000000",
		OwnerUserID: "01USER00000000000000000000", OriginalName: "evidence.txt",
		DeclaredMediaType: "text/plain", Classification: "CONFIDENTIAL", RequestID: "upload-request-1",
		Content: bytes.NewBufferString(content),
	}
}

func TestUploadSessionConcurrentReplayReturnsSameFileAndWritesOnce(t *testing.T) {
	repository := newUploadSessionRepositoryStub()
	store := &uploadSessionStoreStub{}
	service := newUploadSessionService(t, repository, store)

	const callers = 16
	results := make(chan domain.File, callers)
	errorsFound := make(chan error, callers)
	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	for range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			file, err := service.Upload(context.Background(), uploadSessionInput("same payload"))
			if err != nil {
				errorsFound <- err
				return
			}
			results <- file
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)
	close(errorsFound)

	for err := range errorsFound {
		// 与首个请求真正并发、会话尚为 WRITING 时允许返回冲突；响应绝不能伪装成功。
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("unexpected concurrent upload error: %v", err)
		}
	}
	for file := range results {
		if file.ID != "01K00000000000000000000000" {
			t.Fatalf("replay returned another file: %#v", file)
		}
	}
	store.mutex.Lock()
	writes := store.writes
	store.mutex.Unlock()
	if writes != 1 {
		t.Fatalf("object content written %d times, want 1", writes)
	}

	// 首次上传完成后的相同重放必须稳定返回同一个文件，而不是创建新版本。
	replayed, err := service.Upload(context.Background(), uploadSessionInput("same payload"))
	if err != nil || replayed.ID != "01K00000000000000000000000" {
		t.Fatalf("completed replay result = %#v, %v", replayed, err)
	}
}

func TestUploadSessionRejectsHashConflictAndFailedReplay(t *testing.T) {
	t.Run("different complete request hash", func(t *testing.T) {
		repository := newUploadSessionRepositoryStub()
		store := &uploadSessionStoreStub{}
		service := newUploadSessionService(t, repository, store)
		if _, err := service.Upload(context.Background(), uploadSessionInput("first payload")); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Upload(context.Background(), uploadSessionInput("changed payload")); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed payload error = %v, want conflict", err)
		}
	})

	t.Run("failed session is never reusable success", func(t *testing.T) {
		repository := newUploadSessionRepositoryStub()
		store := &uploadSessionStoreStub{fail: true}
		service := newUploadSessionService(t, repository, store)
		if _, err := service.Upload(context.Background(), uploadSessionInput("same payload")); !errors.Is(err, ErrStorage) {
			t.Fatalf("first upload error = %v, want storage error", err)
		}
		if _, err := service.Upload(context.Background(), uploadSessionInput("same payload")); !errors.Is(err, ErrConflict) {
			t.Fatalf("failed replay error = %v, want conflict", err)
		}
		store.mutex.Lock()
		writes := store.writes
		store.mutex.Unlock()
		if writes != 1 {
			t.Fatalf("failed replay wrote object %d times, want 1", writes)
		}
	})
}
