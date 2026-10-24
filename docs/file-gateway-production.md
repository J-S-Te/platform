# File Gateway 生产运行说明

> 源码核验：2026-10-13；由生产运维、应用凭据管理员及业务验收人员分别执行。本文未声明真实环境已部署或验收。

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

生产默认使用部署根目录下的 `data/file-gateway`；可由 `FILE_GATEWAY_HOST_ROOT` 指定，必须以实际 Compose 装配为准，不能假定总是 `/opt/unified-identity-platform`。仅 File Gateway 容器挂载对象目录。
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

### 到期临时文件清理与精确引用释放

网关后台同时调度到期临时文件清理，默认每小时、每租户最多 100 条，并要求文件创建至少 24 小时。
数据库仍独立检查 `TEMPORARY`、保留期已到和没有 ACTIVE 绑定，不能仅凭文件年龄删除。
`LONG_TERM`、仍绑定的文件及业务历史不会被该调度清理。

```dotenv
FILE_GATEWAY_CLEANUP_ENABLED=true
FILE_GATEWAY_CLEANUP_INTERVAL=1h
FILE_GATEWAY_CLEANUP_MINIMUM_AGE=24h
FILE_GATEWAY_CLEANUP_BATCH_SIZE=100
```

可以显式设为 `false` 暂停自动物理清理，不影响正常上传、下载和上传状态对账。
批量须为 1～500，清理周期至少 1 分钟，最小年龄不得小于上传超时恢复窗口。

业务持久补偿使用 `POST /api/v1/files/{file_id}/bindings/release`，仍需 `platform:file:bind`。
请求体为 `application_id/resource_type/resource_id/binding_type`，只允许已验签租户和应用的精确引用；
重复释放成功，已释放的相同文件/资源绑定不能被迟到请求复活，其他代次不受影响。接口不删除文件字节。
此能力要求独立网关迁移 `000104_file_binding_release.sql`，应先完成备份、升级网关及健康检查，再升级使用它的业务 Worker。
升级代码不会自动扫描或删除历史上缺少业务任务记录的孤立绑定。

清理每次按租户短事务领取一条（`FOR UPDATE SKIP LOCKED`）并先置 DELETING，磁盘删除在事务外执行。失败释放与完成均按领取后版本 CAS；后台有界对账遗留 DELETING：字节已不存在只推进 DELETED，字节仍在但保留条件已不满足则释放，仍满足条件才删除，无法读取或版本冲突继续保留待处理。不能批量重置 READY 冒充文件恢复。

## 发布与验证

发布前至少执行：

```bash
go test -race ./internal/platform/filetask/...
docker compose -f compose.local.yaml config --quiet
```

真实 MySQL 并发验证通过 `FILE_GATEWAY_TEST_DSN` 启用。测试 DSN 必须指向可清空的专用数据库，
文件测试使用独立临时目录。

生产运维先校验独立网关镜像、数据库密码、宿主目录与签名公钥，再通过生产包装入口发布。
网关启动实际先连接 MySQL、执行文件迁移，再装配本地存储、文件／任务服务、上传处理器、应用 Token 校验、对账和容量监控。
由平台受控接入下发各应用凭据后，验证 `/readyz`、真实上传／下载、大小及 SHA-256、租户／资源绑定和无权限拒绝。
上述测试命令只是待执行步骤；本次未运行测试。旧手册的“双写后统一删除 BLOB”不是本次已核实的自动操作能力，不得据此清除历史数据。

## 备份恢复及失败处理

按 [生产备份恢复](../deploy/production/BACKUP_RECOVERY.md) 使用同批次数据库／对象备份，暂停所有网关写入者；临时上传被排除，quarantine 被保留。
恢复数据库导入失败时脚本可恢复旧文件目录，但数据库可能已部分写入，网关保持停止；必须人工评估数据库恢复，不宣称已完整回滚。
隔离网关演练使用 PLATFORM_IMAGE 内的 inventory 工具、临时 1 GiB MySQL 和最多 100000 条记录扫描，超过范围不能标记全量对账完成。
恢复成功还需技术健康、真实业务读写和摘要校验；只校验归档、`/livez` 或脚本退出零均不等于业务恢复。

## 上传请求解析与退休（2026-10-22源码同步）

当前独立网关迁移还包括000105。`GET /api/v1/file-upload-requests/{request_id}`读取稳定标识/状态，`POST …/{request_id}/retire`持久化请求墓碑，两者需机器scope `platform:file:upload`，租户/应用从已验签主体取得。退休回执不保证文件READY，也不删除字节；消费者拿到file_id后还需按精确资源tuple释放引用，禁止用GET404或临时超时就换key重复上传。当前实现与重试边界见[上传请求恢复](file-upload-request-recovery.md)；本轮未运行该迁移或MySQL测试。
