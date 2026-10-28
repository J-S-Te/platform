package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type runtimeObservationProvider interface {
	ObserveLicenseRuntime(context.Context, string, string) (licenseevidence.RuntimeObservation, error)
}

type composePSRow struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
	Image   string `json:"Image"`
	Status  string `json:"Status"`
}

// ObserveLicenseRuntime 通过部署 Agent 的 Unix 协议执行固定只读观察。
// 只发送应用/环境选择器；不读取、复制或返回环境变量、凭据或原始 inspect 数据。
func (p *UnixSocketSubsystemProvisioner) ObserveLicenseRuntime(ctx context.Context, application, environment string) (licenseevidence.RuntimeObservation, error) {
	if !p.enabled {
		return licenseevidence.RuntimeObservation{}, provisioningError("LICENSE_RUNTIME_OBSERVATION_UNSUPPORTED: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return licenseevidence.RuntimeObservation{}, provisioningError("LICENSE_RUNTIME_OBSERVATION_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{
		Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_OBSERVATION",
		Code: application, Environment: environment,
	}) != nil {
		return licenseevidence.RuntimeObservation{}, provisioningError("LICENSE_RUNTIME_OBSERVATION_UNAVAILABLE: send failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 256*1024)).Decode(&reply) != nil {
		return licenseevidence.RuntimeObservation{}, provisioningError("LICENSE_RUNTIME_OBSERVATION_UNAVAILABLE: response invalid")
	}
	if !reply.Success || reply.RuntimeObservation == nil {
		return licenseevidence.RuntimeObservation{}, provisioningRejectionError(reply.Message, "")
	}
	return *reply.RuntimeObservation, nil
}

// ObserveLicenseRuntime 读取审核目标当前 Compose 拓扑与容器状态。它是普通观察，
// 不要求 license-evidence.json 批准，也不创建/修改任何运行资源。
func (p *ProductionComposeSubsystemProvisioner) ObserveLicenseRuntime(ctx context.Context, application, environment string) (licenseevidence.RuntimeObservation, error) {
	original, err := p.target(application, environment)
	if err != nil {
		return licenseevidence.RuntimeObservation{}, err
	}
	// 只读观察不得改变并发部署缓存的 enabledServices。
	target := &productionComposeTarget{config: original.config, runner: original.runner}
	now := time.Now().UTC()
	unavailable := licenseevidence.RuntimeObservation{
		Status: "UNKNOWN", Available: false, Message: "运行观察不可用", CheckedAt: now,
	}
	raw, err := target.runComposeOutput(ctx, "ps", "-a", "--format", "json")
	if err != nil {
		return unavailable, nil
	}
	rows, err := decodeComposePS(raw)
	if err != nil {
		return unavailable, nil
	}
	byService := map[string]composePSRow{}
	for _, row := range rows {
		if row.Service != "" {
			byService[row.Service] = row
		}
	}
	images := map[string]string{}
	if configRaw, configErr := target.runComposeOutput(ctx, "config", "--format", "json"); configErr == nil {
		var config struct {
			Services map[string]struct {
				Image string `json:"image"`
			} `json:"services"`
		}
		if json.Unmarshal(configRaw, &config) == nil {
			for service, value := range config.Services {
				images[service] = strings.TrimSpace(value.Image)
			}
		}
	}
	compose := target.config.Profile.Manifest.Compose
	expected := append([]string(nil), compose.RuntimeServices...)
	sort.Strings(expected)
	observation := licenseevidence.RuntimeObservation{
		Status: "UNKNOWN", Available: true, CheckedAt: now,
		Message:    "只读运行观察；容器与镜像来自本机，许可配置/状态卷/公钥接线尚未核验",
		Components: make([]licenseevidence.RuntimeComponent, 0, len(expected)),
	}
	requiredTotal, requiredRunning, foundAny, unhealthy := 0, 0, false, false
	for _, service := range expected {
		_, conditional := compose.ConditionalRuntimeServices[service]
		row, exists := byService[service]
		component := licenseevidence.RuntimeComponent{
			ServiceID: service, Conditional: conditional, Exists: exists,
			Running: exists && strings.EqualFold(strings.TrimSpace(row.State), "running"),
			Health:  strings.TrimSpace(row.Health), Image: strings.TrimSpace(row.Image),
		}
		if component.Health == "" && exists {
			switch {
			case strings.Contains(strings.ToLower(row.Status), "(healthy)"):
				component.Health = "healthy"
			case strings.Contains(strings.ToLower(row.Status), "(unhealthy)"):
				component.Health = "unhealthy"
			}
		}
		if exists {
			foundAny = true
			if expectedImage := images[service]; expectedImage != "" && component.Image != "" {
				matches := expectedImage == component.Image
				component.ImageMatches = &matches
				if !matches {
					component.Issues = append(component.Issues, "image_mismatch")
				}
			}
		}
		if !conditional {
			requiredTotal++
			if component.Running {
				requiredRunning++
			}
		}
		if !exists && !conditional {
			component.Issues = append(component.Issues, "not_found")
		} else if exists && !component.Running {
			component.Issues = append(component.Issues, "not_running")
		}
		if component.Health == "unhealthy" {
			unhealthy = true
			component.Issues = append(component.Issues, "unhealthy")
		}
		observation.Components = append(observation.Components, component)
	}
	switch {
	case requiredTotal == 0:
		observation.Status = "UNKNOWN"
		observation.Message = "审核目标没有运行服务，不能判断运行状态"
	case !foundAny:
		observation.Status = "NOT_FOUND"
	case requiredRunning == requiredTotal && !unhealthy:
		observation.Status = "RUNNING"
	case requiredRunning == 0:
		observation.Status = "STOPPED"
	default:
		observation.Status = "PARTIAL"
	}
	return observation, nil
}

func decodeComposePS(raw []byte) ([]composePSRow, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var rows []composePSRow
	if json.Unmarshal(trimmed, &rows) == nil {
		return rows, nil
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	rows = make([]composePSRow, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row composePSRow
		if json.Unmarshal(line, &row) != nil {
			return nil, provisioningError("runtime observation output invalid")
		}
		rows = append(rows, row)
	}
	return rows, nil
}
