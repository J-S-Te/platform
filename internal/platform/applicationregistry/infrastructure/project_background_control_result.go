package infrastructure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var errProjectBackgroundEnsurePending = errors.New("project background promotion observation pending")

// projectBackgroundControlResult 严格保留CLI失败，只识别绑定正确的提升后观察滞后。
// 参数：output及commandErr为真实命令结果，其余参数限定原动作、操作及可信身份。
// 返回值：通过校验的状态；仅ensure的特定未提升观察返回哨兵，其余失败关闭。
func projectBackgroundControlResult(output []byte, commandErr error, action, operation,
	build, tenant, environment string, drained bool) (json.RawMessage, error) {
	if commandErr == nil {
		if err := validateProjectBackgroundControl(output, build, tenant, environment, action, drained); err != nil {
			return nil, err
		}
		return json.RawMessage(output), nil
	}
	failure := fmt.Errorf("project background %s failed; operation retained", action)
	if action != "ensure" || drained || operation == "" || len(output) > 262144 {
		return nil, failure
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var raw json.RawMessage
	if decoder.Decode(&raw) != nil ||
		!bytes.Equal(bytes.TrimSpace(output[decoder.InputOffset():]), []byte("background control blocked")) {
		return nil, failure
	}
	if validateProjectBackgroundControl(raw, build, tenant, environment, "inspect", false) != nil {
		return nil, failure
	}
	var state projectBackgroundControlState
	if json.Unmarshal(raw, &state) != nil || state.Engine != "temporal" || !state.BootstrapReady ||
		state.RoutingReady || state.ExecutionReady || state.PausedReason != "POLLING_UNPROMOTED" ||
		state.ErrorCode != "CONTROL_BLOCKED" || len(state.Schedules) != 3 {
		return nil, failure
	}
	for _, schedule := range state.Schedules {
		if schedule.OperationID != "" && schedule.OperationID != operation {
			return nil, failure
		}
	}
	return raw, errProjectBackgroundEnsurePending
}
