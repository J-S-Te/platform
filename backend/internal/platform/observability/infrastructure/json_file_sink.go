package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
)

// JSONFileSink appends structured runtime logs to a daily JSONL file under a controlled directory.
// It is intended for platform-local diagnostics; deployment log agents may tail these files.
type JSONFileSink struct {
	root string
	mu   sync.Mutex
}

func NewJSONFileSink(root string) (*JSONFileSink, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("structured log root is required")
	}
	return &JSONFileSink{root: root}, nil
}
func (sink *JSONFileSink) WriteStructuredLog(ctx context.Context, record domain.LogRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	date := record.Timestamp.UTC().Format("2006-01-02")
	relative := filepath.Join(record.Resource.TenantID, record.Resource.ApplicationID, date+".jsonl")
	absolute, err := safeRuntimePath(sink.root, relative)
	if err != nil {
		return err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(absolute), 0o750); err != nil {
		return fmt.Errorf("create structured log directory: %w", err)
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open structured log: %w", err)
	}
	defer file.Close()
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode structured log: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("append structured log: %w", err)
	}
	return nil
}
func safeRuntimePath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("runtime log path must be relative")
	}
	absolute := filepath.Join(root, relative)
	resolved, err := filepath.Rel(root, absolute)
	if err != nil || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return "", errors.New("runtime log path escapes root")
	}
	return absolute, nil
}

var _ = time.Second
