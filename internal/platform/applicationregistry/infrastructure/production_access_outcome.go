package infrastructure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const maxAccessOutcomeBytes = 48 << 10

// serviceCredentialRetryError 保留真实凭据校验拒绝的明确类别。
// 参数：无，仅在尚未开始部署的凭据校验点调用。
// 返回值：兼容既有部署不可用错误且可按sentinel识别的拒绝。
func serviceCredentialRetryError() error {
	return fmt.Errorf("%w: %w", application.ErrSubsystemProvisioningUnavailable,
		application.ErrServiceCredentialRetryRequired)
}

// accessOutcomePath 将结果限定到原回执的不可猜测绑定文件。
// 参数：root为受控部署根，receipt为目标及原代次。
// 返回值：结果路径；绑定不完整时返回错误。
func accessOutcomePath(root string, receipt application.SubsystemAccessDeploymentReceipt) (string, error) {
	path, err := accessReceiptPath(root, receipt)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "runtime", ".access-deployment-outcomes", filepath.Base(path)), nil
}

// validateAccessOutcome 校验结果类别与精确回执，不把人工决定冒充部署事实。
// 参数：value为Agent持久结果，receipt为本次期待的原始绑定。
// 返回值：类别、操作者或绑定异常时返回错误。
func validateAccessOutcome(value application.SubsystemAccessDeploymentOutcome,
	receipt application.SubsystemAccessDeploymentReceipt) error {
	valid := value.State == application.AccessDeploymentRejected &&
		value.Code == application.AccessCredentialRetryRequired && value.OperatorID == "" &&
		value.Reason == "" && value.EvidenceDigest == "" && len(value.RuntimeFiles) == 0 && len(value.Containers) == 0
	if value.State == application.AccessDeploymentAbandoned {
		digest, err := accessRecoveryEvidenceDigest(receipt, value.RuntimeFiles, value.Containers)
		valid = value.Code == application.AccessOperatorAbandoned &&
			strings.TrimSpace(value.OperatorID) != "" && len(value.OperatorID) <= 64 &&
			utf8.ValidString(value.Reason) && utf8.RuneCountInString(strings.TrimSpace(value.Reason)) >= 10 &&
			utf8.RuneCountInString(value.Reason) <= 500 &&
			len(value.RuntimeFiles) > 0 && len(value.Containers) > 0 &&
			err == nil && value.EvidenceDigest == digest
	}
	if !valid || value.Receipt != receipt || value.RecordedAt.IsZero() {
		return provisioningError("deployment outcome binding or classification is invalid")
	}
	return nil
}

// privateAccessDirectory 拒绝运行目录及结果目录的符号链接和宽松权限。
// 参数：root为部署根，directory为结果目录，create表示写入时允许创建。
// 返回值：目录安全时为空；不存在的只读目录也为空。
func privateAccessDirectory(root, directory string, create bool) error {
	return privateAccessDirectoryWithSync(root, directory, create, syncAccessDirectory)
}

// syncAccessDirectory 将目录项写入主机持久存储。
// 参数：path为已核验的目录，不读取其中配置内容。
// 返回值：打开或同步失败返回错误，不得视为持久化完成。
func syncAccessDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// confirmAccessProofDurability 重新确认可读证明的文件与目录项均已持久化。
// 参数：root/path限定原证明，file为已验证打开的文件，syncDirectory同步主机目录。
// 返回值：同步失败返回错误，避免上次rename后的同步失败被重试读取绕过。
func confirmAccessProofDurability(root, path string, file *os.File, syncDirectory func(string) error) error {
	if err := privateAccessDirectoryWithSync(root, filepath.Dir(path), true, syncDirectory); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// privateAccessDirectoryWithSync 为目录校验保留可注入的主机同步边界。
// 参数：root/directory限定受控目录，create允许创建，syncDirectory同步父目录项。
// 返回值：安全校验、创建或同步失败时返回错误。
func privateAccessDirectoryWithSync(root, directory string, create bool, syncDirectory func(string) error) error {
	runtime := filepath.Join(root, "runtime")
	for _, path := range []string{root, runtime, directory} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(path)
		}
		if errors.Is(err, os.ErrNotExist) && !create {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0002 != 0 ||
			(path == directory && info.Mode().Perm()&0077 != 0) {
			return provisioningError("deployment outcome directory is unsafe")
		}
		// 同步父目录项，包括上次同步失败后已经存在的目录，才能安全确认栅栏持久化。
		if create {
			if err = syncDirectory(filepath.Dir(path)); err != nil {
				return err
			}
		}
	}
	return nil
}

// writePrivateAccessJSON 以受限临时文件和目录同步原子保存结果。
// 参数：path为已核验目录内的文件，value为不含秘密的结果。
// 返回值：写入或同步失败时返回错误。
func writePrivateAccessJSON(path string, value any) error {
	return writePrivateAccessJSONWithSync(path, value, syncAccessDirectory)
}

// writePrivateAccessJSONWithSync 在主机同步边界可注入时保存真实文件。
// 参数：path为受控结果路径，value为结果，syncDirectory持久化目录项。
// 返回值：任一步失败返回错误，rename后文件存在也不等于持久成功。
func writePrivateAccessJSONWithSync(path string, value any, syncDirectory func(string) error) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".outcome-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if err = json.NewEncoder(file).Encode(value); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// readAccessDeploymentOutcome 只读取权限及绑定均正确的真实结果。
// 参数：root为部署根，receipt为期待的目标及代次。
// 返回值：不存在返回found=false；不安全、损坏或错绑定返回错误。
func readAccessDeploymentOutcome(root string, receipt application.SubsystemAccessDeploymentReceipt) (
	application.SubsystemAccessDeploymentOutcome, bool, error) {
	return readAccessDeploymentOutcomeWithSync(root, receipt, syncAccessDirectory)
}

// readAccessDeploymentOutcomeWithSync 在可注入的主机同步边界读取结果。
// 参数：root/receipt限定原结果，syncDirectory负责主机目录持久确认。
// 返回值：只有完成持久确认的结果才能标记存在，否则失败关闭。
func readAccessDeploymentOutcomeWithSync(root string, receipt application.SubsystemAccessDeploymentReceipt,
	syncDirectory func(string) error) (application.SubsystemAccessDeploymentOutcome, bool, error) {
	var value application.SubsystemAccessDeploymentOutcome
	path, err := accessOutcomePath(root, receipt)
	if err != nil {
		return value, false, err
	}
	if err = privateAccessDirectory(root, filepath.Dir(path), false); err != nil {
		return value, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > maxAccessOutcomeBytes {
		return value, false, provisioningError("deployment outcome file is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return value, false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxAccessOutcomeBytes))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&value); err != nil {
		return value, false, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return value, false, provisioningError("deployment outcome contains trailing data")
	}
	if err = validateAccessOutcome(value, receipt); err != nil {
		return value, false, err
	}
	successPath, err := accessReceiptPath(root, receipt)
	if err != nil {
		return value, false, err
	}
	if _, err = os.Lstat(successPath); !errors.Is(err, os.ErrNotExist) {
		return value, false, provisioningError("deployment has contradictory or unreadable success evidence")
	}
	if err = confirmAccessProofDurability(root, path, file, syncDirectory); err != nil {
		return value, false, err
	}
	return value, true, nil
}

// writeAccessDeploymentOutcome 在持有目标与发布锁时保存互斥结果。
// 参数：root为受控根，value为本次真实拒绝或人工终止决定。
// 返回值：已有成功、不同结果或不安全文件时拒绝，不覆盖事实。
func writeAccessDeploymentOutcome(root string, value application.SubsystemAccessDeploymentOutcome) error {
	if err := validateAccessOutcome(value, value.Receipt); err != nil {
		return err
	}
	if raw, err := json.Marshal(value); err != nil || len(raw)+1 > maxAccessOutcomeBytes {
		return provisioningError("deployment outcome exceeds bounded storage")
	}
	if completed, err := readAccessDeploymentReceipt(root, value.Receipt); err != nil || completed {
		return provisioningError("successful deployment cannot be rejected or abandoned")
	}
	if previous, found, err := readAccessDeploymentOutcome(root, value.Receipt); err != nil {
		return err
	} else if found {
		if previous.State == value.State && previous.Code == value.Code {
			return nil
		}
		return provisioningError("deployment already has a different terminal outcome")
	}
	path, err := accessOutcomePath(root, value.Receipt)
	if err != nil {
		return err
	}
	if err = privateAccessDirectory(root, filepath.Dir(path), true); err != nil {
		return err
	}
	return writePrivateAccessJSON(path, value)
}

// accessRecoveryEvidenceDigest 为受审目标的运行文件与容器事实生成有界摘要。
// 参数：receipt限定原代次，files及containers为实际观察且已排序的非秘密事实。
// 返回值：规范JSON摘要；超出受限协议容量或编码失败返回错误。
func accessRecoveryEvidenceDigest(receipt application.SubsystemAccessDeploymentReceipt,
	files []application.SubsystemAccessRecoveryFile, containers []application.SubsystemAccessRecoveryContainer) (string, error) {
	value := struct {
		Receipt    application.SubsystemAccessDeploymentReceipt   `json:"receipt"`
		Files      []application.SubsystemAccessRecoveryFile      `json:"runtime_files"`
		Containers []application.SubsystemAccessRecoveryContainer `json:"containers"`
	}{receipt, files, containers}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 32<<10 {
		return "", provisioningError("deployment recovery evidence exceeds bounded protocol")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// recordAccessCredentialRejection 仅在实际凭据校验拒绝后保存部署前结果。
// 参数：input为受信原代次，resultErr为目标函数的真实返回错误；调用方持有发布锁。
// 返回值：无；持久化失败抹去可恢复类别并保持未知，不制造拒绝证明。
func (target *productionComposeTarget) recordAccessCredentialRejection(
	input application.SubsystemProvisioningInput, resultErr *error) {
	if input.AccessReceipt == nil || !errors.Is(*resultErr, application.ErrServiceCredentialRetryRequired) {
		return
	}
	value := application.SubsystemAccessDeploymentOutcome{Receipt: *input.AccessReceipt,
		State: application.AccessDeploymentRejected, Code: application.AccessCredentialRetryRequired,
		RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(target.config.DeployRoot, value); err != nil {
		*resultErr = provisioningError("deployment credential rejection could not be persisted")
	}
}

// accessDeploymentOutcomeReference 生成可审计的非秘密结果摘要。
// 参数：value为Agent原样返回的持久结果。
// 返回值：规范JSON的SHA256引用。
func accessDeploymentOutcomeReference(value application.SubsystemAccessDeploymentOutcome) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return "deployment-outcome:sha256:" + hex.EncodeToString(digest[:])
}

// accessDeploymentAlreadyClosed 在锁内拦截迟到旧代次并幂等返回已完成任务。
// 参数：input为通过目标校验的部署输入。
// 返回值：已成功时为true；已拒绝/终止或证据不安全时返回错误。
func (target *productionComposeTarget) accessDeploymentAlreadyClosed(
	input application.SubsystemProvisioningInput) (bool, error) {
	if input.AccessReceipt == nil {
		return false, nil
	}
	if _, found, err := readAccessDeploymentOutcome(target.config.DeployRoot, *input.AccessReceipt); err != nil {
		return false, err
	} else if found {
		return false, provisioningError("deployment generation is durably rejected or abandoned")
	}
	return readAccessDeploymentReceipt(target.config.DeployRoot, *input.AccessReceipt)
}
