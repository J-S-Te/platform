# File Gateway 生产运行说明

## 进程与数据边界

`file-gateway` 是独立进程，使用独立 MySQL Schema 和独立对象存储 Bucket。基础平台 API
不再注册文件上传、下载、绑定、清理或对账路由，也不连接 File Gateway 数据库；平台只负责
签发受限应用令牌和交付 `file_gateway_write` 凭据。

业务子系统只能通过各自仓库中的 `filegatewayclient` 访问网关，不能导入平台内部包。每次文件
访问都同时携带租户、应用和业务资源绑定；数据库只保存 File Gateway 的文件 ID、版本、摘要
及业务状态，不保存新的大二进制字段。

## 上传状态与幂等

上传以 `(tenant_id, application_id, request_id)` 为唯一会话，完整请求哈希覆盖租户、应用、
用户、文件名、MIME、密级和完整内容。预约 Session、创建文件和首个版本在同一 MySQL 事务
中完成：

```text
PENDING_UPLOAD -> VALIDATING -> READY
                         |----> REJECTED
             任意基础设施失败 -> FAILED
```

相同请求只有 `READY` 可以返回原文件；不同哈希以及仍在写入、失败或拒绝的 Session 均返回
冲突，由受控对账或新的业务请求号恢复，不能伪装上传成功。

## 对象存储

生产使用 AWS SDK for Go v2 S3 协议，支持 AWS S3、兼容 S3 的阿里云 OSS Endpoint 和
MinIO。凭据为空时使用 SDK 默认凭据链；静态凭据只能通过运行时环境注入，不能写入 Git。

必填配置：

- `FILE_GATEWAY_DATABASE_DSN`
- `FILE_GATEWAY_DB_PASSWORD` / `FILE_GATEWAY_DB_ROOT_PASSWORD`
- `FILE_GATEWAY_STORAGE_BACKEND=s3`
- `FILE_GATEWAY_S3_BUCKET`
- `FILE_GATEWAY_S3_REGION`
- `FILE_GATEWAY_TOKEN_ISSUER`
- `FILE_GATEWAY_TOKEN_AUDIENCE`
- `FILE_GATEWAY_TOKEN_PUBLIC_KEY_PATH`

兼容服务可配置 `FILE_GATEWAY_S3_ENDPOINT` 和 `FILE_GATEWAY_S3_USE_PATH_STYLE`。生产健康检查
使用 `/readyz`，会同时探测 MySQL 和 Bucket；`/livez` 仅表示进程存活。

Bucket 生命周期规则应清理未完成的临时对象，并根据合规要求启用版本化、服务端加密、访问
日志和保留策略。网关不会执行无界 ListObjects 扫描。

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

真实 MySQL 并发验证通过 `FILE_GATEWAY_TEST_DSN` 启用；真实 S3 兼容服务验证通过
`FILE_GATEWAY_S3_INTEGRATION_*` 启用。测试 DSN 必须指向可清空的专用数据库，集成测试
Bucket 也必须与生产 Bucket 隔离。

生产切换顺序：先发布独立数据库与网关，确认 `/readyz`；再为子系统交付网关凭据并使用双写；
完成历史文件回填与抽样校验后切换为必需模式，最后删除子系统旧 BLOB/本地文件写路径。
