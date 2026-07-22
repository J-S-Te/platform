<script setup>
import { computed, reactive, ref } from 'vue'
import ConsoleIcon from '@/components/ConsoleIcon.vue'
import '@/styles/security-observability.css'

const emit = defineEmits(['toast'])

const activePanel = ref('security')
const securityConfig = reactive({
  maxAttempts: 5,
  lockMinutes: 15,
  resetMinutes: 30,
  ipRateLimit: 10,
  mfaRequired: true,
  stepUpEnabled: true,
  failClosed: true,
})

const panels = [
  { key: 'security', label: '登录安全', icon: 'shield', description: '失败尝试、账户锁定、MFA 与高风险操作策略' },
  { key: 'delivery', label: '审计上报', icon: 'audit', description: 'audit_outbox、事件幂等、重试与死信处理状态' },
  { key: 'telemetry', label: '运行可观测', icon: 'dashboard', description: '运行日志、Trace、Metric 和告警入口分别管理' },
]

const lockedAccounts = reactive([
  { id: 'acc_01J0A3KQ9ZP8R2N7M4A3', username: 'audit.viewer', user: '审计专员', failures: 5, lockedUntil: '2026-07-16 12:30', source: 'OIDC', ip: '10.12.39.18' },
  { id: 'acc_01J0A3KQ9ZP8R2N7M4A9', username: 'contract.operator', user: '合同经办人', failures: 7, lockedUntil: '2026-07-16 11:45', source: '本地账号', ip: '10.12.45.9' },
])

const securityEvents = reactive([
  { id: 'risk_001', time: '2026-07-16 10:18:42', level: 'HIGH', title: '异常登录失败集中发生', detail: 'audit.viewer 在 8 分钟内发生 5 次认证失败，已自动锁定。', source: '登录安全', status: '待复核' },
  { id: 'risk_002', time: '2026-07-16 09:45:08', level: 'MEDIUM', title: '高风险导出完成二次认证', detail: '审计导出操作完成 Step-up Authentication，授权决策为允许。', source: '授权中心', status: '已处置' },
  { id: 'risk_003', time: '2026-07-15 22:16:02', level: 'HIGH', title: '授权服务降级保护触发', detail: '高风险权限校验不可用时，导出请求已按失败关闭策略拒绝。', source: '授权中心', status: '已处置' },
])

const auditDeliveries = reactive([
  { id: 'outbox_001', application: '合同管理', environment: 'production', pending: 3, retried: 1, deadLetter: 0, latest: '2026-07-16 10:22:14', status: 'HEALTHY' },
  { id: 'outbox_002', application: '项目管理', environment: 'staging', pending: 8, retried: 2, deadLetter: 1, latest: '2026-07-16 10:20:03', status: 'ATTENTION' },
  { id: 'outbox_003', application: '报销管理', environment: 'production', pending: 0, retried: 0, deadLetter: 0, latest: '2026-07-16 10:21:51', status: 'HEALTHY' },
])

const telemetryServices = reactive([
  { service: 'basic-platform', environment: 'production', tenant: 'default', log: '正常', trace: '正常', metric: '正常', errorRate: '0.08%', p95: '86 ms', updatedAt: '10:23:08' },
  { service: 'contract-management', environment: 'production', tenant: 'default', log: '正常', trace: '正常', metric: '正常', errorRate: '0.36%', p95: '148 ms', updatedAt: '10:22:56' },
  { service: 'project-management', environment: 'staging', tenant: 'default', log: '延迟', trace: '正常', metric: '正常', errorRate: '0.92%', p95: '266 ms', updatedAt: '10:20:03' },
])

const alertRules = reactive([
  { id: 'alert_001', name: '认证失败阈值', target: '登录安全', condition: '5 分钟内失败 ≥ 5 次', severity: '高', channel: '站内信', status: '启用' },
  { id: 'alert_002', name: '审计死信队列', target: 'audit_outbox', condition: 'dead_letter_count > 0', severity: '高', channel: '站内信', status: '启用' },
  { id: 'alert_003', name: '接口异常率', target: 'OTLP Metric', condition: '5xx 比率 > 1%', severity: '中', channel: '站内信', status: '启用' },
])

const currentPanel = computed(() => panels.find((item) => item.key === activePanel.value) || panels[0])
const totalDeadLetters = computed(() => auditDeliveries.reduce((sum, item) => sum + item.deadLetter, 0))
const pendingEvents = computed(() => auditDeliveries.reduce((sum, item) => sum + item.pending, 0))
const activeLockCount = computed(() => lockedAccounts.length)
const highRiskEvents = computed(() => securityEvents.filter((item) => item.level === 'HIGH' && item.status !== '已处置').length)

function emitToast(message) {
  emit('toast', message)
}

function selectPanel(key) {
  activePanel.value = key
}

function saveSecurityPolicy() {
  emitToast('登录安全策略已在前端暂存；Go 服务接入后应写入安全配置，并对变更生成审计事件。')
}

function unlockAccount(account) {
  const index = lockedAccounts.indexOf(account)
  if (index >= 0) lockedAccounts.splice(index, 1)
  securityEvents.unshift({
    id: `risk_unlock_${Date.now()}`,
    time: '2026-07-16 10:30:00',
    level: 'MEDIUM',
    title: '管理员手动解锁账号',
    detail: `已对 ${account.username} 清除登录失败记录并解除锁定。`,
    source: '登录安全',
    status: '已处置',
  })
  emitToast('已在前端解除账号锁定；正式操作需校验权限、记录操作审计并清理失败记录。')
}

function retryDelivery(item) {
  if (item.deadLetter > 0) {
    item.retried += item.deadLetter
    item.pending += item.deadLetter
    item.deadLetter = 0
    item.status = 'HEALTHY'
  } else {
    item.retried += 1
  }
  item.latest = '2026-07-16 10:30:00'
  emitToast(`${item.application} 的审计上报已在前端发起重试；平台应按 event_id 幂等接收。`)
}

function resolveRiskEvent(item) {
  item.status = '已处置'
  emitToast('风险事件已在前端标记为已处置；正式系统需保留处置人、原因和时间审计。')
}

function toggleAlert(rule) {
  rule.status = rule.status === '启用' ? '停用' : '启用'
  emitToast('告警规则状态已在前端更新；后续通过告警配置 API 和 OTLP 规则引擎同步。')
}
</script>

<template>
  <section class="security-observability" aria-label="审计安全与可观测性设置">
    <div class="so-summary-grid">
      <article class="so-summary-card blue"><span><ConsoleIcon name="shield" /></span><div><small>当前锁定账号</small><strong>{{ activeLockCount }}</strong><p>超过阈值后自动锁定</p></div></article>
      <article class="so-summary-card red"><span><ConsoleIcon name="audit" /></span><div><small>待复核高风险事件</small><strong>{{ highRiskEvents }}</strong><p>需安全管理员跟进</p></div></article>
      <article class="so-summary-card violet"><span><ConsoleIcon name="link" /></span><div><small>待上报审计事件</small><strong>{{ pendingEvents }}</strong><p>来自应用 audit_outbox</p></div></article>
      <article class="so-summary-card orange"><span><ConsoleIcon name="bell" /></span><div><small>审计死信事件</small><strong>{{ totalDeadLetters }}</strong><p>支持重试或人工处理</p></div></article>
    </div>

    <div class="so-workspace">
      <aside class="so-nav" aria-label="审计安全与可观测性功能导航">
        <button v-for="item in panels" :key="item.key" type="button" :class="{ active: activePanel === item.key }" @click="selectPanel(item.key)">
          <ConsoleIcon :name="item.icon" /><span>{{ item.label }}</span>
        </button>
      </aside>

      <section class="so-content">
        <header class="so-panel-head"><div><h2>{{ currentPanel.label }}</h2><p>{{ currentPanel.description }}</p></div></header>

        <template v-if="activePanel === 'security'">
          <div class="so-two-column">
            <article class="so-card">
              <header><div><h3>认证与锁定策略</h3><p>统一由 IAM / Security 模块执行，业务系统不保存密码和失败记录。</p></div><span class="so-status success">统一生效</span></header>
              <div class="so-policy-grid">
                <label><span>最大失败次数</span><input v-model.number="securityConfig.maxAttempts" type="number" min="1" max="20" /><small>达到阈值后锁定账号</small></label>
                <label><span>锁定时长（分钟）</span><input v-model.number="securityConfig.lockMinutes" type="number" min="1" max="1440" /><small>锁定期间拒绝登录</small></label>
                <label><span>失败记录重置（分钟）</span><input v-model.number="securityConfig.resetMinutes" type="number" min="1" max="1440" /><small>首次失败后的重置窗口</small></label>
                <label><span>登录限流（次 / 分钟 / IP）</span><input v-model.number="securityConfig.ipRateLimit" type="number" min="1" max="1000" /><small>认证接口防暴力破解</small></label>
              </div>
              <div class="so-toggle-list">
                <div><span><b>管理员强制 MFA</b><small>管理员登录需要额外验证因素。</small></span><button class="console-switch" :class="{ on: securityConfig.mfaRequired }" type="button" :aria-pressed="securityConfig.mfaRequired" @click="securityConfig.mfaRequired = !securityConfig.mfaRequired"><i /></button></div>
                <div><span><b>高风险操作二次认证</b><small>导出、权限变更等操作启用 Step-up Authentication。</small></span><button class="console-switch" :class="{ on: securityConfig.stepUpEnabled }" type="button" :aria-pressed="securityConfig.stepUpEnabled" @click="securityConfig.stepUpEnabled = !securityConfig.stepUpEnabled"><i /></button></div>
                <div><span><b>授权服务不可用时失败关闭</b><small>无法完成授权判定时拒绝高风险操作。</small></span><button class="console-switch" :class="{ on: securityConfig.failClosed }" type="button" :aria-pressed="securityConfig.failClosed" @click="securityConfig.failClosed = !securityConfig.failClosed"><i /></button></div>
              </div>
              <footer><button class="console-button primary" type="button" @click="saveSecurityPolicy"><ConsoleIcon name="save" />保存安全策略</button></footer>
            </article>

            <article class="so-card">
              <header><div><h3>风险事件</h3><p>登录、会话、MFA、授权与外部身份相关风险统一留痕。</p></div><span class="so-status warning">{{ securityEvents.length }} 条</span></header>
              <div class="so-event-list">
                <article v-for="item in securityEvents" :key="item.id"><span class="so-event-level" :class="item.level.toLowerCase()">{{ item.level === 'HIGH' ? '高' : '中' }}</span><div><strong>{{ item.title }}</strong><p>{{ item.detail }}</p><small>{{ item.time }} · {{ item.source }}</small></div><button v-if="item.status !== '已处置'" class="console-text-button" type="button" @click="resolveRiskEvent(item)">处置</button><em v-else>已处置</em></article>
              </div>
            </article>
          </div>

          <article class="so-card so-table-card">
            <header><div><h3>已锁定账号</h3><p>管理员可手动解锁；操作需要安全管理权限并应写入审计中心。</p></div><span class="so-status warning">{{ lockedAccounts.length }} 个</span></header>
            <div class="console-table-scroll"><table class="console-data-table"><thead><tr><th>账号</th><th>用户</th><th>认证来源</th><th>失败次数</th><th>来源 IP</th><th>锁定至</th><th class="console-actions-cell">操作</th></tr></thead><tbody><tr v-for="item in lockedAccounts" :key="item.id"><td class="console-mono">{{ item.username }}</td><td>{{ item.user }}</td><td>{{ item.source }}</td><td>{{ item.failures }}</td><td class="console-mono">{{ item.ip }}</td><td class="console-mono">{{ item.lockedUntil }}</td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="unlockAccount(item)">手动解锁</button></td></tr><tr v-if="!lockedAccounts.length"><td colspan="7" class="console-empty">当前没有锁定账号。</td></tr></tbody></table></div>
          </article>
        </template>

        <template v-else-if="activePanel === 'delivery'">
          <div class="so-delivery-note"><ConsoleIcon name="info" /><span>操作审计与数据变更审计分别由 <code>audit_event</code>、<code>audit_change</code> 承载；敏感字段仅传递最小化摘要，平台按 <code>event_id</code> 幂等接收。</span></div>
          <article class="so-card so-table-card">
            <header><div><h3>应用审计上报状态</h3><p>业务事务先写本地 MySQL <code>audit_outbox</code>，后台任务通过 SDK / API 上报。</p></div><span class="so-status success">事件接收正常</span></header>
            <div class="console-table-scroll"><table class="console-data-table"><thead><tr><th>应用</th><th>环境</th><th>待上报</th><th>重试次数</th><th>死信</th><th>最近上报</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead><tbody><tr v-for="item in auditDeliveries" :key="item.id"><td>{{ item.application }}</td><td><span class="so-tag">{{ item.environment }}</span></td><td>{{ item.pending }}</td><td>{{ item.retried }}</td><td><span :class="item.deadLetter ? 'so-count danger' : 'so-count'">{{ item.deadLetter }}</span></td><td class="console-mono">{{ item.latest }}</td><td><span class="so-status" :class="item.status === 'HEALTHY' ? 'success' : 'warning'">{{ item.status === 'HEALTHY' ? '正常' : '需处理' }}</span></td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="retryDelivery(item)">{{ item.deadLetter ? '重试死信' : '发起重试' }}</button></td></tr></tbody></table></div>
          </article>
          <div class="so-split-card-grid">
            <article class="so-card compact"><h3>审计事件最小字段</h3><div class="so-token-list"><span>event_id</span><span>occurred_at</span><span>tenant</span><span>application</span><span>environment</span><span>subject</span><span>resource</span><span>action</span><span>result</span><span>risk_level</span><span>request_id</span><span>trace_id</span></div></article>
            <article class="so-card compact"><h3>失败处理原则</h3><ol class="so-steps"><li>按 event_id 去重接收</li><li>短暂故障自动重试</li><li>超过阈值进入死信队列</li><li>管理员人工处理并留痕</li></ol></article>
          </div>
        </template>

        <template v-else>
          <div class="so-telemetry-note"><ConsoleIcon name="info" /><span>审计查询、运行日志、Trace / Metric 与告警分离管理；禁止使用审计表替代应用运行日志。</span></div>
          <div class="so-observability-grid"><article class="so-channel-card"><ConsoleIcon name="audit" /><div><h3>结构化运行日志</h3><p>携带应用、环境、租户、Request ID / Trace ID 等字段。</p><button class="console-text-button" type="button" @click="emitToast('运行日志查询入口待接入结构化日志存储。')">查看日志</button></div></article><article class="so-channel-card"><ConsoleIcon name="link" /><div><h3>Trace 链路</h3><p>通过 OpenTelemetry / OTLP 关联请求跨模块调用。</p><button class="console-text-button" type="button" @click="emitToast('Trace 查询入口待接入 OTLP Collector。')">查看链路</button></div></article><article class="so-channel-card"><ConsoleIcon name="dashboard" /><div><h3>Metric 指标</h3><p>聚合吞吐、时延、错误率与资源使用情况。</p><button class="console-text-button" type="button" @click="emitToast('Metric 仪表盘待接入指标存储。')">查看指标</button></div></article></div>
          <article class="so-card so-table-card"><header><div><h3>服务遥测状态</h3><p>所有服务请求统一注入 Request ID / Trace ID，并标注应用、环境和租户资源标签。</p></div><span class="so-status success">OTLP 就绪</span></header><div class="console-table-scroll"><table class="console-data-table"><thead><tr><th>服务</th><th>环境</th><th>租户</th><th>日志</th><th>Trace</th><th>Metric</th><th>错误率</th><th>P95 时延</th><th>最近上报</th></tr></thead><tbody><tr v-for="item in telemetryServices" :key="item.service"><td class="console-mono">{{ item.service }}</td><td><span class="so-tag">{{ item.environment }}</span></td><td>{{ item.tenant }}</td><td><span class="so-status" :class="item.log === '正常' ? 'success' : 'warning'">{{ item.log }}</span></td><td><span class="so-status success">{{ item.trace }}</span></td><td><span class="so-status success">{{ item.metric }}</span></td><td>{{ item.errorRate }}</td><td>{{ item.p95 }}</td><td class="console-mono">{{ item.updatedAt }}</td></tr></tbody></table></div></article>
          <article class="so-card so-table-card"><header><div><h3>告警规则</h3><p>告警由安全事件和运行遥测驱动，通过站内信渠道投递，不发送邮件或短信。</p></div></header><div class="console-table-scroll"><table class="console-data-table"><thead><tr><th>规则</th><th>监测对象</th><th>触发条件</th><th>等级</th><th>通知渠道</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead><tbody><tr v-for="item in alertRules" :key="item.id"><td>{{ item.name }}</td><td>{{ item.target }}</td><td class="console-mono">{{ item.condition }}</td><td><span class="console-badge" :class="`risk-${item.severity}`">{{ item.severity }}</span></td><td>{{ item.channel }}</td><td><span class="so-status" :class="item.status === '启用' ? 'success' : 'neutral'">{{ item.status }}</span></td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="toggleAlert(item)">{{ item.status === '启用' ? '停用' : '启用' }}</button></td></tr></tbody></table></div></article>
        </template>
      </section>
    </div>
  </section>
</template>
