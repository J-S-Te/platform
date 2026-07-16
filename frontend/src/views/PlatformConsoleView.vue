<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import ConsoleIcon from '@/components/ConsoleIcon.vue'
import IamSettingsModule from '@/components/IamSettingsModule.vue'
import SecurityObservabilityModule from '@/components/SecurityObservabilityModule.vue'
import '@/styles/console.css'

const initialSettings = {
  organizationName: '基础能力平台',
  organizationAlias: '基础平台',
  timezone: '(GMT+08:00) 北京 / 上海',
  qualification: '统一社会信用代码：待后端接入 · 等保备案信息：待配置',
  inboxEnabled: true,
  emailEnabled: true,
  reminderFrequency: '每日',
}

const settings = reactive({ ...initialSettings })
const currentView = ref(resolveView(window.location.pathname))
const activeSettingsTab = ref('iam')
const mobileMenuOpen = ref(false)
const toastMessage = ref('')
const auditKeyword = ref('')
const auditType = ref('')
const auditRisk = ref('')
const auditApplication = ref('')
const auditEnvironment = ref('')
const auditResult = ref('')
const auditTimeRange = ref('7d')
const auditPage = ref(1)
const auditPageSize = 5
const selectedAuditIds = ref([])
const auditDetail = ref(null)
let toastTimer = null

const settingsTabs = [
  { key: 'base', label: '基础设置' },
  { key: 'iam', label: '用户与角色' },
  { key: 'notify', label: '通知设置' },
  { key: 'security', label: '安全与可观测' },
  { key: 'dict', label: '字典管理' },
]

const auditRecords = reactive([
  { id: 'evt_01J0A3KQ9ZP8R2N7AE01', time: '2026-07-16 10:18:42', operator: '审计专员', type: '登录', application: '基础能力平台', environment: 'production', object: '统一认证服务', resource: 'account', action: 'login', method: 'POST', path: '/api/v1/auth/login', ip: '10.12.39.18', statusCode: 423, result: '拒绝', risk: '高', userAgent: 'Chrome 140 / macOS', detail: '登录失败达到锁定阈值，账号已锁定 15 分钟。', changeSummary: 'login_failure_records +1；account.locked_until 更新。' },
  { id: 'evt_01J0A3KQ9ZP8R2N7AE02', time: '2026-07-16 09:45:08', operator: '平台管理员', type: '导出', application: '基础能力平台', environment: 'production', object: '审计日志', resource: 'audit', action: 'export', method: 'GET', path: '/api/v1/audit/events/export', ip: '10.12.34.21', statusCode: 200, result: '成功', risk: '高', userAgent: 'Chrome 140 / macOS', detail: '通过二次认证后导出最近 30 天审计事件。', changeSummary: '导出任务创建；文件访问记录已留痕。' },
  { id: 'evt_01J0A3KQ9ZP8R2N7AE03', time: '2026-07-16 09:24:10', operator: '王敏', type: '修改', application: '基础能力平台', environment: 'production', object: '安全策略：会话时效', resource: 'security_policy', action: 'update', method: 'PUT', path: '/api/v1/security/policies/session', ip: '10.12.35.8', statusCode: 200, result: '成功', risk: '中', userAgent: 'Chrome 140 / macOS', detail: '将会话超时时间由 60 分钟调整为 30 分钟。', changeSummary: 'session_timeout: 60m → 30m。' },
  { id: 'evt_01J0A3KQ9ZP8R2N7AE04', time: '2026-07-15 22:16:02', operator: '系统', type: '状态变更', application: '基础能力平台', environment: 'production', object: '高风险授权策略', resource: 'authorization', action: 'deny', method: 'POST', path: '/internal/authorization/decision', ip: '127.0.0.1', statusCode: 503, result: '拒绝', risk: '高', userAgent: 'authorization-service', detail: '授权服务不可用，高风险导出请求按失败关闭策略拒绝。', changeSummary: '无业务数据变更；风险事件已创建。' },
  { id: 'evt_01J0A3KQ9ZP8R2N7AE05', time: '2026-07-15 16:44:30', operator: '李明', type: '新增', application: '合同管理', environment: 'production', object: '合同：HT-2026-0018', resource: 'contract', action: 'create', method: 'POST', path: '/api/v1/contracts', ip: '10.12.36.12', statusCode: 201, result: '成功', risk: '中', userAgent: 'Chrome 140 / macOS', detail: '创建合同草稿，审计事件已写入本地 outbox 并上报。', changeSummary: 'contract.status: null → DRAFT；金额字段已脱敏。' },
  { id: 'evt_01J0A3KQ9ZP8R2N7AE06', time: '2026-07-14 18:40:02', operator: '李明', type: '新增', application: '项目管理', environment: 'staging', object: '应用接入：项目管理', resource: 'application', action: 'register', method: 'POST', path: '/api/v1/integrations/applications', ip: '10.12.36.12', statusCode: 201, result: '成功', risk: '低', userAgent: 'Chrome 140 / macOS', detail: '登记应用审计 SDK 接入信息及 OTLP 资源标签。', changeSummary: 'application.code: project-management。' },
  { id: 'evt_01J0A3KQ9ZP8R2N7AE07', time: '2026-07-14 11:02:55', operator: '系统', type: '状态变更', application: '基础能力平台', environment: 'production', object: '登录失败锁定策略', resource: 'security_policy', action: 'apply', method: 'POST', path: '/internal/security/policies/reload', ip: '127.0.0.1', statusCode: 200, result: '成功', risk: '低', userAgent: 'security-service', detail: '按最新配置加载登录失败锁定策略。', changeSummary: '最大失败次数 5；锁定时长 15 分钟。' },
])

const filteredAuditRecords = computed(() => {
  const keyword = auditKeyword.value.trim().toLowerCase()

  return auditRecords.filter((record) => {
    const matchesKeyword = !keyword || [record.operator, record.object, record.application, record.path, record.ip, record.detail]
      .join(' ')
      .toLowerCase()
      .includes(keyword)
    const matchesType = !auditType.value || record.type === auditType.value
    const matchesRisk = !auditRisk.value || record.risk === auditRisk.value
    const matchesApplication = !auditApplication.value || record.application === auditApplication.value
    const matchesEnvironment = !auditEnvironment.value || record.environment === auditEnvironment.value
    const matchesResult = !auditResult.value || record.result === auditResult.value
    return matchesKeyword && matchesType && matchesRisk && matchesApplication && matchesEnvironment && matchesResult
  })
})

const auditTotalPages = computed(() => Math.max(1, Math.ceil(filteredAuditRecords.value.length / auditPageSize)))
const pagedAuditRecords = computed(() => {
  const startIndex = (auditPage.value - 1) * auditPageSize
  return filteredAuditRecords.value.slice(startIndex, startIndex + auditPageSize)
})
const allPageAuditSelected = computed(() => pagedAuditRecords.value.length > 0 && pagedAuditRecords.value.every((record) => selectedAuditIds.value.includes(record.id)))

const viewMeta = computed(() => {
  if (currentView.value === 'audit') {
    return { title: '审计日志', crumb: '审计日志', description: 'AUD-001 · 审计事件、数据变更摘要与跨链路追溯' }
  }
  if (activeSettingsTab.value === 'iam') {
    return { title: '系统设置', crumb: '系统设置', description: 'SYS-002 ~ SYS-004 · 身份、组织、角色与权限集中配置' }
  }
  if (activeSettingsTab.value === 'security') {
    return { title: '系统设置', crumb: '系统设置', description: 'AUD-002 · 登录安全、审计上报与运行可观测性集中配置' }
  }
  return { title: '系统设置', crumb: '系统设置', description: 'SYS-001 · 平台级参数、通知与安全策略集中配置' }
})

function resolveView(pathname) {
  return pathname.toLowerCase().startsWith('/audit') ? 'audit' : 'settings'
}

function navigate(view) {
  const targetPath = view === 'audit' ? '/audit' : '/'
  if (window.location.pathname !== targetPath) {
    window.history.pushState({}, '', targetPath)
  }
  currentView.value = view
  mobileMenuOpen.value = false
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

function showToast(message) {
  toastMessage.value = message
  if (toastTimer) {
    window.clearTimeout(toastTimer)
  }
  toastTimer = window.setTimeout(() => {
    toastMessage.value = ''
  }, 2600)
}

function saveSettings() {
  showToast('设置已在前端暂存；待 Go 接口与 MySQL 模型接入后将支持持久化保存。')
}

function resetSettings() {
  Object.assign(settings, initialSettings)
  showToast('已恢复为当前前端默认配置。')
}

function resetAuditFilters() {
  auditKeyword.value = ''
  auditType.value = ''
  auditRisk.value = ''
  auditApplication.value = ''
  auditEnvironment.value = ''
  auditResult.value = ''
  auditTimeRange.value = '7d'
  auditPage.value = 1
  selectedAuditIds.value = []
}

function toggleAllAuditRecords() {
  const ids = pagedAuditRecords.value.map((record) => record.id)
  if (allPageAuditSelected.value) {
    selectedAuditIds.value = selectedAuditIds.value.filter((id) => !ids.includes(id))
  } else {
    selectedAuditIds.value = [...new Set([...selectedAuditIds.value, ...ids])]
  }
}

function changeAuditPage(nextPage) {
  auditPage.value = Math.min(Math.max(nextPage, 1), auditTotalPages.value)
  selectedAuditIds.value = []
}

function openAuditDetail(record) {
  auditDetail.value = record
}

function closeAuditDetail() {
  auditDetail.value = null
}

function deleteAuditRecords(ids) {
  if (!ids.length) {
    showToast('请先选择需要删除的审计示例记录。')
    return
  }
  const idSet = new Set(ids)
  for (let index = auditRecords.length - 1; index >= 0; index -= 1) {
    if (idSet.has(auditRecords[index].id)) auditRecords.splice(index, 1)
  }
  selectedAuditIds.value = []
  if (auditPage.value > auditTotalPages.value) auditPage.value = auditTotalPages.value
  showToast(`已从当前前端示例中删除 ${ids.length} 条审计记录；正式环境应采用受控清理与保留策略，并另行留痕。`)
}

function exportAuditRecords() {
  const headers = ['事件 ID', '发生时间', '操作人', '操作类型', '应用', '环境', '资源对象', 'HTTP 方法', '请求路径', '客户端 IP', '状态码', '结果', '风险等级', '变更摘要']
  const rows = filteredAuditRecords.value.map((record) => [
    record.id,
    record.time,
    record.operator,
    record.type,
    record.application,
    record.environment,
    record.object,
    record.method,
    record.path,
    record.ip,
    record.statusCode,
    record.result,
    record.risk,
    record.changeSummary,
  ])
  const csv = [headers, ...rows]
    .map((row) => row.map((cell) => `"${String(cell).replaceAll('"', '""')}"`).join(','))
    .join('\n')
  const blob = new Blob([`\ufeff${csv}`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `audit-events-${auditTimeRange.value}.csv`
  anchor.click()
  URL.revokeObjectURL(url)
  showToast(`已导出 ${filteredAuditRecords.value.length} 条当前筛选结果。`)
}

function logout() {
  window.location.assign('/login.html')
}

function handlePopState() {
  currentView.value = resolveView(window.location.pathname)
}

watch([auditKeyword, auditType, auditRisk, auditApplication, auditEnvironment, auditResult, auditTimeRange], () => {
  auditPage.value = 1
  selectedAuditIds.value = []
})

watch([currentView, activeSettingsTab], () => {
  document.title = `${viewMeta.value.title} · 基础能力平台`
})

onMounted(() => {
  document.title = `${viewMeta.value.title} · 基础能力平台`
  window.addEventListener('popstate', handlePopState)
})

onBeforeUnmount(() => {
  window.removeEventListener('popstate', handlePopState)
  if (toastTimer) {
    window.clearTimeout(toastTimer)
  }
})
</script>

<template>
  <div class="console-page">
    <aside class="console-sidebar" :class="{ open: mobileMenuOpen }">
      <div class="console-brand">
        <span class="console-brand-mark"><ConsoleIcon name="logo" /></span>
        <span class="console-brand-copy">
          <strong>基础能力平台</strong>
          <small>Basic Platform</small>
        </span>
        <button class="console-close-menu" type="button" aria-label="关闭导航菜单" @click="mobileMenuOpen = false">
          <ConsoleIcon name="close" />
        </button>
      </div>

      <nav class="console-nav" aria-label="平台导航">
        <p class="console-nav-label">系统管理</p>
        <button class="console-nav-item" :class="{ active: currentView === 'settings' }" type="button" @click="navigate('settings')">
          <ConsoleIcon name="settings" />
          <span>系统设置</span>
        </button>
        <button class="console-nav-item" :class="{ active: currentView === 'audit' }" type="button" @click="navigate('audit')">
          <ConsoleIcon name="audit" />
          <span>审计日志</span>
          <span class="console-nav-note">只读</span>
        </button>
      </nav>

      <div class="console-sidebar-note">
        <ConsoleIcon name="info" />
        <span>本期仅开放系统设置与审计日志。</span>
      </div>

      <div class="console-sidebar-user">
        <span class="console-avatar">管</span>
        <span class="console-user-copy"><strong>平台管理员</strong><small>系统管理员</small></span>
        <button class="console-logout" type="button" aria-label="退出登录" @click="logout"><ConsoleIcon name="logout" /></button>
      </div>
    </aside>

    <main class="console-main">
      <header class="console-topbar">
        <button class="console-menu-button" type="button" aria-label="打开导航菜单" @click="mobileMenuOpen = true"><ConsoleIcon name="menu" /></button>
        <div class="console-crumb"><span>基础能力平台</span><ConsoleIcon name="chevron" /><strong>{{ viewMeta.crumb }}</strong></div>
        <div class="console-topbar-actions">
          <button class="console-icon-button" type="button" aria-label="通知" @click="showToast('暂无新的平台通知。')"><ConsoleIcon name="bell" /><i></i></button>
          <span class="console-topbar-avatar">管</span>
        </div>
      </header>

      <section class="console-content">
        <div class="console-page-head">
          <div>
            <h1>{{ viewMeta.title }}</h1>
            <p>{{ viewMeta.description }}</p>
          </div>
          <button v-if="currentView === 'audit'" class="console-button secondary" type="button" @click="exportAuditRecords"><ConsoleIcon name="export" />导出日志</button>
        </div>

        <section v-if="currentView === 'audit'" class="audit-view" aria-label="审计日志列表">
          <div class="audit-readonly-note"><ConsoleIcon name="info" /><span>审计事件与运行日志分离存储；本页用于查询 <code>audit_event</code> 及其变更摘要，运行日志、Trace、Metric 与告警请在“安全与可观测”中查看。</span></div>
          <div class="console-filter-bar audit-filter-bar">
            <label class="console-search-field">
              <ConsoleIcon name="search" />
              <input v-model="auditKeyword" type="search" placeholder="操作人 / 请求路径 / Request ID / Trace ID…" />
            </label>
            <label class="console-select-field"><select v-model="auditApplication" aria-label="应用"><option value="">全部应用</option><option>基础能力平台</option><option>合同管理</option><option>项目管理</option></select></label>
            <label class="console-select-field"><select v-model="auditEnvironment" aria-label="环境"><option value="">全部环境</option><option value="production">生产环境</option><option value="staging">预发布环境</option></select></label>
            <label class="console-select-field"><select v-model="auditType" aria-label="操作类型"><option value="">全部操作</option><option>登录</option><option>新增</option><option>修改</option><option>导出</option><option>状态变更</option></select></label>
            <label class="console-select-field"><select v-model="auditRisk" aria-label="风险等级"><option value="">全部风险</option><option>高</option><option>中</option><option>低</option></select></label>
            <label class="console-select-field"><select v-model="auditResult" aria-label="操作结果"><option value="">全部结果</option><option>成功</option><option>拒绝</option></select></label>
            <label class="console-select-field"><select v-model="auditTimeRange" aria-label="时间范围"><option value="24h">最近 24 小时</option><option value="7d">最近 7 天</option><option value="30d">最近 30 天</option></select></label>
            <button class="console-button primary small" type="button" @click="showToast(`已筛选出 ${filteredAuditRecords.length} 条审计事件。`)">查询</button>
            <button class="console-button ghost small" type="button" @click="resetAuditFilters"><ConsoleIcon name="reset" />重置</button>
          </div>

          <div class="audit-batch-bar">
            <span>已选择 <b>{{ selectedAuditIds.length }}</b> 条事件</span>
            <div><button class="console-button ghost small" type="button" :disabled="!selectedAuditIds.length" @click="deleteAuditRecords(selectedAuditIds)">批量删除</button><button class="console-button secondary small" type="button" @click="exportAuditRecords"><ConsoleIcon name="export" />导出筛选结果</button></div>
          </div>

          <div class="console-table-card audit-table-card">
            <div class="console-table-scroll">
              <table class="console-data-table audit-data-table">
                <thead><tr><th class="audit-check-cell"><input type="checkbox" :checked="allPageAuditSelected" aria-label="全选当前页审计事件" @change="toggleAllAuditRecords" /></th><th>发生时间</th><th>操作人</th><th>操作</th><th>应用 / 环境</th><th>资源对象</th><th>方法 / 路径</th><th>客户端 IP</th><th>状态</th><th>风险</th><th class="console-actions-cell">操作</th></tr></thead>
                <tbody>
                  <tr v-for="record in pagedAuditRecords" :key="record.id">
                    <td class="audit-check-cell"><input v-model="selectedAuditIds" type="checkbox" :value="record.id" :aria-label="`选择 ${record.id}`" /></td>
                    <td class="console-mono" data-label="发生时间">{{ record.time }}</td>
                    <td data-label="操作人"><strong class="console-entity-name">{{ record.operator }}</strong></td>
                    <td data-label="操作"><span class="console-badge" :class="`type-${record.type}`">{{ record.type }}</span><span class="console-entity-meta">{{ record.action }}</span></td>
                    <td data-label="应用 / 环境"><strong>{{ record.application }}</strong><span class="console-entity-meta">{{ record.environment }}</span></td>
                    <td data-label="资源对象"><strong>{{ record.object }}</strong><span class="console-entity-meta">{{ record.resource }}</span></td>
                    <td data-label="方法 / 路径"><span class="audit-method">{{ record.method }}</span><span class="console-entity-meta console-mono">{{ record.path }}</span></td>
                    <td class="console-mono" data-label="客户端 IP">{{ record.ip }}</td>
                    <td data-label="状态"><span class="console-badge" :class="record.result === '成功' ? 'status-active' : 'audit-result-denied'">{{ record.statusCode }} · {{ record.result }}</span></td>
                    <td data-label="风险"><span class="console-badge" :class="`risk-${record.risk}`">{{ record.risk }}</span></td>
                    <td class="console-actions-cell" data-label="操作"><button class="console-text-button" type="button" @click="openAuditDetail(record)">详情</button><button class="console-text-button danger" type="button" @click="deleteAuditRecords([record.id])">删除</button></td>
                  </tr>
                  <tr v-if="!pagedAuditRecords.length"><td class="console-empty" colspan="12">未找到符合筛选条件的审计事件。</td></tr>
                </tbody>
              </table>
            </div>
            <footer class="console-table-footer audit-table-footer"><span>第 {{ auditPage }} / {{ auditTotalPages }} 页 · 共 {{ filteredAuditRecords.length }} 条前端示例事件 · 生产环境应使用受控保留策略</span><div class="audit-pagination"><button class="console-text-button" type="button" :disabled="auditPage === 1" @click="changeAuditPage(auditPage - 1)">上一页</button><span class="console-page-token">{{ auditPage }} / {{ auditTotalPages }}</span><button class="console-text-button" type="button" :disabled="auditPage === auditTotalPages" @click="changeAuditPage(auditPage + 1)">下一页</button></div></footer>
          </div>
        </section>

        <section v-else class="settings-view" aria-label="系统设置">
          <div class="console-tabs" role="tablist" aria-label="系统设置分类">
            <button v-for="tab in settingsTabs" :key="tab.key" class="console-tab" :class="{ active: activeSettingsTab === tab.key }" type="button" role="tab" :aria-selected="activeSettingsTab === tab.key" @click="activeSettingsTab = tab.key">{{ tab.label }}</button>
          </div>

          <div v-if="activeSettingsTab === 'base'" class="console-card settings-card">
            <div class="console-card-body">
              <h2>平台基础信息</h2>
              <p class="console-card-hint">用于定义基础能力平台的展示名称、时区和备案信息。</p>
              <div class="console-form-grid">
                <label class="console-form-item"><span>平台名称</span><input v-model="settings.organizationName" /></label>
                <label class="console-form-item"><span>平台简称</span><input v-model="settings.organizationAlias" /></label>
                <label class="console-form-item"><span>系统时区</span><select v-model="settings.timezone"><option>(GMT+08:00) 北京 / 上海</option><option>(GMT+09:00) 东京</option><option>(GMT+00:00) 伦敦</option></select></label>
                <div class="console-form-item"><span>平台标识</span><div class="console-logo-preview"><b>基</b><small>默认文字标识，后续可接入本地文件上传。</small></div></div>
                <label class="console-form-item full"><span>备案 / 资质信息</span><textarea v-model="settings.qualification" rows="3"></textarea></label>
              </div>
              <div class="console-form-actions"><button class="console-button primary" type="button" @click="saveSettings"><ConsoleIcon name="save" />保存设置</button><button class="console-button ghost" type="button" @click="resetSettings">重置</button></div>
            </div>
          </div>

          <IamSettingsModule v-else-if="activeSettingsTab === 'iam'" @toast="showToast" />

          <div v-else-if="activeSettingsTab === 'notify'" class="console-card settings-card"><div class="console-card-body"><h2>通知设置</h2><p class="console-card-hint">配置基础平台内的安全事件、审计导出和系统告警通知方式。</p>
            <div class="console-setting-list">
              <div class="console-setting-row"><div><strong>站内信</strong><p>安全策略变更、审计导出等重要事件推送到平台通知中心。</p></div><button class="console-switch" :class="{ on: settings.inboxEnabled }" type="button" :aria-pressed="settings.inboxEnabled" @click="settings.inboxEnabled = !settings.inboxEnabled"><i></i></button></div>
              <div class="console-setting-row"><div><strong>邮件通知</strong><p>将高风险审计事件和安全告警发送至已配置的管理员邮箱。</p></div><button class="console-switch" :class="{ on: settings.emailEnabled }" type="button" :aria-pressed="settings.emailEnabled" @click="settings.emailEnabled = !settings.emailEnabled"><i></i></button></div>
              <label class="console-setting-row"><span><strong>提醒频率</strong><p>安全告警的批量推送频率。</p></span><select v-model="settings.reminderFrequency" class="console-control-select"><option>每日</option><option>每 4 小时</option><option>仅一次</option></select></label>
            </div>
            <div class="console-form-actions"><button class="console-button primary" type="button" @click="saveSettings"><ConsoleIcon name="save" />保存设置</button></div>
          </div></div>

          <SecurityObservabilityModule v-else-if="activeSettingsTab === 'security'" @toast="showToast" />

          <div v-else class="console-card settings-card"><div class="console-card-body"><h2>字典管理</h2><p class="console-card-hint">本期保留字典管理入口，字典项维护接口后续由平台配置模块接入。</p>
            <div class="console-setting-list">
              <div class="console-setting-row"><div><strong>审计操作类型</strong><p>登录、查看、新增、修改、删除、导出、状态变更。</p></div><button class="console-button ghost small" type="button" @click="showToast('字典维护功能待配置模块接口接入。')">管理</button></div>
              <div class="console-setting-row"><div><strong>风险等级</strong><p>高、中、低；用于审计事件风险分级展示。</p></div><button class="console-button ghost small" type="button" @click="showToast('字典维护功能待配置模块接口接入。')">管理</button></div>
              <div class="console-setting-row"><div><strong>通知事件类型</strong><p>安全告警、策略变更、审计导出、系统异常。</p></div><button class="console-button ghost small" type="button" @click="showToast('字典维护功能待配置模块接口接入。')">管理</button></div>
            </div>
          </div></div>
        </section>
      </section>
    </main>

    <div v-if="auditDetail" class="console-modal-backdrop" role="presentation" @click.self="closeAuditDetail">
      <section class="console-detail-modal audit-detail-modal" role="dialog" aria-modal="true" aria-label="审计事件详情">
        <header><div><p class="console-modal-eyebrow">AUDIT EVENT · {{ auditDetail.id }}</p><h2>{{ auditDetail.object }}</h2></div><button class="console-modal-close" type="button" aria-label="关闭审计事件详情" @click="closeAuditDetail"><ConsoleIcon name="close" /></button></header>
        <div class="audit-detail-grid"><div><span>发生时间</span><strong>{{ auditDetail.time }}</strong></div><div><span>操作人</span><strong>{{ auditDetail.operator }}</strong></div><div><span>应用 / 环境</span><strong>{{ auditDetail.application }} / {{ auditDetail.environment }}</strong></div><div><span>操作结果</span><strong>{{ auditDetail.statusCode }} · {{ auditDetail.result }} · {{ auditDetail.risk }}风险</strong></div><div><span>HTTP 请求</span><strong>{{ auditDetail.method }} {{ auditDetail.path }}</strong></div><div><span>客户端</span><strong>{{ auditDetail.ip }} · {{ auditDetail.userAgent }}</strong></div></div>
        <section class="audit-detail-section"><h3>事件说明</h3><p>{{ auditDetail.detail }}</p></section>
        <section class="audit-detail-section"><h3>数据变更摘要</h3><p>{{ auditDetail.changeSummary }}</p><small>审计事件保存操作上下文；数据变更明细由独立 audit_change 存储，敏感字段按脱敏和最小化原则展示。</small></section>
        <footer><button class="console-button ghost" type="button" @click="closeAuditDetail">关闭</button></footer>
      </section>
    </div>

    <button v-if="mobileMenuOpen" class="console-menu-mask" type="button" aria-label="关闭导航遮罩" @click="mobileMenuOpen = false"></button>
    <div v-if="toastMessage" class="console-toast" role="status"><ConsoleIcon name="info" /><span>{{ toastMessage }}</span></div>
  </div>
</template>
