package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// FixedWindowRateLimit 用进程内固定窗口保护敏感端点。互斥锁同时保护计数和清理；它适用于
// 单体或单副本防护，不提供跨实例总额度，扩容后必须改用共享限流存储或网关策略。
func FixedWindowRateLimit(limit int, window time.Duration) gin.HandlerFunc {
	type bucket struct {
		startedAt time.Time
		count     int
	}
	var mutex sync.Mutex
	buckets := make(map[string]bucket)
	lastCleanup := time.Now()
	return func(context *gin.Context) {
		now := time.Now()
		key := RequestClientIP(context.Request)
		if key == "" {
			key = "unknown"
		}
		mutex.Lock()
		// 清理与请求计数共用临界区，避免删除刚被另一请求刷新过的桶；清理频率最多每窗口一次。
		if now.Sub(lastCleanup) >= window {
			for candidate, current := range buckets {
				if now.Sub(current.startedAt) >= window {
					delete(buckets, candidate)
				}
			}
			lastCleanup = now
		}
		current := buckets[key]
		if current.startedAt.IsZero() || now.Sub(current.startedAt) >= window {
			current = bucket{startedAt: now}
		}
		current.count++
		buckets[key] = current
		allowed := current.count <= limit
		mutex.Unlock()
		if !allowed {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusTooManyRequests, httperror.New("PLATFORM_RATE_LIMITED", "请求过于频繁，请稍后再试", nil))
			return
		}
		context.Next()
	}
}

// loginRateLimitBodyBytes 是登录 JSON 载荷上限；超限视为无法识别账号，只计 IP 兜底桶。
const loginRateLimitBodyBytes = 8 << 10

// LoginRateLimit 登录专用双桶固定窗口限流（安全审查 SEC-B7 / M-2）：
//   - 账号维度先判（accountLimit，主防线，阈值与原登录30/min一致）：账号桶只统计
//     **失败尝试**（响应 401），请求进入时仅做预检查，不因本次请求预增额度。同 IP
//     不同账号互不影响；攻击者轮换出口 IP 也始终受同一账号桶约束。成功登录（2xx/3xx）
//     会清零该账号的失败计数，因此攻击者无法靠“正常登录请求”把任意账号锁死（M-2）；
//   - IP 维度兜底（ipLimit）：所有请求（无论成败）都计数，单出口海量随机账号撞库在
//     IP 桶上有界；兜底阈值刻意高于账号阈值，避免 NAT/办公出口下正常多账号登录互相
//     拖死——原实现全客户端共用一个 30/min IP 桶，任一攻击者即可触发全员锁定。
//
// 账号只从请求体指定字段安全提取：限流层仅缓冲并原样还原 body，不触碰密码语义；
// 字段缺失/超长/解析失败时退化为只计 IP 桶。
//
// 账号键命名空间：登录契约（loginRequest）没有租户/应用字段，无法可靠得到租户，
// 因此账号键保持全局 "account:<小写账号>"。失败-only 计数已消除“正常登录被攻击者
// 锁死”；残余风险是攻击者用失败尝试打满窗口，让目标账号在窗口内 429——这正是防
// 撞库的预期行为。
//
// 并发语义：预检查与响应后的失败计数不在同一临界区，并发请求允许少量越过阈值；
// 计数与清理仍共用同一 mutex，无数据竞争。
//
// 部署要求（与 platform/docs/rate-limiting.md 一致，缺一不可）：
//  1. 多副本：本桶为进程内状态，N 个副本总额度≈N 倍——多副本部署必须改用共享限流
//     存储（Redis 等）或入口网关限流策略，本中间件只作单副本兜底；
//  2. 反代：IP 桶依赖 RequestClientIP（受 APP_TRUSTED_PROXIES 约束）——必须把真实
//     入口网关纳入可信代理，否则所有请求对端都落在网关 IP，退化为“全员共桶”。
func LoginRateLimit(accountField string, accountLimit, ipLimit int, window time.Duration) gin.HandlerFunc {
	type bucket struct {
		startedAt time.Time
		count     int
	}
	var mutex sync.Mutex
	buckets := make(map[string]bucket)
	lastCleanup := time.Now()
	// cleanupLocked 必须在持有 mutex 的临界区内调用：计数与清理共用临界区，
	// 避免删除刚被另一请求刷新过的桶；清理频率最多每窗口一次。
	cleanupLocked := func(now time.Time) {
		if now.Sub(lastCleanup) < window {
			return
		}
		for candidate, current := range buckets {
			if now.Sub(current.startedAt) >= window {
				delete(buckets, candidate)
			}
		}
		lastCleanup = now
	}
	// accountFailuresLocked 只读当前窗口内的账号失败计数，不修改任何状态：窗口过期或
	// 桶不存在都视为 0，保证请求入口不会预增账号额度。
	accountFailuresLocked := func(now time.Time, key string) int {
		current, ok := buckets[key]
		if !ok || current.startedAt.IsZero() || now.Sub(current.startedAt) >= window {
			return 0
		}
		return current.count
	}
	// recordAccountFailureLocked 只在认证失败（401）后 +1；窗口起点由本窗口第一次失败
	// 建立，窗口过期后重新从 0 开始。
	recordAccountFailureLocked := func(now time.Time, key string) {
		current := buckets[key]
		if current.startedAt.IsZero() || now.Sub(current.startedAt) >= window {
			current = bucket{startedAt: now}
		}
		current.count++
		buckets[key] = current
	}
	// allowIPLocked 保持 IP 兜底桶“所有请求预计数”的语义：随机账号撞库只能靠它兜底。
	allowIPLocked := func(now time.Time, key string, limit int) bool {
		current := buckets[key]
		if current.startedAt.IsZero() || now.Sub(current.startedAt) >= window {
			current = bucket{startedAt: now}
		}
		current.count++
		buckets[key] = current
		return current.count <= limit
	}
	reject := func(context *gin.Context) {
		context.Abort()
		httpresponse.WriteError(context.Writer, context.Request, http.StatusTooManyRequests, httperror.New("PLATFORM_RATE_LIMITED", "请求过于频繁，请稍后再试", nil))
	}
	return func(context *gin.Context) {
		now := time.Now()
		accountKey := ""
		if account := loginRateLimitAccount(context.Request, accountField); account != "" {
			accountKey = "account:" + account
		}
		ipKey := RequestClientIP(context.Request)
		if ipKey == "" {
			ipKey = "unknown"
		}
		mutex.Lock()
		cleanupLocked(now)
		// 账号桶入口只做预检查：已打满窗口就不放行，且本次请求不预增失败计数。
		allowed := accountKey == "" || accountFailuresLocked(now, accountKey) < accountLimit
		if allowed {
			allowed = allowIPLocked(now, "ip:"+ipKey, ipLimit)
		}
		mutex.Unlock()
		if !allowed {
			reject(context)
			return
		}
		context.Next()
		// 响应后计账：仅 401（认证失败）消耗账号额度；2xx/3xx 成功登录清零历史失败，
		// 避免故障期积累的失败拖累正常登录；429 等其它状态（含本中间件自身的拒绝）不计数。
		if accountKey == "" {
			return
		}
		switch status := context.Writer.Status(); {
		case status == http.StatusUnauthorized:
			mutex.Lock()
			recordAccountFailureLocked(time.Now(), accountKey)
			mutex.Unlock()
		case status >= http.StatusOK && status < http.StatusBadRequest:
			mutex.Lock()
			delete(buckets, accountKey)
			mutex.Unlock()
		}
	}
}

// loginRateLimitAccount 缓冲并还原请求体，从 JSON 提取限流账号键。
// 只读取指定字段、统一小写并限长，任何异常都退化为空（只计 IP 桶），绝不放行失败。
func loginRateLimitAccount(request *http.Request, field string) string {
	if request == nil || request.Body == nil || field == "" {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, loginRateLimitBodyBytes+1))
	// 无论提取成败都必须把 body 原样交给后续 handler（密码校验仍由 handler 解析）。
	request.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > loginRateLimitBodyBytes {
		return ""
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	raw, ok := payload[field]
	if !ok {
		return ""
	}
	var account string
	if err := json.Unmarshal(raw, &account); err != nil {
		return ""
	}
	account = strings.ToLower(strings.TrimSpace(account))
	if account == "" || len(account) > 128 {
		return ""
	}
	return account
}
