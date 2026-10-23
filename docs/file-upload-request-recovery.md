# 上传请求解析与退休

文件网关 v1 上传以 `(tenant_id, application_id, request_id)` 幂等；原请求只有完整 READY 才可成功重放，FAILED 原请求不能重新上传。超时、409 或客户端缺少 fileID 不能证明远端未创建文件，也不能直接分配新 key。

`GET /api/v1/file-upload-requests/{request_id}` 需要机器 scope `platform:file:upload`，租户和应用只取已验签主体，忽略客户端应用选择。成功响应 `data` 包含 `request_id`、稳定 `file_id` 和 `status`：WRITING、READY、FAILED、REJECTED。没有已提交预留且没有退休墓碑时返回 404；其他租户/应用的请求同样不可见。已经退休且从未预留文件的请求返回 CANCELLED 和空 fileID。接口不读取文件字节，不返回存储路径，不修改原请求状态。

`POST /api/v1/file-upload-requests/{request_id}/retire` 使用相同机器 scope，不需要请求正文。成功响应 `data` 包含 `request_id`、`file_id`、`status`、`retired: true`。不存在预留时，持久化请求墓碑后返回 CANCELLED/空 fileID；已有请求返回其稳定 fileID 和原上传会话状态，包括 WRITING。这只是身份退休回执，不声明字节可读；文件已依法清理为 DELETED/REMOVED 时仍能退休。租户、应用、文件和版本的身份链损坏则失败关闭。它不删除或取消 READY 文件字节，也不改写原上传会话终态。

迁移 `000105_file_upload_request_retirement.sql` 创建独立请求 guard。Reserve 和 Retire 都先锁同一 tenant/application/request 行：Retire 若先提交，迟到 Reserve 失败；Reserve 若先提交，Retire 返回其已提交 fileID。guard 不可读时 Reserve 失败关闭，不能走 READY 重放兜底。请求 key 的 ascii_bin 比较规则与旧上传会话保持一致。

退休消费者直接执行 Retire，在获得 fileID 后还必须调用既有 `POST /api/v1/files/{file_id}/bindings/release`，带该消费者的精确资源 tuple；该接口需要 `platform:file:bind`。不要以前置严格 Resolve 的成功作为清理条件，已清理文件的 Resolve 会正确拒绝可读结果。WRITING/FAILED/REJECTED 也可退休引用。请求墓碑封住迟到 Reserve，引用墓碑封住迟到 Bind，其他业务引用与 LONG_TERM 字节均保留。单纯 GET 404 没有退休保证，不能作为丢弃仍可能迟到请求的最终凭据。

需要重试有效业务内容时，只有在解析确认 FAILED、精确旧引用释放成功并持久化旧 fileID/请求身份后，才可独立提交下一次尝试的 key。WRITING 等待原请求收敛；REJECTED 需修正内容，避免同一不合规正文无限生成新请求。

隔离 MySQL 验证使用显式 `FILE_GATEWAY_TEST_DSN`，新增 `TestResolveUploadRequestOnMySQL` 与 `TestRetireUploadRequestFencesLateReserveAndBindingOnMySQL`。后者覆盖无预留墓碑、迟到 Reserve、WRITING 退休后迟到 READY/Bind，以及 Reserve/Retire 并发。未配置测试 DSN 时明确跳过，不连接业务数据库。
