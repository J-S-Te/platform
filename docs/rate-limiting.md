# 平台登录限流配置与部署要求（SEC-B7）

## 现状：双桶进程内限流

登录接口（POST /api/v1/auth/login）由 `middleware.LoginRateLimit("account", 30, 120, time.Minute)` 保护，两层桶语义：

| 桶 | 键 | 阈值 | 作用 |
| --- | --- | --- | --- |
| 账号桶（先判） | `account:<小写账号>` | 30 次/分钟 | 主防线：同 IP 多账号互不影响；攻击者轮换出口 IP 也无法重置目标账号额度 |
| IP 兜底桶 | `ip:<客户端IP>` | 120 次/分钟 | 单出口海量随机账号撞库有上界；阈值高于账号桶，避免 NAT/办公出口正常登录互相拖死 |

修复前为"全客户端共用一个 30/min IP 桶"：任一攻击者或流量尖峰即可触发全员登录锁定；
账号字段缺失/超长/解析失败的请求只计 IP 兜底桶（限流层仅缓冲并原样还原请求体，不触碰密码语义）。

其他限流点：`/oauth2/token` 维持 IP 单桶 60/min（FixedWindowRateLimit，机器流量无浏览器账号维度）。

## 多副本部署要求（必须）

上述桶是**进程内状态**：

- N 个 API 副本的总额度约为单副本的 N 倍，攻击者可用轮换源 IP 稀释阈值；
- 单副本故障/重启会清空窗口，短时可能出现额度抖动。

因此：**生产多副本部署不得只依赖本中间件**，必须在其一落地：

1. 入口网关/负载均衡层限流（推荐：nginx limit_req 或网关 WAF 按 IP + 账号维度限流）；或
2. 将限流状态迁移到共享存储（Redis 等）——属架构升级，需改造 middleware 接口。

单机/单副本部署（当前默认）使用本中间件即可满足审计基线。

## 反向代理配置要求（必须）

IP 兜底桶依赖 `RequestClientIP`，其结果由 `APP_TRUSTED_PROXIES` 决定：

- **必须**把真实入口网关（nginx/LB 的容器地址或宿主回环）纳入 `APP_TRUSTED_PROXIES`；
- 若网关未在可信列表内，所有请求对端地址都会退化为网关 IP，"全员共桶"问题会复现；
- **严禁**配置 `0.0.0.0/0` 或 `172.16.0.0/12` 等宽泛网段——任意容器伪造 X-Forwarded-For
  即可劫持他人桶额度（生产/本地模板已收敛为 loopback + 网关精确地址，见
  `deploy/production/.env.example` 与 `docker/.env.local.example`）。

## 相关代码

- 实现：`internal/transport/http/middleware/rate_limit.go`
- 挂载：`internal/transport/http/router.go`（authRouter POST /login）
- 测试：`internal/transport/http/middleware/rate_limit_test.go`
  （同 IP 异账号互不影响 / 同账号跨 IP 受约束 / IP 兜底上界 / 请求体还原）
