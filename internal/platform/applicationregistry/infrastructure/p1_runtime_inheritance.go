package infrastructure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type runtimeEnvironmentUpdate struct {
	path   string
	values map[string]string
}

func usableProductionCredential(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "PENDING_ONBOARDING" && !strings.HasPrefix(value, "REPLACE_WITH_")
}
func isP1ProductionServiceBinding(source string) bool {
	for _, purpose := range []string{"settlement_contract_ingest", "portal_report_file_read", "portal_report_download_identity_read"} {
		if source == "service."+purpose+".client_id" || source == "service."+purpose+".client_secret" {
			return true
		}
	}
	return false
}
func inheritProductionRuntimeValues(root string, file productionSubsystemRuntimeFileManifest, staged map[string]map[string]string, values map[string]string) error {
	if file.InheritFrom == "" {
		return nil
	}
	source, ok := staged[file.InheritFrom]
	if !ok {
		return provisioningError("runtime inheritance source has not been prepared")
	}
	sourcePath := filepath.Join(root, filepath.FromSlash(file.InheritFrom))
	resolved, err := filepath.EvalSymlinks(sourcePath)
	if err != nil || resolved != sourcePath || !pathWithinRoot(root, resolved) {
		return provisioningError("runtime inheritance source path is unsafe")
	}
	if !validProductionGeneratedEnvironmentValue("PORTAL_REPORT_CONTENT_KEY_BASE64", source["PORTAL_REPORT_CONTENT_KEY_BASE64"]) || !validProductionGeneratedEnvironmentValue("PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", source["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"]) || source["PORTAL_REPORT_CONTENT_KEY_BASE64"] == source["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"] {
		return provisioningError("report vault requires distinct valid content and descriptor keys")
	}
	for _, key := range file.InheritKeys {
		if !allowedProductionInheritedKey(key) || !usableProductionCredential(source[key]) {
			return provisioningError("runtime inherited value is unavailable")
		}
		values[key] = source[key]
	}
	return nil
}

// 先暂存并 fsync 全部文件，再统一发布；只有在整组成功后才重启消费者。重命名失败时
// 恢复已发布的文件——防止受控重试生成另一把 API/worker 密钥。
func updateProductionRuntimeGroup(updates []runtimeEnvironmentUpdate) error {
	type stagedFile struct{ path, temp, backup string }
	prepared := make([]stagedFile, 0, len(updates))
	defer func() {
		for _, file := range prepared {
			_ = os.Remove(file.temp)
			_ = os.Remove(file.backup)
		}
	}()
	for _, update := range updates {
		resolved, err := filepath.EvalSymlinks(update.path)
		if err != nil || resolved != update.path {
			return provisioningError("runtime environment path is unsafe")
		}
		info, err := os.Lstat(update.path)
		if err != nil || !info.Mode().IsRegular() {
			return provisioningError("runtime environment file is unavailable")
		}
		content, err := os.ReadFile(update.path)
		if err != nil {
			return provisioningError("read runtime environment group")
		}
		names := make([]string, 0, 2)
		for i := 0; i < 2; i++ {
			file, err := os.CreateTemp(filepath.Dir(update.path), ".runtime-group-*")
			if err != nil {
				for _, name := range names {
					_ = os.Remove(name)
				}
				return provisioningError("stage runtime environment group")
			}
			name := file.Name()
			names = append(names, name)
			if err = file.Chmod(0600); err == nil {
				_, err = file.Write(content)
			}
			if err == nil {
				if stat, ok := info.Sys().(*syscall.Stat_t); ok {
					err = file.Chown(int(stat.Uid), int(stat.Gid))
				}
			}
			if err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				for _, name := range names {
					_ = os.Remove(name)
				}
				return provisioningError("stage runtime environment group")
			}
		}
		prepared = append(prepared, stagedFile{update.path, names[0], names[1]})
		if err := updateProductionSubsystemEnvironment(names[0], update.values); err != nil {
			return provisioningError("validate runtime environment group")
		}
	}
	for index, file := range prepared {
		if err := os.Rename(file.temp, file.path); err != nil {
			var rollbackErr error
			for rollback := index - 1; rollback >= 0; rollback-- {
				if err := os.Rename(prepared[rollback].backup, prepared[rollback].path); err != nil {
					rollbackErr = errors.Join(rollbackErr, err)
				}
			}
			if rollbackErr != nil {
				return fmt.Errorf("runtime publication and restoration failed: %w", rollbackErr)
			}
			return provisioningError("publish runtime environment group")
		}
	}
	directories := map[string]bool{}
	for _, file := range prepared {
		directory := filepath.Dir(file.path)
		if directories[directory] {
			continue
		}
		directories[directory] = true
		dir, err := os.Open(directory)
		if err != nil {
			return provisioningError("sync runtime environment group")
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return provisioningError("sync runtime environment group")
		}
	}
	return nil
}
