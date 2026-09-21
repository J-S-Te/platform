# File Gateway 生产运行说明

## 进程与数据边界

`file-gateway` 是独立进程，使用独立 MySQL Schema 和宿主机持久化目录。基础平台 API
不再注册文件上传、下载、绑定、清理或对账路由，也不连接 File Gateway 数据库；平台只负责
签发受限应用令牌和交付 `file_gateway_write` 凭据。

业务子系统只能通过各自仓库中的 `filegatewayclient` 访问网关，不能导入平台内部包。每次文件
访问都同时携带租户、应用和业务资源绑定；数据库只保存 File Gateway 的文件 ID、版本、摘要
及业务状态，不保存新的大二进制字段。

## 上传状态与幂等

v2 上传以 `(tenant_id, application_id, idempotency_key)` 为唯一会话，预登记大小、SHA-256、
用途和业务绑定。浏览器只拿一次性票据，文件内容始终先流式写入 `temporary`，通过静态校验后
才原子移动到正式目录：

```text
CREATED -> UPLOADING -> VALIDATING -> READY
                              |----> REJECTED
                  任意基础设施失败 -> FAILED
```

相同请求只有 `READY` 可以返回原文件；不同哈希以及仍在写入、失败或拒绝的 Session 均返回
冲突，由受控对账或新的业务请求号恢复，不能伪装上传成功。

## 本地目录存储

生产使用 `/opt/basic-platform/data/file-gateway` 宿主机目录，仅 File Gateway 容器挂载。
容器内网关进程以专用 UID/GID `10001` 运行，目录和文件权限分别为 `0750`、`0640`。
业务子系统只保存 `file_id` 和摘要，不接触物理路径。正式文件按
`namespace/purpose/tenant/year/month/file/version/content` 分层；半成品和被拒绝文件分别进入
`temporary` 和 `quarantine`。不使用 OSS、S3 或 MinIO。

必填配置：

- `FILE_GATEWAY_DATABASE_DSN`
- `FILE_GATEWAY_DB_PASSWORD` / `FILE_GATEWAY_DB_ROOT_PASSWORD`
- `FILE_GATEWAY_STORAGE_BACKEND=local`
- `FILE_GATEWAY_STORAGE_ROOT=/app/data/file-gateway`
- `FILE_GATEWAY_TEMP_ROOT=/app/data/file-gateway/temporary`
- `FILE_GATEWAY_QUARANTINE_ROOT=/app/data/file-gateway/quarantine`
- `FILE_GATEWAY_CAPACITY_REJECT_PERCENT=90`
- `FILE_GATEWAY_TOKEN_ISSUER`
- `FILE_GATEWAY_TOKEN_AUDIENCE`
- `FILE_GATEWAY_TOKEN_PUBLIC_KEY_PATH`

生产健康检查使用 `/readyz`，会同时探测 MySQL 和存储目录；`/livez` 仅表示进程存活。
文件目录与 File Gateway MySQL 必须使用同一批次标识备份，恢复后执行对账。
网关按 70%、80%、90% 记录分级容量日志；达到拒绝阈值后停止新上传，但 READY 文件读取仍可用。

## Reconciliation Worker

网关进程按固定周期对每个已有租户执行有界扫描，恢复超时的 `PENDING_UPLOAD` 和
`VALIDATING` 文件。默认配置为每分钟一次、15 分钟视为超时、每租户每轮最多 100 条：

- `FILE_GATEWAY_RECONCILIATION_INTERVAL`
- `FILE_GATEWAY_RECONCILIATION_STALE_AFTER`
- `FILE_GATEWAY_RECONCILIATION_BATCH_SIZE`

人工恢复端点仍保留，但不是唯一恢复机制。

## 发布与验证

发布前至少执行：

```bash
go test -race ./internal/platform/filetask/...
docker compose -f compose.local.yaml config --quiet
```

真实 MySQL 并发验证通过 `FILE_GATEWAY_TEST_DSN` 启用。测试 DSN 必须指向可清空的专用数据库，
文件测试使用独立临时目录。

生产切换顺序：先发布独立数据库与网关，确认 `/readyz`；再为子系统交付网关凭据并使用双写；
完成历史文件回填与抽样校验后切换为必需模式，最后删除子系统旧 BLOB/本地文件写路径。
