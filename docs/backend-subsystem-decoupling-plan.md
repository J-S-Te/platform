# 后端子系统解耦改造方案（待评审，未实施）

> 目标：让平台后端不再硬编码具体子系统（contract_management / customer_and_opportunity /
> customer_portal / project_management），新增子系统只改 `subsystems.d/*.yaml` 清单即可，
> 无需改平台 Go 代码。
> 原则：子系统特有行为下沉到清单/配置，后端以通用方式消费。

## 一、现状：后端耦合点清单

| # | 文件 | 耦合点 | 影响 |
|---|------|--------|------|
| 1 | `internal/bootstrap/operational_modules.go:235` | `initialSubsystemAdministratorRoles()` 按 applicationCode 返回角色（customer_and_opportunity → sales_director/team_lead/technical_lead；customer_portal → 空） | 新增子系统要授予初始管理员角色必须改代码 |
| 2 | `internal/platform/applicationregistry/application/subsystem_onboarding.go:15` | `integratedCustomerApplicationCode` / `integratedPortalApplicationCode` / `integratedContractApplicationCode` + 路径/上游常量 | 集成子系统身份写死在平台 |
| 3 | `internal/platform/applicationregistry/infrastructure/production_subsystem_profiles.go:360` | `validProductionServiceBindingForApplication()` 按 applicationCode 白名单允许的服务用途绑定 | 新增子系统要声明新服务用途必须改代码 |
| 4 | `internal/platform/applicationregistry/infrastructure/local_docker_subsystem_provisioner.go:29` | `integratedXXXApplicationCode` + 多处 `switch applicationCode`（env 文件、compose、portal 集成） | 本地模式强耦合 |
| 5 | `internal/platform/externalidentity/application/service.go:23` | `PortalApplicationCode = "customer_portal"` | 外部身份逻辑硬编码门户应用 |

## 二、解耦设计

### 核心思路
把“每个子系统的行为声明”放进清单（`subsystems.d/*.yaml`），后端从清单读取，不再写 `case "xxx"`。

### 清单新增字段（向后兼容：缺省时保留现有默认行为）

```yaml
application:
  code: customer_and_opportunity
  ...
  initial_admin_roles: [sales_director, team_lead, technical_lead]   # 空=不授予内部管理员
  allowed_service_bindings: [owner_directory_read, contract_summary_read, contract_opportunity_signed_write]
  path_aliases:                                                        # legacy 路径兼容
    - { path_prefix: /customer_and_opportunity, upstream_url: http://opportunity-api:8082 }
  capabilities:
    portal: true          # 外部客户门户语义（当前 customer_portal）
    external_identity: true
```

### 后端改造（按清单驱动）

1. **初始管理员角色**：`initialSubsystemAdministratorRoles()` 改为读取清单 `application.initial_admin_roles`；
   - 本地 preset / 生产 manifest 都从同一份声明加载。
2. **服务绑定白名单**：`validProductionServiceBindingForApplication()` 改为遍历清单 `application.allowed_service_bindings`，不再按 applicationCode switch；
   - 校验器仍校验 purpose 是已知的 ServiceCredential 常量（防止拼写错误），但“哪个应用允许哪些用途”来自清单。
3. **集成编码/路径/上游**：`subsystem_onboarding.go` 与 `local_docker_subsystem_provisioner.go` 中的 `integratedXXXApplicationCode`/PathPrefix/UpstreamURL 改为从清单读取；legacy 路径用 `path_aliases` 表达；
4. **外部身份门户标志**：`externalidentity` 的 `PortalApplicationCode` 改为由清单 `capabilities.external_identity/portal` 驱动（或由外部身份服务按 manifest 判断）。

### 契约与校验

- `production_subsystem_profiles.go` 的清单校验器新增字段白名单：
  - `initial_admin_roles` 必须引用目标子系统目录中真实存在的角色（否则接入时初始授权失败应提前报错）；
  - `allowed_service_bindings` 必须是已知 ServiceCredential 用途；
  - `capabilities` 只允许布尔字段白名单。
- 本地 preset（`scripts/subsystem.sh`）与清单字段保持一致。

## 二点五、兜底措施（必须满足）

解耦的核心是“等价重构”：**任何情况下都不能改变现有子系统接入的角色/凭据行为**。兜底措施按优先级：

1. **缺省回退默认（硬性要求）**
   - 清单缺少新字段（`initial_admin_roles` / `allowed_service_bindings` / `capabilities` 等）时，
     后端**回退到当前硬编码默认值**，绝不因缺字段而改变行为；
   - 也就是说：老清单、缺字段的清单，行为和今天完全一致；
   - 只有显式填写了新字段的清单才走新逻辑，且新字段必须与现状等价。

2. **一致性告警（不阻断）**
   - 平台启动/接入时，对“硬编码默认 vs 清单声明”做一致性检查；
   - 不一致只记 WARN 日志，**不阻断接入**，便于运维发现清单配置问题。

3. **功能开关（feature flag）**
   - 每个阶段用一个配置开关（如 `SUBSYSTEM_MANIFEST_DRIVEN_ROLES=false`）控制是否启用清单驱动；
   - 默认 `false`（仍走硬编码），测试环境验证无误后再开启；
   - 出问题把开关改回 `false` 即整体回退，无需改代码。

4. **等价性测试（每阶段）**
   - 对现有 4 个子系统，单元测试断言“清单驱动结果 == 原硬编码结果”；
   - 任意不等 → 测试失败，阻止上线。

5. **快速回滚**
   - 每阶段独立 commit；出问题 revert 单个 commit；
   - 清单字段删除即回到默认（配合第 1 条）。

6. **上线顺序**
   - 先在本地/测试环境：加清单字段（等价）→ 验证接入行为不变 → 开启开关；
   - 再上生产：同样先加字段、后开开关，灰度确认。

## 三、分阶段实施（每阶段独立可上线）

| 阶段 | 内容 | 风险 | 涉及 |
|------|------|------|------|
| B1 | `initial_admin_roles` 下沉到清单（影响最小） | 低 | bootstrap、4 个 manifest、本地 preset |
| B2 | `allowed_service_bindings` 下沉到清单 | 中 | production_subsystem_profiles、接入服务、4 个 manifest |
| B3 | 集成编码/路径/上游 + legacy 别名 | 中 | subsystem_onboarding、local provisioner、manifest |
| B4 | 外部身份 portal 标志解耦 | 中 | externalidentity、manifest |

## 四、测试与回滚

- 每个阶段补“清单驱动 vs 原硬编码行为等价”的单元测试（硬性）；
- 现有子系统接入回归：contract/customer/portal/project 各跑一遍 onboard 预检；
- 兜底验证：删除新字段 / 关闭开关 → 行为与改造前完全一致（自动化测试覆盖）；
- 清单字段缺省时保持现有行为（默认值兼容），可随时回滚（删除字段即回到硬编码默认）。
- 风险点：清单校验变严格可能让“缺字段的新清单”启动失败 → 上线前跑 `go test ./internal/platform/applicationregistry/...` 的清单加载测试，且校验器对缺字段一律放行（回退默认）。

## 五、验收标准

- `subsystems.d/` 新增一个任意子系统清单（含上述字段），无需改平台代码即可：
  - 出现在应用接入能力列表；
  - 预检通过；
  - 初始管理员按 `initial_admin_roles` 授予；
  - 服务绑定按 `allowed_service_bindings` 创建；
  - 本地/生产 provisioner 按清单路径部署。
- 平台后端源码中不再出现 `case "contract_management"` / `case "customer_and_opportunity"` 等子系统编码特判。
