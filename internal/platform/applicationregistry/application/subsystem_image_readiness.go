package application

// SubsystemImageReadiness 描述"批准清单中的某个服务镜像在本机是否存在"。
// 只包含非敏感信息（服务标识与镜像摘要），由部署 Agent 读取本机状态后上报。
type SubsystemImageReadiness struct {
	ServiceID   string `json:"service_id"`
	ImageDigest string `json:"image_digest"`
	Present     bool   `json:"present"`
}
