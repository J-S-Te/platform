package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// accessRecoveryConflict 表达事实变化或当前不能安全终止，不能据此重放部署。
// 参数：reason为不含运行秘密的固定诊断。
// 返回值：可由正式入口映射为冲突的错误。
func accessRecoveryConflict(reason string) error {
	return fmt.Errorf("%w: %s", application.ErrSubsystemAccessOperationConflict, reason)
}

// withAccessRecoveryLock 在同目标互斥锁及共用发布锁内核对并处理旧代次。
// 参数：ctx控制锁期限，request限定原目标及受信发布，action仅为固定内部操作。
// 返回值：活动部署、清单/签名漂移或固定操作失败时返回错误。
func (p *ProductionComposeSubsystemProvisioner) withAccessRecoveryLock(ctx context.Context,
	request application.SubsystemAccessAbandonRequest, action func(context.Context, *productionComposeTarget) error) error {
	target, err := p.target(request.Receipt.ApplicationCode, request.Receipt.Environment)
	if err != nil {
		return err
	}
	if err = target.validateTenant(request.Receipt.TenantID); err != nil {
		return err
	}
	if _, err = accessReceiptPath(target.config.DeployRoot, request.Receipt); err != nil {
		return err
	}
	if request.Receipt.ManifestChecksum != target.config.Profile.Checksum {
		return accessRecoveryConflict("deployment recovery manifest changed")
	}
	if !target.mutex.TryLock() {
		return accessRecoveryConflict("deployment is still executing")
	}
	defer target.mutex.Unlock()
	if err = privateAccessDirectory(target.config.DeployRoot,
		filepath.Join(target.config.DeployRoot, "runtime", ".access-deployment-outcomes"), false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lockCtx, cancelLock := context.WithTimeout(ctx, 2*time.Second)
	defer cancelLock()
	lock, err := acquireProvisioningFileLock(lockCtx, filepath.Join(target.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return accessRecoveryConflict("shared deployment lock is busy")
	}
	defer releaseProvisioningFileLock(lock)
	if err = target.validateEnabledServices(ctx); err != nil {
		return err
	}
	if err = target.checkReleaseIntegrity(ctx, request.ReleaseDigest); err != nil {
		return err
	}
	if err = target.checkRuntimeProjection(ctx); err != nil {
		return err
	}
	if completed, err := readAccessDeploymentReceipt(target.config.DeployRoot, request.Receipt); err != nil {
		return err
	} else if completed {
		return accessRecoveryConflict("deployment already has a success receipt")
	}
	return action(ctx, target)
}

// recoveryRuntimeFiles 只输出清单内文件的存在性和摘要，不暴露运行配置。
// 参数：无，使用已通过审批的目标清单；调用方持有发布锁。
// 返回值：排序后的文件事实，不安全路径或异常读取返回错误。
func (target *productionComposeTarget) recoveryRuntimeFiles() ([]application.SubsystemAccessRecoveryFile, error) {
	files := []application.SubsystemAccessRecoveryFile{}
	for _, item := range target.selectedRuntimeFiles() {
		fact := application.SubsystemAccessRecoveryFile{Path: item.Path}
		path := filepath.Join(target.config.DeployRoot, filepath.FromSlash(item.Path))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			files = append(files, fact)
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0027 != 0 || info.Size() > 2<<20 {
			return nil, provisioningError("recovery runtime file is unsafe")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		fact.Present, fact.SHA256 = true, hex.EncodeToString(digest[:])
		files = append(files, fact)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

type accessRecoveryPSRow struct{ ID, Service, Image, State, Health string }

// decodeRecoveryContainers 兼容Compose数组和逐行JSON，但不读取环境或其他元数据。
// 参数：raw为固定compose ps命令输出。
// 返回值：容器字段；损坏或过大的输出返回错误。
func decodeRecoveryContainers(raw []byte) ([]accessRecoveryPSRow, error) {
	if len(raw) > 1<<20 {
		return nil, provisioningError("recovery container inventory exceeds limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var rows []accessRecoveryPSRow
	if json.Unmarshal(raw, &rows) == nil {
		return rows, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var row accessRecoveryPSRow
		err := decoder.Decode(&row)
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, provisioningError("recovery container inventory is invalid")
		}
		rows = append(rows, row)
	}
}

// recoveryRuntimeContainers 只返回该受控应用当前选择服务的容器事实。
// 参数：ctx控制固定只读Compose查询，调用方持有发布锁。
// 返回值：排序后的服务存在性/容器/镜像/健康；不可观察时返回错误。
func (target *productionComposeTarget) recoveryRuntimeContainers(ctx context.Context) (
	[]application.SubsystemAccessRecoveryContainer, error) {
	raw, err := target.runComposeOutput(ctx, "ps", "-a", "--format", "json")
	if err != nil {
		return nil, provisioningError("recovery container inventory unavailable")
	}
	rows, err := decodeRecoveryContainers(raw)
	if err != nil {
		return nil, err
	}
	result := []application.SubsystemAccessRecoveryContainer{}
	for _, service := range target.selectedRuntimeServices() {
		found := false
		for _, row := range rows {
			if row.Service != service {
				continue
			}
			if row.ID == "" || row.Image == "" || row.State == "" {
				return nil, provisioningError("recovery container identity is incomplete")
			}
			found = true
			result = append(result, application.SubsystemAccessRecoveryContainer{ServiceID: service,
				Present: true, ContainerID: row.ID, Image: row.Image, State: row.State, Health: row.Health})
		}
		if !found {
			result = append(result, application.SubsystemAccessRecoveryContainer{ServiceID: service})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ServiceID != result[j].ServiceID {
			return result[i].ServiceID < result[j].ServiceID
		}
		return result[i].ContainerID < result[j].ContainerID
	})
	return result, nil
}

// recoveryEvidence 在锁内收集真实事实，或返回已持久人工终止的原证据。
// 参数：ctx为只读查询上下文，receipt为原代次。
// 返回值：不含秘密的事实与摘要；已有确定拒绝则要求正常恢复。
func (target *productionComposeTarget) recoveryEvidence(ctx context.Context,
	receipt application.SubsystemAccessDeploymentReceipt) (application.SubsystemAccessRecoveryEvidence, error) {
	result := application.SubsystemAccessRecoveryEvidence{Receipt: receipt}
	value, found, err := readAccessDeploymentOutcome(target.config.DeployRoot, receipt)
	if err != nil {
		return result, err
	}
	if found {
		if value.State != application.AccessDeploymentAbandoned {
			return result, accessRecoveryConflict("durable rejection exists; resume original operation")
		}
		result.EvidenceDigest, result.RuntimeFiles = value.EvidenceDigest, value.RuntimeFiles
		result.Containers, result.AlreadyFenced = value.Containers, true
		return result, nil
	}
	if result.RuntimeFiles, err = target.recoveryRuntimeFiles(); err != nil {
		return result, err
	}
	if result.Containers, err = target.recoveryRuntimeContainers(ctx); err != nil {
		return result, err
	}
	if len(result.RuntimeFiles) == 0 || len(result.Containers) == 0 {
		return result, provisioningError("reviewed runtime evidence is incomplete")
	}
	result.EvidenceDigest, err = accessRecoveryEvidenceDigest(receipt, result.RuntimeFiles, result.Containers)
	return result, err
}

// CheckAccessDeploymentRecovery 在受控锁和签名检查下预检人工终止的真实事实。
// 参数：ctx控制查询，request限定原目标和受信发布，不执行终止。
// 返回值：可审查事实及摘要；活动请求、漂移或不可观察时拒绝。
func (p *ProductionComposeSubsystemProvisioner) CheckAccessDeploymentRecovery(ctx context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error) {
	var result application.SubsystemAccessRecoveryEvidence
	err := p.withAccessRecoveryLock(ctx, request, func(operationCtx context.Context, target *productionComposeTarget) error {
		var err error
		result, err = target.recoveryEvidence(operationCtx, request.Receipt)
		return err
	})
	return result, err
}

// AbandonAccessDeployment 人工核对后先原子栅栏原代次，不伪造历史拒绝或成功。
// 参数：request包含原绑定、已审查摘要、操作者及人工原因；ctx控制执行。
// 返回值：Agent原样持久的人工决定；事实变化、活动部署或签名异常时拒绝。
func (p *ProductionComposeSubsystemProvisioner) AbandonAccessDeployment(ctx context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessDeploymentOutcome, error) {
	var result application.SubsystemAccessDeploymentOutcome
	request.OperatorID, request.Reason = strings.TrimSpace(request.OperatorID), strings.TrimSpace(request.Reason)
	if request.OperatorID == "" || len(request.OperatorID) > 64 ||
		!utf8.ValidString(request.Reason) || utf8.RuneCountInString(request.Reason) < 10 ||
		utf8.RuneCountInString(request.Reason) > 500 || len(request.EvidenceDigest) != 64 {
		return result, application.ErrValidation
	}
	err := p.withAccessRecoveryLock(ctx, request, func(operationCtx context.Context, target *productionComposeTarget) error {
		evidence, err := target.recoveryEvidence(operationCtx, request.Receipt)
		if err != nil {
			return err
		}
		if evidence.EvidenceDigest != request.EvidenceDigest {
			return accessRecoveryConflict("reviewed runtime facts changed")
		}
		if evidence.AlreadyFenced {
			result, _, err = readAccessDeploymentOutcome(target.config.DeployRoot, request.Receipt)
			return err
		}
		result = application.SubsystemAccessDeploymentOutcome{Receipt: request.Receipt,
			State: application.AccessDeploymentAbandoned, Code: application.AccessOperatorAbandoned,
			OperatorID: request.OperatorID, Reason: request.Reason, RecordedAt: time.Now().UTC(),
			EvidenceDigest: evidence.EvidenceDigest, RuntimeFiles: evidence.RuntimeFiles, Containers: evidence.Containers}
		if err = writeAccessDeploymentOutcome(target.config.DeployRoot, result); err != nil {
			return err
		}
		result, _, err = readAccessDeploymentOutcome(target.config.DeployRoot, request.Receipt)
		return err
	})
	return result, err
}
