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
	"syscall"
	"time"
)

// LocalStore 只接受可信根目录下的相对路径，并拒绝绝对路径、穿越片段及检查时可见的符号链接，
// 用于降低数据库路径被篡改后的目录逃逸风险。检查与打开/删除不是基于同一目录文件描述符，
// 因而不能把这层校验视为可抵御并发替换目录的完整沙箱。
type LocalStore struct {
	root, realRoot                string
	temporaryRoot, quarantineRoot string
	rejectUsagePercent            uint64
}

// Ready 验证生产本地存储根仍存在且为目录。
func (store *LocalStore) Ready(_ context.Context) error {
	if store == nil || store.realRoot == "" {
		return errors.New("local file store is not configured")
	}
	info, err := os.Stat(store.realRoot)
	if err != nil {
		return fmt.Errorf("stat local file storage root: %w", err)
	}
	if !info.IsDir() {
		return errors.New("local file storage root is not a directory")
	}
	return nil
}

func NewLocalStore(root string) (*LocalStore, error) {
	return NewLocalStoreWithPaths(root, filepath.Join(root, "temporary"), filepath.Join(root, "quarantine"))
}

// NewLocalStoreWithPaths 将上传半成品和被拒绝文件限制在存储根内的专用目录。
// 三个目录必须位于同一文件系统，以保持校验成功后的原子 rename 语义。
func NewLocalStoreWithPaths(root, temporaryRoot, quarantineRoot string) (*LocalStore, error) {
	return NewLocalStoreWithCapacityLimit(root, temporaryRoot, quarantineRoot, 90)
}

func NewLocalStoreWithCapacityLimit(root, temporaryRoot, quarantineRoot string, rejectUsagePercent uint64) (*LocalStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("local file storage root is required")
	}
	if rejectUsagePercent < 1 || rejectUsagePercent > 100 {
		return nil, errors.New("local file storage rejection threshold must be between 1 and 100")
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
	resolveChild := func(value, fallback string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			value = fallback
		}
		absolute, resolveErr := filepath.Abs(filepath.Clean(value))
		if resolveErr != nil {
			return "", resolveErr
		}
		if !isWithin(absoluteRoot, absolute) {
			return "", errors.New("local auxiliary directory must remain below storage root")
		}
		if resolveErr = os.MkdirAll(absolute, 0o750); resolveErr != nil {
			return "", resolveErr
		}
		real, resolveErr := filepath.EvalSymlinks(absolute)
		if resolveErr != nil {
			return "", resolveErr
		}
		if !isWithin(realRoot, real) {
			return "", errors.New("local auxiliary directory escapes storage root")
		}
		return real, nil
	}
	temporary, err := resolveChild(temporaryRoot, filepath.Join(absoluteRoot, "temporary"))
	if err != nil {
		return nil, fmt.Errorf("configure temporary storage: %w", err)
	}
	quarantine, err := resolveChild(quarantineRoot, filepath.Join(absoluteRoot, "quarantine"))
	if err != nil {
		return nil, fmt.Errorf("configure quarantine storage: %w", err)
	}
	return &LocalStore{root: absoluteRoot, realRoot: realRoot, temporaryRoot: temporary, quarantineRoot: quarantine, rejectUsagePercent: rejectUsagePercent}, nil
}

// WriteAtomically 是兼容旧调用方的便捷方法。新上传链路使用 Stage/OpenStaged/
// PublishStaged，确保内容校验完成前文件始终留在 temporary 目录。
func (store *LocalStore) WriteAtomically(ctx context.Context, relativePath string, content io.Reader, maxBytes int64) (uint64, []byte, error) {
	stageID, size, digest, err := store.Stage(ctx, content, maxBytes)
	if err != nil {
		return 0, nil, err
	}
	if err := store.PublishStaged(stageID, relativePath); err != nil {
		_ = store.RemoveStaged(stageID)
		return 0, nil, err
	}
	return size, digest, nil
}

// Stage 流式写入不可下载的 temporary 目录并 fsync。只有 PublishStaged 会把文件
// 原子移动到正式命名空间，因而校验失败的字节不会短暂出现在正式目录。
func (store *LocalStore) Stage(ctx context.Context, content io.Reader, maxBytes int64) (string, uint64, []byte, error) {
	if content == nil || maxBytes <= 0 {
		return "", 0, nil, errors.New("upload content and positive max bytes are required")
	}
	usage, err := store.UsagePercent()
	if err != nil {
		return "", 0, nil, fmt.Errorf("check local storage capacity: %w", err)
	}
	if usage >= store.rejectUsagePercent {
		return "", 0, nil, fmt.Errorf("local storage capacity rejection threshold reached: %d%%", usage)
	}

	stageID := fmt.Sprintf("%d-%d.uploading", time.Now().UTC().UnixNano(), os.Getpid())
	temporaryPath := filepath.Join(store.temporaryRoot, stageID)
	output, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", 0, nil, fmt.Errorf("create upload temporary file: %w", err)
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
		return "", 0, nil, err
	}
	if err := output.Sync(); err != nil {
		return "", 0, nil, fmt.Errorf("fsync upload temporary file: %w", err)
	}
	if err := output.Close(); err != nil {
		return "", 0, nil, fmt.Errorf("close upload temporary file: %w", err)
	}
	complete = true
	return stageID, uint64(written), hash.Sum(nil), nil
}

// UsagePercent 返回底层文件系统已用比例。Bavail 而不是 Bfree 反映运行账号实际可用容量。
func (store *LocalStore) UsagePercent() (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(store.realRoot, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 100, errors.New("local storage reports zero capacity")
	}
	available := uint64(stat.Bavail)
	total := uint64(stat.Blocks)
	if available > total {
		available = total
	}
	return (total - available) * 100 / total, nil
}

func (store *LocalStore) OpenStaged(stageID string) (io.ReadSeekCloser, error) {
	path, err := store.safeStagePath(stageID)
	if err != nil {
		return nil, err
	}
	return openRegular(path)
}

func (store *LocalStore) PublishStaged(stageID, relativePath string) error {
	stagedPath, err := store.safeStagePath(stageID)
	if err != nil {
		return err
	}
	absolutePath, err := store.safePath(relativePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		return fmt.Errorf("create file directory: %w", err)
	}
	if err := store.ensureDirectoryInsideRoot(filepath.Dir(absolutePath)); err != nil {
		return err
	}
	file, err := openRegular(stagedPath)
	if err != nil {
		return err
	}
	_ = file.Close()
	if err := os.Rename(stagedPath, absolutePath); err != nil {
		return fmt.Errorf("atomically publish upload file: %w", err)
	}
	return nil
}

func (store *LocalStore) RemoveStaged(stageID string) error {
	path, err := store.safeStagePath(stageID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (store *LocalStore) QuarantineStaged(stageID string) error {
	path, err := store.safeStagePath(stageID)
	if err != nil {
		return err
	}
	file, err := openRegular(path)
	if err != nil {
		return err
	}
	_ = file.Close()
	target := filepath.Join(store.quarantineRoot, strings.TrimSuffix(stageID, ".uploading")+".rejected")
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("quarantine rejected staged file: %w", err)
	}
	return nil
}

// Quarantine 把未通过内容校验的普通文件移出正式目录。隔离路径不保留原文件名，
// 且不会被 OpenVerified 的正常下载流程触及。
func (store *LocalStore) Quarantine(relativePath string) error {
	absolutePath, err := store.safePath(relativePath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		return err
	}
	if !safeRegularFile(info) {
		return errors.New("refuse to quarantine non-regular file")
	}
	target := filepath.Join(store.quarantineRoot, filepath.Base(filepath.Dir(absolutePath))+"-"+filepath.Base(absolutePath)+fmt.Sprintf("-%d.rejected", time.Now().UTC().UnixNano()))
	if err := os.Rename(absolutePath, target); err != nil {
		return fmt.Errorf("quarantine rejected file: %w", err)
	}
	return nil
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
	if !safeRegularFile(info) {
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
	if !safeRegularFile(info) {
		return errors.New("refuse to remove non-regular local file")
	}
	if err := os.Remove(absolutePath); err != nil {
		return fmt.Errorf("remove local file: %w", err)
	}
	return nil
}

// CleanupTemporary 只删除本模块生成且早于截止时间的临时文件；遍历时跳过符号链接，
// 不跟随任何可能指向存储根目录外的目录项。
func (store *LocalStore) CleanupTemporary(cutoff time.Time) (int, error) {
	removed := 0
	err := filepath.WalkDir(store.temporaryRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == store.temporaryRoot {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".uploading") && !strings.Contains(entry.Name(), ".part-")) {
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

func (store *LocalStore) safeStagePath(stageID string) (string, error) {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" || filepath.Base(stageID) != stageID || !strings.HasSuffix(stageID, ".uploading") {
		return "", errors.New("unsafe upload stage identifier")
	}
	path := filepath.Join(store.temporaryRoot, stageID)
	if !isWithin(store.temporaryRoot, path) {
		return "", errors.New("upload stage escapes temporary root")
	}
	return path, nil
}

func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !safeRegularFile(info) {
		return nil, errors.New("local file is not a regular file")
	}
	return os.Open(path)
}

func safeRegularFile(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink == 1
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
