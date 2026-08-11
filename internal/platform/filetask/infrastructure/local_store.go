// Package infrastructure contains the GORM/MySQL and local-directory implementations for filetask.
package infrastructure

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalStore 只接受可信根目录下的相对路径，并拒绝绝对路径、穿越片段及检查时可见的符号链接，
// 用于降低数据库路径被篡改后的目录逃逸风险。检查与打开/删除不是基于同一目录文件描述符，
// 因而不能把这层校验视为可抵御并发替换目录的完整沙箱。
type LocalStore struct {
	root     string
	realRoot string
}

func NewLocalStore(root string) (*LocalStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("local file storage root is required")
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("resolve file storage root: %w", err)
	}
	if err := os.MkdirAll(absoluteRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create file storage root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve file storage root symlinks: %w", err)
	}
	return &LocalStore{root: absoluteRoot, realRoot: realRoot}, nil
}

// WriteAtomically 将上传流写入同目录临时文件，完成 fsync 后原子 rename 为最终路径。
// 临时后缀与 CleanupTemporary 约定一致，使进程崩溃留下的半文件可被定向清理。
func (store *LocalStore) WriteAtomically(ctx context.Context, relativePath string, content io.Reader, maxBytes int64) (uint64, []byte, error) {
	if content == nil || maxBytes <= 0 {
		return 0, nil, errors.New("upload content and positive max bytes are required")
	}
	absolutePath, err := store.safePath(relativePath)
	if err != nil {
		return 0, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		return 0, nil, fmt.Errorf("create file directory: %w", err)
	}
	if err := store.ensureDirectoryInsideRoot(filepath.Dir(absolutePath)); err != nil {
		return 0, nil, err
	}

	temporaryPath := absolutePath + fmt.Sprintf(".part-%d", time.Now().UTC().UnixNano())
	output, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, nil, fmt.Errorf("create upload temporary file: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = output.Close()
			_ = os.Remove(temporaryPath)
		}
	}()

	hash := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(output, hash), content, maxBytes)
	if err != nil {
		return 0, nil, err
	}
	if err := output.Sync(); err != nil {
		return 0, nil, fmt.Errorf("fsync upload temporary file: %w", err)
	}
	if err := output.Close(); err != nil {
		return 0, nil, fmt.Errorf("close upload temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, absolutePath); err != nil {
		return 0, nil, fmt.Errorf("atomically move upload file: %w", err)
	}
	complete = true
	return uint64(written), hash.Sum(nil), nil
}

func (store *LocalStore) OpenVerified(relativePath string) (io.ReadSeekCloser, error) {
	// safePath 校验字符串边界后仍需 Lstat：合法路径上的符号链接也可能在运行时把读取重定向到根目录外。
	absolutePath, err := store.safePath(relativePath)
	if err != nil {
		return nil, err
	}
	if err := store.ensureDirectoryInsideRoot(filepath.Dir(absolutePath)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("stat local file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("local file is not a regular file")
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("open local file: %w", err)
	}
	return file, nil
}

func (store *LocalStore) Remove(relativePath string) error {
	absolutePath, err := store.safePath(relativePath)
	if err != nil {
		return err
	}
	if err := store.ensureDirectoryInsideRoot(filepath.Dir(absolutePath)); err != nil {
		return err
	}
	info, err := os.Lstat(absolutePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat file before removal: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("refuse to remove non-regular local file")
	}
	if err := os.Remove(absolutePath); err != nil {
		return fmt.Errorf("remove local file: %w", err)
	}
	return nil
}

// CleanupTemporary 只删除本模块生成且早于截止时间的 .part-* 文件；遍历时跳过符号链接，
// 不跟随任何可能指向存储根目录外的目录项。
func (store *LocalStore) CleanupTemporary(cutoff time.Time) (int, error) {
	removed := 0
	err := filepath.WalkDir(store.realRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == store.realRoot {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.Contains(entry.Name(), ".part-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().UTC().Before(cutoff.UTC()) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

func (store *LocalStore) safePath(relativePath string) (string, error) {
	relativePath = filepath.Clean(strings.TrimSpace(relativePath))
	if relativePath == "." || filepath.IsAbs(relativePath) || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || relativePath == ".." {
		return "", errors.New("unsafe local storage relative path")
	}
	absolutePath := filepath.Join(store.root, relativePath)
	if !isWithin(store.root, absolutePath) {
		return "", errors.New("local storage path escapes root")
	}
	return absolutePath, nil
}

func (store *LocalStore) ensureDirectoryInsideRoot(directory string) error {
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve local storage directory: %w", err)
	}
	if !isWithin(store.realRoot, realDirectory) {
		return errors.New("local storage directory escapes root through symlink")
	}
	return nil
}

func isWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader, maxBytes int64) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > maxBytes {
				return 0, errors.New("upload exceeds configured maximum size")
			}
			if _, err := destination.Write(buffer[:count]); err != nil {
				return 0, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return 0, readErr
		}
	}
}
