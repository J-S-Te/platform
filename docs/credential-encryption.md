# 账号口令应用层加密（uip-credential/v1）

## 保护边界

本机制保护登录、改密、创建账号、员工入职、初始化/重置密码和批量导入中的凭据请求，以及返回临时密码的 JSON 响应。它降低普通 HTTP 被动抓包和请求体日志直接泄漏口令的风险。

**它不是 HTTPS 的替代品**：HTTP 下页面、脚本和公钥仍可能被主动篡改，整个会话也没有得到 HTTPS 等价保护。不能据此宣称系统已达到 HTTPS 安全等级。

## 协议

- 协议标识：`uip-credential/v1`
- RSA-2048，RSA-OAEP，SHA-256，MGF1-SHA-256，label `uip-credential-v1`
- RSA 只封装 64 字节：`K_req(32) || K_resp(32)`
- 请求/响应正文：AES-256-GCM，12 字节 IV，16 字节 tag
- Base64URL 无填充
- AAD：`protocol \n direction \n kid \n challenge_id \n purpose \n resource_id \n content_type`
- challenge 默认 60 秒，数据库存储 SHA-256 摘要；消费为条件更新，业务执行前提交。

## 配置

```text
CREDENTIAL_ENCRYPTION_MODE=disabled|compat|required
CREDENTIAL_ENCRYPTION_KEYRING_FILE=/app/data/keys/credential-keyring.json
CREDENTIAL_ENCRYPTION_CHALLENGE_TTL_SECONDS=60
CREDENTIAL_ENCRYPTION_CLEANUP_BATCH_SIZE=1000
CREDENTIAL_ENCRYPTION_RESPONSE_MAX_BYTES=8388608
```

`compat`/`required` 下启动时必须加载有效密钥环；不得静默降级明文。前端构建变量：

```text
VITE_CREDENTIAL_ENCRYPTION_MODE=disabled|required
```

前端生产构建默认 `required` 并启用加密 API 调用；`disabled` 仅用于尚未完成密钥分发的过渡构建。上线时必须与后端 `required` 同步。

## 密钥环

使用 `cmd/credential-keygen`：

```bash
go run ./cmd/credential-keygen -dir ./data/keys -name credential-keyring
```

生成私钥（PKCS#8，0600）、公钥（SPKI）和元数据。私钥不得进入 Git、镜像或前端；生产通过只读挂载提供。元数据：

```json
{
  "active_kid": "<SPKI SHA-256 hex>",
  "keys": [
    {
      "kid": "<same>",
      "private_key_file": "credential-keyring-<kid>.private.pem",
      "public_key_file": "credential-keyring-<kid>.public.pem"
    }
  ]
}
```

轮换：先把新私钥加入所有实例，确认可识别；再切换 active kid；旧私钥至少保留 5 分钟并确认无旧 challenge 后移除。

## 数据库

迁移 `000128_create_credential_encryption_challenge.sql` 创建 `credential_encryption_challenge`。服务启动时加载密钥环并启用每分钟有界清理（每批最多 1000 条）。

## 当前接入

- 后端协议模块、challenge、密钥环、HTTP 封套、登录/改密/账号创建/员工创建/批量导入/初始化/重置路由适配已接入。
- 前端 `src/modules/shared/credential/` 提供协议、challenge、RSA/AES 和 JSON/multipart 请求封装。
- 前端登录和改密 API 已按 `VITE_CREDENTIAL_ENCRYPTION_MODE` 切换；IAM 创建账号、员工、批量导入、初始化/重置已接入。

## 上线门禁

尚未完成：真实 MySQL 迁移验证、多实例 challenge 竞争测试、浏览器 HTTP 非 localhost 验收、完整全业务回归、`required` 模式部署证据。上述完成前不能把本机制标记为生产可用。

## 容器发布初始化（2026-10-26）

生产 `.env.example` 与前端生产构建统一为 `required`。`platform-key-init` 的显式初始化阶段在开启 `compat/required` 时创建缺失的 RSA-2048 密钥环；API 常规启动与 `config-check` 不生成或轮换该密钥。镜像包含 `credential-keygen`，运行时沿用密钥挂载目录的属主并使用0600私钥/元数据权限，便于既有部署账号执行完整备份。

已有环境升级时先按发布门禁完成完整备份，确认没有待恢复的密钥环或旧RSA文件，然后同时设置 `CREDENTIAL_ENCRYPTION_MODE=required` 和 `CREDENTIAL_ENCRYPTION_KEYRING_FILE=/app/data/keys/credential-keyring.json`，执行正常平台发布（含显式key-init、迁移及config-check）。已有有效密钥环保持不变；存在孤立PEM或非普通文件时初始化拒绝继续，不自动换钥。发布后验证公开challenge接口，再由用户完成真实登录验收；HTTP模式的保护边界仍以本文开头说明为准。
