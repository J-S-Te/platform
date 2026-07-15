<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import ConsoleIcon from '@/components/ConsoleIcon.vue'
import '@/styles/console.css'

const initialSettings = {
  organizationName: '基础能力平台',
  organizationAlias: '基础平台',
  timezone: '(GMT+08:00) 北京 / 上海',
  qualification: '统一社会信用代码：待后端接入 · 等保备案信息：待配置',
  inboxEnabled: true,
  emailEnabled: true,
  smsEnabled: false,
  reminderFrequency: '每日',
  passwordLength: 12,
  lockEnabled: true,
  twoFactorEnabled: true,
  sessionTimeout: '30 分钟',
}

const settings = reactive({ ...initialSettings })
const currentView = ref(resolveView(window.location.pathname))
const activeSettingsTab = ref('base')
const mobileMenuOpen = ref(false)
const toastMessage = ref('')
const auditKeyword = ref('')
const auditType = ref('')
const auditRisk = ref('')
const activeIamPanel = ref('users')
const userKeyword = ref('')
const userStatus = ref('')
const roleKeyword = ref('')
const roleType = ref('')
const identityDetail = ref(null)
let toastTimer = null

const settingsTabs = [
  { key: 'base', label: '基础设置' },
  { key: 'iam', label: '用户与角色' },
  { key: 'notify', label: '通知设置' },
  { key: 'security', label: '安全设置' },
  { key: 'dict', label: '字典管理' },
]

const auditRecords = [
  { time: '2026-07-15 09:12:33', operator: '平台管理员', type: '登录', object: '统一认证服务', ip: '10.12.34.21', risk: '低', detail: '密码登录成功' },
  { time: '2026-07-15 09:24:10', operator: '王敏', type: '修改', object: '系统参数：会话时效', ip: '10.12.35.8', risk: '中', detail: '将会话超时时间由 60 分钟调整为 30 分钟' },
  { time: '2026-07-14 18:40:02', operator: '李明', type: '新增', object: '应用接入：项目管理', ip: '10.12.36.12', risk: '低', detail: '登记外部应用接入信息' },
  { time: '2026-07-14 11:02:55', operator: '系统', type: '状态变更', object: '安全策略：登录失败锁定', ip: '127.0.0.1', risk: '低', detail: '按最新配置加载安全策略' },
  { time: '2026-07-13 22:15:40', operator: '审计员', type: '导出', object: '审计日志（最近 30 天）', ip: '10.12.38.5', risk: '高', detail: '导出操作审计记录，已写入安全留痕' },
  { time: '2026-07-12 16:30:21', operator: '平台管理员', type: '修改', object: '通知设置：邮件通知', ip: '10.12.34.9', risk: '中', detail: '更新重要安全事件的邮件通知开关' },
]

const iamPanels = [
  { key: 'users', label: '用户' },
  { key: 'roles', label: '角色' },
]

const users = reactive([
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V6', name: '平台管理员', employeeNo: 'BP-0001', organization: '平台运营部',
    account: 'admin', source: '本地账号', accountStatus: 'ACTIVE', employmentStatus: '在职', roles: ['系统管理员'],
    scope: '全租户', lastLogin: '2026-07-15 09:12',
  },
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V7', name: '王敏', employeeNo: 'BP-0018', organization: '信息安全部',
    account: 'wang.min', source: '企业微信', accountStatus: 'ACTIVE', employmentStatus: '在职', roles: ['安全管理员'],
    scope: '全租户', lastLogin: '2026-07-15 08:46',
  },
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V8', name: '审计专员', employeeNo: 'BP-0026', organization: '内控审计部',
    account: 'audit.viewer', source: 'OIDC', accountStatus: 'ACTIVE', employmentStatus: '在职', roles: ['审计管理员'],
    scope: '审计中心', lastLogin: '2026-07-14 16:20',
  },
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V9', name: '李明', employeeNo: 'BP-0032', organization: '项目管理部',
    account: 'li.ming', source: '本地账号', accountStatus: 'DISABLED', employmentStatus: '离职', roles: ['项目管理员'],
    scope: '项目管理（待接入）', lastLogin: '2026-06-28 18:31',
  },
])

const roles = reactive([
  {
    id: 'role_01J0A3KQ9ZP8R2N7M500', code: 'platform:system:admin', name: '系统管理员', type: '平台角色',
    application: '基础能力平台', memberCount: 1, permissionCount: 18, status: 'ACTIVE', scope: '全租户',
    permissions: ['platform:user:manage', 'platform:role:manage', 'platform:config:manage', 'platform:audit:read'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M501', code: 'platform:security:admin', name: '安全管理员', type: '平台角色',
    application: '基础能力平台', memberCount: 1, permissionCount: 9, status: 'ACTIVE', scope: '全租户',
    permissions: ['platform:session:manage', 'platform:risk:read', 'platform:security:manage'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M502', code: 'platform:audit:admin', name: '审计管理员', type: '平台角色',
    application: '基础能力平台', memberCount: 1, permissionCount: 5, status: 'ACTIVE', scope: '审计中心',
    permissions: ['platform:audit:read', 'platform:audit:export', 'platform:audit:archive'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M503', code: 'project:project:manager', name: '项目管理员', type: '应用角色',
    application: '项目管理（待接入）', memberCount: 1, permissionCount: 6, status: 'ACTIVE', scope: '所属组织及下级组织',
    permissions: ['project:project:read', 'project:project:update', 'project:member:manage'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M504', code: 'platform:operations:viewer', name: '平台运维查看员', type: '自定义角色',
    application: '基础能力平台', memberCount: 0, permissionCount: 3, status: 'DISABLED', scope: '仅本人',
    permissions: ['platform:config:read', 'platform:audit:read', 'platform:log:read'],
  },
])

const filteredUsers = computed(() => {
  const keyword = userKeyword.value.trim().toLowerCase()
  return users.filter((user) => {
    const matchesKeyword = !keyword || [user.name, user.employeeNo, user.organization, user.account, ...user.roles]
      .join(' ')
      .toLowerCase()
      .includes(keyword)
    return matchesKeyword && (!userStatus.value || user.accountStatus === userStatus.value)
  })
})

const filteredRoles = computed(() => {
  const keyword = roleKeyword.value.trim().toLowerCase()
  return roles.filter((role) => {
    const matchesKeyword = !keyword || [role.name, role.code, role.application, role.scope]
      .join(' ')
      .toLowerCase()
      .includes(keyword)
    return matchesKeyword && (!roleType.value || role.type === roleType.value)
  })
})

const filteredAuditRecords = computed(() => {
  const keyword = auditKeyword.value.trim().toLowerCase()

  return auditRecords.filter((record) => {
    const matchesKeyword = !keyword || [record.operator, record.object, record.detail, record.ip]
      .join(' ')
      .toLowerCase()
      .includes(keyword)
    const matchesType = !auditType.value || record.type === auditType.value
    const matchesRisk = !auditRisk.value || record.risk === auditRisk.value
    return matchesKeyword && matchesType && matchesRisk
  })
})

const viewMeta = computed(() => currentView.value === 'audit'
  ? { title: '审计日志', crumb: '审计日志', description: 'AUD-001 · 全链路操作留痕 · 安全合规审计（只读）' }
  : { title: '系统设置', crumb: '系统设置', description: 'SYS-001 · 平台级参数、通知与安全策略集中配置' })

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
}

function resetIamFilters() {
  userKeyword.value = ''
  userStatus.value = ''
  roleKeyword.value = ''
  roleType.value = ''
}

function openIdentityDetail(kind, item) {
  identityDetail.value = { kind, item }
}

function closeIdentityDetail() {
  identityDetail.value = null
}

function createIdentity(kind) {
  showToast(kind === 'user'
    ? '新增用户表单待 IAM API 接入后启用。'
    : '新增角色表单待授权中心 API 接入后启用。')
}

function toggleUserStatus(user) {
  user.accountStatus = user.accountStatus === 'ACTIVE' ? 'DISABLED' : 'ACTIVE'
  showToast(`已在前端暂存 ${user.name} 的账号状态变更。`)
}

function toggleRoleStatus(role) {
  role.status = role.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE'
  showToast(`已在前端暂存 ${role.name} 的角色状态变更。`)
}

function exportAuditRecords() {
  const headers = ['时间', '操作人', '操作类型', '对象', 'IP 地址', '风险等级', '详情']
  const rows = filteredAuditRecords.value.map((record) => [
    record.time,
    record.operator,
    record.type,
    record.object,
    record.ip,
    record.risk,
    record.detail,
  ])
  const csv = [headers, ...rows]
    .map((row) => row.map((cell) => `"${String(cell).replaceAll('"', '""')}"`).join(','))
    .join('\n')
  const blob = new Blob([`\ufeff${csv}`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = 'audit-records.csv'
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

watch(currentView, () => {
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
          <div class="audit-readonly-note"><ConsoleIcon name="info" /><span>审计日志为只读记录；筛选和导出均不会改变原始审计数据。</span></div>
          <div class="console-filter-bar">
            <label class="console-search-field">
              <ConsoleIcon name="search" />
              <input v-model="auditKeyword" type="search" placeholder="操作人 / 对象 / IP 地址…" />
            </label>
            <label class="console-select-field"><select v-model="auditType" aria-label="操作类型"><option value="">全部类型</option><option>登录</option><option>新增</option><option>修改</option><option>导出</option><option>状态变更</option></select></label>
            <label class="console-select-field"><select v-model="auditRisk" aria-label="风险等级"><option value="">全部风险</option><option>高</option><option>中</option><option>低</option></select></label>
            <button class="console-button primary small" type="button" @click="showToast(`已筛选出 ${filteredAuditRecords.length} 条审计记录。`)">查询</button>
            <button class="console-button ghost small" type="button" @click="resetAuditFilters"><ConsoleIcon name="reset" />重置</button>
          </div>

          <div class="console-table-card">
            <div class="console-table-scroll">
              <table class="console-data-table">
                <thead><tr><th>时间</th><th>操作人</th><th>操作类型</th><th>对象</th><th>IP 地址</th><th>风险等级</th><th>详情</th></tr></thead>
                <tbody>
                  <tr v-for="record in filteredAuditRecords" :key="`${record.time}-${record.object}`">
                    <td class="console-mono">{{ record.time }}</td>
                    <td>{{ record.operator }}</td>
                    <td><span class="console-badge" :class="`type-${record.type}`">{{ record.type }}</span></td>
                    <td>{{ record.object }}</td>
                    <td class="console-mono">{{ record.ip }}</td>
                    <td><span class="console-badge" :class="`risk-${record.risk}`">{{ record.risk }}</span></td>
                    <td class="console-detail">{{ record.detail }}</td>
                  </tr>
                  <tr v-if="!filteredAuditRecords.length"><td class="console-empty" colspan="7">未找到符合筛选条件的审计记录。</td></tr>
                </tbody>
              </table>
            </div>
            <footer class="console-table-footer"><span>展示 {{ filteredAuditRecords.length }} 条前端示例记录 · 接口接入后替换为 MySQL 审计数据</span><span class="console-page-token">1 / 1</span></footer>
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

          <div v-else-if="activeSettingsTab === 'iam'" class="console-card settings-card iam-card">
            <div class="console-card-body">
              <div class="console-iam-head">
                <div><h2>用户与角色</h2><p class="console-card-hint">遵循 User、Account、Role 与 RoleBinding 模型：用户是自然人，登录账号与角色授权独立管理。</p></div>
                <button class="console-button primary" type="button" @click="createIdentity(activeIamPanel === 'users' ? 'user' : 'role')">新增{{ activeIamPanel === 'users' ? '用户' : '角色' }}</button>
              </div>

              <div class="console-sub-tabs" role="tablist" aria-label="用户与角色分类">
                <button v-for="panel in iamPanels" :key="panel.key" class="console-sub-tab" :class="{ active: activeIamPanel === panel.key }" type="button" role="tab" :aria-selected="activeIamPanel === panel.key" @click="activeIamPanel = panel.key">{{ panel.label }}</button>
              </div>

              <section v-if="activeIamPanel === 'users'" aria-label="用户列表">
                <div class="console-iam-tip"><ConsoleIcon name="info" /><span>统一用户 ID 是跨系统稳定标识；用户名、手机号和邮箱不作为跨系统主键。</span></div>
                <div class="console-filter-bar console-iam-filter">
                  <label class="console-search-field"><ConsoleIcon name="search" /><input v-model="userKeyword" type="search" placeholder="姓名 / 工号 / 账号 / 组织 / 角色" /></label>
                  <label class="console-select-field"><select v-model="userStatus" aria-label="账号状态"><option value="">全部账号状态</option><option value="ACTIVE">正常</option><option value="DISABLED">已停用</option></select></label>
                  <button class="console-button primary small" type="button" @click="showToast(`已筛选出 ${filteredUsers.length} 位用户。`)">查询</button>
                  <button class="console-button ghost small" type="button" @click="resetIamFilters"><ConsoleIcon name="reset" />重置</button>
                </div>
                <div class="console-table-card">
                  <div class="console-table-scroll"><table class="console-data-table console-iam-table">
                    <thead><tr><th>用户</th><th>登录账号</th><th>主组织</th><th>角色绑定</th><th>账号状态</th><th>最近登录</th><th class="console-actions-cell">操作</th></tr></thead>
                    <tbody>
                      <tr v-for="user in filteredUsers" :key="user.id">
                        <td><strong class="console-entity-name">{{ user.name }}</strong><span class="console-entity-meta">{{ user.employeeNo }} · {{ user.employmentStatus }}</span></td>
                        <td><span class="console-mono">{{ user.account }}</span><span class="console-entity-meta">{{ user.source }}</span></td>
                        <td>{{ user.organization }}</td>
                        <td><span v-for="role in user.roles" :key="role" class="console-role-chip">{{ role }}</span><span class="console-entity-meta">{{ user.scope }}</span></td>
                        <td><span class="console-badge" :class="user.accountStatus === 'ACTIVE' ? 'status-active' : 'status-disabled'">{{ user.accountStatus === 'ACTIVE' ? '正常' : '已停用' }}</span></td>
                        <td class="console-mono">{{ user.lastLogin }}</td>
                        <td class="console-actions-cell"><button class="console-text-button" type="button" @click="openIdentityDetail('user', user)">查看</button><button class="console-text-button" type="button" @click="toggleUserStatus(user)">{{ user.accountStatus === 'ACTIVE' ? '停用' : '启用' }}</button></td>
                      </tr>
                      <tr v-if="!filteredUsers.length"><td class="console-empty" colspan="7">未找到符合筛选条件的用户。</td></tr>
                    </tbody>
                  </table></div>
                  <footer class="console-table-footer"><span>展示 {{ filteredUsers.length }} 位前端示例用户 · 后续对接 iam_user、iam_account、iam_membership</span><span class="console-page-token">1 / 1</span></footer>
                </div>
              </section>

              <section v-else aria-label="角色列表">
                <div class="console-iam-tip"><ConsoleIcon name="info" /><span>角色区分平台角色、应用角色和自定义角色；权限编码采用 {application}:{resource}:{action} 命名空间。</span></div>
                <div class="console-filter-bar console-iam-filter">
                  <label class="console-search-field"><ConsoleIcon name="search" /><input v-model="roleKeyword" type="search" placeholder="角色名称 / 编码 / 应用范围" /></label>
                  <label class="console-select-field"><select v-model="roleType" aria-label="角色类型"><option value="">全部角色类型</option><option>平台角色</option><option>应用角色</option><option>自定义角色</option></select></label>
                  <button class="console-button primary small" type="button" @click="showToast(`已筛选出 ${filteredRoles.length} 个角色。`)">查询</button>
                  <button class="console-button ghost small" type="button" @click="resetIamFilters"><ConsoleIcon name="reset" />重置</button>
                </div>
                <div class="console-table-card">
                  <div class="console-table-scroll"><table class="console-data-table console-iam-table">
                    <thead><tr><th>角色</th><th>角色类型</th><th>应用范围</th><th>授权范围</th><th>成员 / 权限</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead>
                    <tbody>
                      <tr v-for="role in filteredRoles" :key="role.id">
                        <td><strong class="console-entity-name">{{ role.name }}</strong><span class="console-entity-meta console-mono">{{ role.code }}</span></td>
                        <td><span class="console-role-type" :class="`role-${role.type}`">{{ role.type }}</span></td>
                        <td>{{ role.application }}</td>
                        <td>{{ role.scope }}</td>
                        <td>{{ role.memberCount }} 名成员 <span class="console-entity-separator">·</span> {{ role.permissionCount }} 项权限</td>
                        <td><span class="console-badge" :class="role.status === 'ACTIVE' ? 'status-active' : 'status-disabled'">{{ role.status === 'ACTIVE' ? '启用' : '停用' }}</span></td>
                        <td class="console-actions-cell"><button class="console-text-button" type="button" @click="openIdentityDetail('role', role)">授权详情</button><button class="console-text-button" type="button" @click="toggleRoleStatus(role)">{{ role.status === 'ACTIVE' ? '停用' : '启用' }}</button></td>
                      </tr>
                      <tr v-if="!filteredRoles.length"><td class="console-empty" colspan="7">未找到符合筛选条件的角色。</td></tr>
                    </tbody>
                  </table></div>
                  <footer class="console-table-footer"><span>展示 {{ filteredRoles.length }} 个前端示例角色 · 后续对接 authz_role、authz_role_permission、authz_role_binding</span><span class="console-page-token">1 / 1</span></footer>
                </div>
              </section>
            </div>
          </div>

          <div v-else-if="activeSettingsTab === 'notify'" class="console-card settings-card"><div class="console-card-body"><h2>通知设置</h2><p class="console-card-hint">配置基础平台内的安全事件、审计导出和系统告警通知方式。</p>
            <div class="console-setting-list">
              <div class="console-setting-row"><div><strong>站内信</strong><p>安全策略变更、审计导出等重要事件推送到平台通知中心。</p></div><button class="console-switch" :class="{ on: settings.inboxEnabled }" type="button" :aria-pressed="settings.inboxEnabled" @click="settings.inboxEnabled = !settings.inboxEnabled"><i></i></button></div>
              <div class="console-setting-row"><div><strong>邮件通知</strong><p>将高风险审计事件和安全告警发送至已配置的管理员邮箱。</p></div><button class="console-switch" :class="{ on: settings.emailEnabled }" type="button" :aria-pressed="settings.emailEnabled" @click="settings.emailEnabled = !settings.emailEnabled"><i></i></button></div>
              <div class="console-setting-row"><div><strong>短信通知</strong><p>默认仅对高风险安全事件启用，短信服务后续由外部服务接入。</p></div><button class="console-switch" :class="{ on: settings.smsEnabled }" type="button" :aria-pressed="settings.smsEnabled" @click="settings.smsEnabled = !settings.smsEnabled"><i></i></button></div>
              <label class="console-setting-row"><span><strong>提醒频率</strong><p>安全告警的批量推送频率。</p></span><select v-model="settings.reminderFrequency" class="console-control-select"><option>每日</option><option>每 4 小时</option><option>仅一次</option></select></label>
            </div>
            <div class="console-form-actions"><button class="console-button primary" type="button" @click="saveSettings"><ConsoleIcon name="save" />保存设置</button></div>
          </div></div>

          <div v-else-if="activeSettingsTab === 'security'" class="console-card settings-card"><div class="console-card-body"><h2>安全设置</h2><p class="console-card-hint">安全设置将作为统一认证和平台访问控制的基础策略。</p>
            <div class="console-setting-list">
              <label class="console-setting-row"><span><strong>密码最小长度</strong><p>要求包含大小写字母、数字和特殊符号。</p></span><input v-model.number="settings.passwordLength" class="console-number-input" type="number" min="8" max="64" /></label>
              <div class="console-setting-row"><div><strong>登录失败锁定</strong><p>连续 5 次失败后锁定账号 15 分钟。</p></div><button class="console-switch" :class="{ on: settings.lockEnabled }" type="button" :aria-pressed="settings.lockEnabled" @click="settings.lockEnabled = !settings.lockEnabled"><i></i></button></div>
              <div class="console-setting-row"><div><strong>双因子认证（2FA）</strong><p>后续认证接口接入后，可针对管理员账号强制启用。</p></div><button class="console-switch" :class="{ on: settings.twoFactorEnabled }" type="button" :aria-pressed="settings.twoFactorEnabled" @click="settings.twoFactorEnabled = !settings.twoFactorEnabled"><i></i></button></div>
              <label class="console-setting-row"><span><strong>会话时效</strong><p>用户无操作后，服务端会话过期时间。</p></span><select v-model="settings.sessionTimeout" class="console-control-select"><option>30 分钟</option><option>1 小时</option><option>4 小时</option></select></label>
            </div>
            <div class="console-form-actions"><button class="console-button primary" type="button" @click="saveSettings"><ConsoleIcon name="save" />保存设置</button></div>
          </div></div>

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

    <div v-if="identityDetail" class="console-modal-backdrop" role="presentation" @click.self="closeIdentityDetail">
      <section class="console-detail-modal" role="dialog" aria-modal="true" :aria-label="identityDetail.kind === 'user' ? '用户详情' : '角色授权详情'">
        <header><div><p class="console-modal-eyebrow">{{ identityDetail.kind === 'user' ? '用户与账号' : '角色与权限' }}</p><h2>{{ identityDetail.item.name }}</h2></div><button class="console-modal-close" type="button" aria-label="关闭详情" @click="closeIdentityDetail"><ConsoleIcon name="close" /></button></header>
        <template v-if="identityDetail.kind === 'user'">
          <div class="console-detail-grid"><div><span>统一用户 ID</span><strong class="console-mono">{{ identityDetail.item.id }}</strong></div><div><span>员工编号</span><strong>{{ identityDetail.item.employeeNo }}</strong></div><div><span>主组织</span><strong>{{ identityDetail.item.organization }}</strong></div><div><span>任职状态</span><strong>{{ identityDetail.item.employmentStatus }}</strong></div><div><span>登录账号</span><strong class="console-mono">{{ identityDetail.item.account }}</strong></div><div><span>身份来源</span><strong>{{ identityDetail.item.source }}</strong></div><div><span>账号状态</span><strong>{{ identityDetail.item.accountStatus === 'ACTIVE' ? '正常' : '已停用' }}</strong></div><div><span>最近登录</span><strong class="console-mono">{{ identityDetail.item.lastLogin }}</strong></div></div>
          <div class="console-detail-section"><h3>角色绑定</h3><p>主体类型：用户 · 数据范围：{{ identityDetail.item.scope }}</p><span v-for="role in identityDetail.item.roles" :key="role" class="console-role-chip large">{{ role }}</span></div>
        </template>
        <template v-else>
          <div class="console-detail-grid"><div><span>角色编码</span><strong class="console-mono">{{ identityDetail.item.code }}</strong></div><div><span>角色类型</span><strong>{{ identityDetail.item.type }}</strong></div><div><span>应用范围</span><strong>{{ identityDetail.item.application }}</strong></div><div><span>授权范围</span><strong>{{ identityDetail.item.scope }}</strong></div><div><span>已绑定成员</span><strong>{{ identityDetail.item.memberCount }} 名</strong></div><div><span>角色状态</span><strong>{{ identityDetail.item.status === 'ACTIVE' ? '启用' : '停用' }}</strong></div></div>
          <div class="console-detail-section"><h3>权限清单</h3><p>第一期按 allow 授权；高风险权限应由后端授权服务统一判定并记录审计。</p><code v-for="permission in identityDetail.item.permissions" :key="permission" class="console-permission-code">{{ permission }}</code></div>
        </template>
        <footer><button class="console-button ghost" type="button" @click="closeIdentityDetail">关闭</button><button class="console-button primary" type="button" @click="showToast('编辑能力待 Go 授权 API 接入后启用。'); closeIdentityDetail()">编辑{{ identityDetail.kind === 'user' ? '用户' : '角色' }}</button></footer>
      </section>
    </div>

    <button v-if="mobileMenuOpen" class="console-menu-mask" type="button" aria-label="关闭导航遮罩" @click="mobileMenuOpen = false"></button>
    <div v-if="toastMessage" class="console-toast" role="status"><ConsoleIcon name="info" /><span>{{ toastMessage }}</span></div>
  </div>
</template>
