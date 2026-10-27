package application

import (
	"errors"
	"fmt"
	"strings"
)

// 统一子系统接入向导的服务端模型：动作、阶段、状态与判定规则。
// 用户不需要自行选择动作；动作由服务端依据当前事实判定（见 DecideSubsystemAccessAction）。

// SubsystemAccessAction 是服务端判定的接入动作。
type SubsystemAccessAction string

const (
	// SubsystemAccessActionNone 表示已接入且一致：只展示结果，不创建或部署。
	SubsystemAccessActionNone SubsystemAccessAction = "NONE"
	// SubsystemAccessActionOnboard 表示尚未登记目录：执行首次登记并接入。
	SubsystemAccessActionOnboard SubsystemAccessAction = "ONBOARD"
	// SubsystemAccessActionAdopt 表示已登记但无受控部署记录（含历史演示环境）：
	// 确认来源后接管为受管运行时，与全新接入刻意分开以免误把旧运行时当成已部署。
	SubsystemAccessActionAdopt SubsystemAccessAction = "ADOPT"
	// SubsystemAccessActionRetry 表示上次操作失败：从失败阶段恢复，不重复登记目录。
	SubsystemAccessActionRetry SubsystemAccessAction = "RETRY"
	// SubsystemAccessActionUpdate 表示配置漂移：展示变更摘要后受控更新。
	SubsystemAccessActionUpdate SubsystemAccessAction = "UPDATE"
)

// SubsystemAccessStage 是固定流程阶段。
type SubsystemAccessStage string

const (
	// 阶段值是持久化操作记录的组成部分，顺序由 subsystemAccessStages 固定；
	// 不得重排或复用已有值，否则历史记录的恢复阶段会指向错误步骤。
	SubsystemAccessStageCheck     SubsystemAccessStage = "CHECK"
	SubsystemAccessStageDirectory SubsystemAccessStage = "DIRECTORY"
	SubsystemAccessStageAuth      SubsystemAccessStage = "AUTH"
	SubsystemAccessStageDeploy    SubsystemAccessStage = "DEPLOY"
	SubsystemAccessStageReadiness SubsystemAccessStage = "READINESS"
	SubsystemAccessStageResult    SubsystemAccessStage = "RESULT"
)

// SubsystemAccessStatus 是操作状态。
type SubsystemAccessStatus string

const (
	// PENDING/RUNNING/WAITING 仍占用目标唯一执行槽位，终态只有 FAILED 与 SUCCEEDED；
	// WAITING 特指被审批/授权/观察期等门禁暂停，必须如实展示而不是伪装失败或成功。
	SubsystemAccessStatusPending   SubsystemAccessStatus = "PENDING"
	SubsystemAccessStatusRunning   SubsystemAccessStatus = "RUNNING"
	SubsystemAccessStatusWaiting   SubsystemAccessStatus = "WAITING"
	SubsystemAccessStatusFailed    SubsystemAccessStatus = "FAILED"
	SubsystemAccessStatusSucceeded SubsystemAccessStatus = "SUCCEEDED"
)

var subsystemAccessStages = []SubsystemAccessStage{
	SubsystemAccessStageCheck,
	SubsystemAccessStageDirectory,
	SubsystemAccessStageAuth,
	SubsystemAccessStageDeploy,
	SubsystemAccessStageReadiness,
	SubsystemAccessStageResult,
}

// ErrSubsystemAccessTargetOutOfScope 表示目标不在审核清单或交付范围内。
var ErrSubsystemAccessTargetOutOfScope = errors.New("subsystem access target is out of the approved scope")

// SubsystemAccessStageSequence 返回固定阶段顺序（副本，调用方不能改内部状态）。
func SubsystemAccessStageSequence() []SubsystemAccessStage {
	sequence := make([]SubsystemAccessStage, len(subsystemAccessStages))
	copy(sequence, subsystemAccessStages)
	return sequence
}

// NextSubsystemAccessStage 返回下一阶段；RESULT 之后没有下一阶段。
func NextSubsystemAccessStage(stage SubsystemAccessStage) (SubsystemAccessStage, bool) {
	for index, current := range subsystemAccessStages {
		if current == stage && index+1 < len(subsystemAccessStages) {
			return subsystemAccessStages[index+1], true
		}
	}
	return "", false
}

// ValidSubsystemAccessStage 判断阶段取值是否合法（用于拒绝来自页面的任意阶段）。
func ValidSubsystemAccessStage(value string) bool {
	for _, stage := range subsystemAccessStages {
		if string(stage) == strings.TrimSpace(value) {
			return true
		}
	}
	return false
}

// HoldsActiveSlot 表示该状态仍在占用目标的唯一执行槽位。
// 结束（SUCCEEDED）与失败（FAILED）后释放槽位，允许后续发起新操作。
func (status SubsystemAccessStatus) HoldsActiveSlot() bool {
	switch status {
	case SubsystemAccessStatusPending, SubsystemAccessStatusRunning, SubsystemAccessStatusWaiting:
		return true
	default:
		return false
	}
}

// Terminal 表示该状态不再推进。
func (status SubsystemAccessStatus) Terminal() bool {
	return status == SubsystemAccessStatusSucceeded || status == SubsystemAccessStatusFailed
}

// ActiveSlotKey 生成目标的唯一执行槽键；空键表示不占用槽位。
func ActiveSlotKey(tenantID, applicationCode, environmentCode string) (string, error) {
	tenantID, applicationCode, environmentCode = strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.TrimSpace(environmentCode)
	if tenantID == "" || applicationCode == "" || environmentCode == "" {
		return "", fmt.Errorf("active slot key requires tenant, application and environment: %w", ErrSubsystemAccessInvalidRequest)
	}
	return tenantID + ":" + applicationCode + ":" + environmentCode, nil
}

var (
	// ErrSubsystemAccessInvalidRequest 表示请求缺少定位目标所需的信息。
	ErrSubsystemAccessInvalidRequest = errors.New("subsystem access request is invalid")
)

// SubsystemAccessFacts 是判定动作所需的当前事实（全部来自服务端读取，不来自页面自述）。
type SubsystemAccessFacts struct {
	// InScope 表示目标在审核清单与交付范围内。
	InScope bool
	// Registered 表示应用与环境已在目录中登记。
	Registered bool
	// ControlledDeployment 表示存在受控部署记录（subsystem_deployment_state）。
	ControlledDeployment bool
	// LegacyDemo 表示该环境是历史演示环境（需展示来源，按受控接管处理）。
	LegacyDemo bool
	// LastOperationFailed 表示上一次接入操作失败。
	LastOperationFailed bool
	// ConfigurationDrift 表示已接入但当前配置与期望不一致（含认证提供方不一致）。
	ConfigurationDrift bool
	// ApplicationID / EnvironmentID 在已登记时提供（用于操作记录与审计）。
	ApplicationID, EnvironmentID string
	// AuthenticationProvider is the persisted environment binding; ordinary updates preserve it.
	AuthenticationProvider string
	// ManifestChecksum 是服务端清单中该目标的规范化摘要（提交时重新核对，防止清单漂移）。
	ManifestChecksum string
	// ResumeStage 是应当从哪个阶段开始或恢复（空表示从 CHECK 开始）。
	ResumeStage SubsystemAccessStage
	// ChangeSummary 是更新场景的变更摘要（服务端生成，页面只展示）。
	ChangeSummary []string
	// ApprovalReady / LicenseReady / ImagesReady 为假时进入"等待处理"。
	ApprovalReady, LicenseReady, ImagesReady bool
	// AuthObservationPending 表示认证观察期未结束（等待，不得绕过）。
	AuthObservationPending bool
	// Unverified 记录"无法确认"的事实（例如旧版 Agent 未上报镜像状态）。
	// 未验证既不能当成"就绪"，也不能当成"缺失"——它必须被如实展示。
	Unverified []string
}

// SubsystemAccessDecision 是判定结果。
type SubsystemAccessDecision struct {
	Action SubsystemAccessAction
	// Reason 是面向用户的简短原因（用于审计与界面提示）。
	Reason string
	// DisplayOnly 为真时表示只展示当前结果，不创建、不部署。
	DisplayOnly bool
}

// DecideSubsystemAccessAction 依据事实判定动作，顺序即优先级：
// 不在范围 → 未登记 → 无受控部署（含历史演示）→ 上次失败 → 配置漂移 → 已一致。
func DecideSubsystemAccessAction(facts SubsystemAccessFacts) SubsystemAccessDecision {
	if !facts.InScope {
		return SubsystemAccessDecision{Action: SubsystemAccessActionNone, DisplayOnly: true,
			Reason: "目标不在审核清单或交付范围内，不生成部署操作"}
	}
	if !facts.Registered {
		return SubsystemAccessDecision{Action: SubsystemAccessActionOnboard, Reason: "应用与环境尚未登记，执行首次登记并接入"}
	}
	if !facts.ControlledDeployment {
		if facts.LegacyDemo {
			return SubsystemAccessDecision{Action: SubsystemAccessActionAdopt, Reason: "历史演示环境：确认为历史来源后走受控接管"}
		}
		return SubsystemAccessDecision{Action: SubsystemAccessActionAdopt, Reason: "已登记但没有受控部署记录，接管运行时"}
	}
	if facts.LastOperationFailed {
		return SubsystemAccessDecision{Action: SubsystemAccessActionRetry, Reason: "上次执行失败，检查后从失败阶段恢复"}
	}
	if facts.ConfigurationDrift {
		return SubsystemAccessDecision{Action: SubsystemAccessActionUpdate, Reason: "已接入但配置与期望不一致，展示变更摘要后受控更新"}
	}
	// 未取得一致性事实不能当作已一致，否则缺少批准时漂移查询被跳过，
	// 向导会误报无需操作，无法展示后续受控检查的真实缺项。
	if subsystemAccessFactUnverified(facts, "configuration_drift") {
		return SubsystemAccessDecision{Action: SubsystemAccessActionUpdate, Reason: "运行配置一致性尚未核验，补齐条件后执行受控检查"}
	}
	return SubsystemAccessDecision{Action: SubsystemAccessActionNone, DisplayOnly: true, Reason: "已接入且配置一致，仅展示当前结果"}
}
