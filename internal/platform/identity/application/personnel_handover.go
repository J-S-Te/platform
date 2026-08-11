package application

import "context"

// HandoverItem 是子系统适配器发现的一项责任；平台只保存交接控制记录，业务数据仍由各子系统负责。
type HandoverItem struct {
	System         string `json:"system"`
	ResourceType   string `json:"resource_type"`
	ResourceID     string `json:"resource_id"`
	CurrentOwnerID string `json:"current_owner_id"`
	TargetOwnerID  string `json:"target_owner_id"`
	Status         string `json:"status"`
}

type HandoverReport struct {
	Ready       bool           `json:"ready"`
	Outstanding []HandoverItem `json:"outstanding"`
}

// HandoverChecker 保持为窄适配边界：子系统发布责任快照，人员变更服务不直接访问子系统数据库。
type HandoverChecker interface {
	Check(context.Context, PersonnelChangeRequest) (HandoverReport, error)
}
