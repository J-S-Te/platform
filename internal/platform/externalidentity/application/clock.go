package application

import "time"

// SystemClock 是生产环境的 UTC 时钟。
type SystemClock struct{}

// Now 返回当前 UTC 时间，防重放时间窗与创建时间戳一律以 UTC 计算。
func (SystemClock) Now() time.Time { return time.Now().UTC() }
