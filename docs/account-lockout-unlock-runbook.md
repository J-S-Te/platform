# 账号锁定解锁运维手册

> 适用：客户环境 `admins` 等平台账号因登录失败被锁定。
> 原则：**走平台正式接口或界面解锁，不直接改数据库、不关闭锁定策略。**

## 1. 锁定是怎么发生的

- 默认策略：连续 **5 次**密码失败 → 锁定 **15 分钟**（`platform-security` 登录安全策略可调，
  锁定时长上限 24 小时）；
- 锁定时会撤销该账号现有会话；解锁后必须重新登录；
- 每次失败尝试写入 `login_attempt`，平台审计同时记录 `auth.login.failed`。

先确认现状：

```bash
# 需要具备 platform:locked-account:read 的账号
curl -sS -H "Cookie: bp_session=..." \
  "https://<host>/api/v1/security/locked-accounts?page=1&page_size=50"
```

返回项含 `account_name`、`locked_until`、`failure_count`。

## 2. 两种正式解锁方式

### 2.1 界面（推荐）

用**另一个**具备 `platform-super-admin` 或 `platform-security-admin` 角色的账号登录平台：

```
平台控制台 → 登录安全 → 锁定账号 → 找到 admins → 解锁
```

### 2.2 脚本 `scripts/unlock-locked-account.sh`

前置条件：存在另一个具备上述角色的账号；脚本只调用
`POST /api/v1/security/locked-accounts/{account_id}/unlock`。

**先探测客户版本能力**（不需要凭据）：

```bash
./unlock-locked-account.sh --base-url https://<host> --probe
```

- `credential-encryption: 404（未启用）`：可用明文密码模式（v1.1.0 及更早版本属于这一类）；
- `credential-encryption: 401/403/405（已启用）`：必须用 Cookie 模式或界面解锁，
  明文密码会被 `422 CREDENTIAL_ENCRYPTION_REQUIRED` 拒绝（当前主线版本属于这一类）。

```bash
# 1) 浏览器登录有权限的账号，开发者工具复制 bp_session Cookie
# 2) 先列出锁定账号
./unlock-locked-account.sh --base-url https://<host> --cookie "bp_session=<值>" --list

# 3) 解锁并自动复核
./unlock-locked-account.sh --base-url https://<host> --cookie "bp_session=<值>" --target admins
```

可选参数：

| 参数 | 说明 |
|---|---|
| `--account-id` | 已知 account_id 时跳过按名称查找 |
| `--cookie-name` | 会话 Cookie 名（默认 `bp_session`） |
| `--yes` | 非交互执行（仍需 `--cookie` 或 `--password-stdin`） |
| `--password-stdin` | 仅适用于**未开启**凭据加密的环境；生产不建议 |

退出码：`0` 成功；`1` 参数/权限/会话错误；`3` 锁定列表中没有目标账号（可能已自动过期）。

## 3. 解锁后的动作

1. 确认账号可登录（首次登录可能要求改密）；
2. 查看失败来源：平台审计里筛 `auth.login.failed` 与 `action=auth.login.concurrent_denied`，
   确认是人为输错还是暴力尝试；若是外部攻击，结合网关限流与来源地址处置；
3. 解锁动作本身会以请求审计形式留痕（操作者、来源地址、结果），可用于事后核对。

## 4. 禁止做法

- **不要** `UPDATE iam_account SET locked_until=NULL` 或清 `iam_password_credential`：
  绕过锁定语义、留下无法解释的变更，且不受支持；
- **不要**为解锁关闭登录锁定策略或调高失败次数上限；
- **不要**用 `bootstrap-admin` 重建账号：它只在"尚无超级管理员"时工作，且不解决锁定。

## 5. 所有管理员都被锁定怎么办

1. 等锁定自然过期（默认 15 分钟，策略未调大时最快）；
2. 找另一位持有 `platform-super-admin` / `platform-security-admin` 的账号解锁；
3. 若确实没有任何可用管理员：按客户变更流程走**应急恢复**（备份恢复或厂商支持），
   并记录审批与操作证据——这属于高影响操作，不在本脚本范围内。
