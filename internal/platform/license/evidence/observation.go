package evidence

import "time"

// RuntimeComponent 是单个运行服务的脱敏观察事实。不得携带环境变量值、机器密钥或原始 Docker inspect 数据。
type RuntimeComponent struct {
	ServiceID    string   `json:"service_id"`
	Conditional  bool     `json:"conditional"`
	Exists       bool     `json:"exists"`
	Running      bool     `json:"running"`
	Health       string   `json:"health,omitempty"`
	Image        string   `json:"image,omitempty"`
	ImageMatches *bool    `json:"image_matches,omitempty"`
	Issues       []string `json:"issues,omitempty"`
}

// RuntimeObservation 是普通运行观察结果；它不建立登记、不签发凭据、不改变编排。
// Available=false 表示观察不可用，页面必须显示 UNKNOWN，不得解释为已停止。
type RuntimeObservation struct {
	Status     string             `json:"status"`
	Available  bool               `json:"available"`
	Message    string             `json:"message,omitempty"`
	CheckedAt  time.Time          `json:"checked_at"`
	Components []RuntimeComponent `json:"components,omitempty"`
}
