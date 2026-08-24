package application

import "time"

// SystemClock 提供生产环境使用的 UTC 实际时钟。
type SystemClock struct{}

// Now 返回当前 UTC 时间。
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}
