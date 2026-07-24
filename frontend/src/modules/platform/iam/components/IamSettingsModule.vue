<script setup>
import { computed, reactive, ref } from 'vue'
import ConsoleIcon from '@/modules/platform/shared/components/ConsoleIcon.vue'
import '@/modules/platform/iam/styles/iam-settings.css'

const emit = defineEmits(['toast'])

const activePanel = ref('users')
const detail = ref(null)
const editor = ref(null)
const filters = reactive({
  user: '',
  account: '',
  organization: '',
  role: '',
  binding: '',
  permission: '',
})
const form = reactive({})

const panels = [
  { key: 'users', label: '用户', icon: 'user', description: '自然人主体、任职状态与跨系统统一用户标识' },
  { key: 'accounts', label: '登录账号', icon: 'account', description: '账号状态、认证来源与外部身份绑定' },
  { key: 'organizations', label: '组织与任职', icon: 'organization', description: '组织单元、主组织、兼岗和历史任职关系' },
  { key: 'roles', label: '角色', icon: 'role', description: '平台角色、应用角色、自定义角色与权限集合' },
  { key: 'bindings', label: '角色绑定', icon: 'link', description: '主体、应用范围、数据范围和有效期授权' },
  { key: 'permissions', label: '权限注册', icon: 'shield', description: '以 application:resource:action 统一登记原子权限' },
]

const users = reactive([
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V6', displayName: '平台管理员', legalName: '张伟', employeeNo: 'BP-0001',
    primaryOrg: '平台运营部', membershipCount: 2, employmentStatus: 'ACTIVE', status: 'ACTIVE', accountCount: 1,
    accounts: ['admin'], roles: ['系统管理员'], lastActiveAt: '2026-07-16 09:12',
    memberships: [
      { organization: '平台运营部', type: 'PRIMARY', position: '平台管理员', startedAt: '2024-01-02', endedAt: '—', status: 'ACTIVE' },
      { organization: '信息安全部', type: 'PART_TIME', position: '安全联络员', startedAt: '2025-08-01', endedAt: '—', status: 'ACTIVE' },
    ],
  },
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V7', displayName: '王敏', legalName: '王敏', employeeNo: 'BP-0018',
    primaryOrg: '信息安全部', membershipCount: 1, employmentStatus: 'ACTIVE', status: 'ACTIVE', accountCount: 1,
    accounts: ['wang.min'], roles: ['安全管理员'], lastActiveAt: '2026-07-16 08:46',
    memberships: [{ organization: '信息安全部', type: 'PRIMARY', position: '安全工程师', startedAt: '2023-07-15', endedAt: '—', status: 'ACTIVE' }],
  },
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V8', displayName: '审计专员', legalName: '陈静', employeeNo: 'BP-0026',
    primaryOrg: '内控审计部', membershipCount: 1, employmentStatus: 'ON_LEAVE', status: 'ACTIVE', accountCount: 1,
    accounts: ['audit.viewer'], roles: ['审计管理员'], lastActiveAt: '2026-07-14 16:20',
    memberships: [{ organization: '内控审计部', type: 'PRIMARY', position: '审计专员', startedAt: '2024-03-10', endedAt: '—', status: 'ACTIVE' }],
  },
  {
    id: 'usr_01J0A3KQ9ZP8R2N7M4V9', displayName: '李明', legalName: '李明', employeeNo: 'BP-0032',
    primaryOrg: '项目管理部', membershipCount: 1, employmentStatus: 'TERMINATED', status: 'DISABLED', accountCount: 1,
    accounts: ['li.ming'], roles: ['项目管理员'], lastActiveAt: '2026-06-28 18:31',
    memberships: [{ organization: '项目管理部', type: 'PRIMARY', position: '项目经理', startedAt: '2023-04-12', endedAt: '2026-06-30', status: 'HISTORICAL' }],
  },
])

const accounts = reactive([
  {
    id: 'acc_01J0A3KQ9ZP8R2N7M4A1', username: 'admin', user: '平台管理员', userId: users[0].id, accountType: 'HUMAN',
    authSource: 'LOCAL', status: 'ACTIVE', lockedUntil: '—', lastLoginAt: '2026-07-16 09:12',
    identities: [{ provider: 'DINGTALK', subject: 'ding_23a9••••f731', status: 'BOUND', boundAt: '2025-01-10 10:12' }],
  },
  {
    id: 'acc_01J0A3KQ9ZP8R2N7M4A2', username: 'wang.min', user: '王敏', userId: users[1].id, accountType: 'HUMAN',
    authSource: 'WECOM', status: 'ACTIVE', lockedUntil: '—', lastLoginAt: '2026-07-16 08:46',
    identities: [{ provider: 'WECOM', subject: 'wm_8bc7••••a046', status: 'BOUND', boundAt: '2025-03-21 09:30' }],
  },
  {
    id: 'acc_01J0A3KQ9ZP8R2N7M4A3', username: 'audit.viewer', user: '审计专员', userId: users[2].id, accountType: 'HUMAN',
    authSource: 'OIDC', status: 'LOCKED', lockedUntil: '2026-07-16 12:30', lastLoginAt: '2026-07-14 16:20',
    identities: [{ provider: 'OIDC', subject: 'sub_01J0A3KQ••••9XZ', status: 'BOUND', boundAt: '2024-03-10 14:22' }],
  },
  {
    id: 'acc_01J0A3KQ9ZP8R2N7M4A4', username: 'project.bot', user: '—', userId: '', accountType: 'SERVICE',
    authSource: 'LOCAL', status: 'ACTIVE', lockedUntil: '—', lastLoginAt: '2026-07-15 23:50', identities: [],
  },
  {
    id: 'acc_01J0A3KQ9ZP8R2N7M4A5', username: 'li.ming', user: '李明', userId: users[3].id, accountType: 'HUMAN',
    authSource: 'LOCAL', status: 'DISABLED', lockedUntil: '—', lastLoginAt: '2026-06-28 18:31', identities: [],
  },
])

const organizations = reactive([
  { id: 'org_platform', level: 0, code: 'PLATFORM', name: '基础能力平台', type: 'COMPANY', parent: '—', manager: '平台管理员', status: 'ACTIVE', memberCount: 12 },
  { id: 'org_operation', level: 1, code: 'OPERATION', name: '平台运营部', type: 'DEPARTMENT', parent: '基础能力平台', manager: '平台管理员', status: 'ACTIVE', memberCount: 4 },
  { id: 'org_security', level: 1, code: 'SECURITY', name: '信息安全部', type: 'DEPARTMENT', parent: '基础能力平台', manager: '王敏', status: 'ACTIVE', memberCount: 3 },
  { id: 'org_audit', level: 1, code: 'AUDIT', name: '内控审计部', type: 'DEPARTMENT', parent: '基础能力平台', manager: '陈静', status: 'ACTIVE', memberCount: 2 },
  { id: 'org_project', level: 1, code: 'PROJECT', name: '项目管理部', type: 'DEPARTMENT', parent: '基础能力平台', manager: '李明', status: 'ACTIVE', memberCount: 3 },
  { id: 'org_delivery', level: 2, code: 'DELIVERY', name: '项目交付组', type: 'TEAM', parent: '项目管理部', manager: '—', status: 'ACTIVE', memberCount: 1 },
])

const roles = reactive([
  {
    id: 'role_01J0A3KQ9ZP8R2N7M500', code: 'platform:system:admin', name: '系统管理员', type: 'PLATFORM', application: '基础能力平台',
    memberCount: 1, permissionCount: 18, status: 'ACTIVE', defaultScope: 'all',
    permissions: ['platform:user:manage', 'platform:account:manage', 'platform:role:manage', 'platform:config:manage', 'platform:audit:read'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M501', code: 'platform:security:admin', name: '安全管理员', type: 'PLATFORM', application: '基础能力平台',
    memberCount: 1, permissionCount: 9, status: 'ACTIVE', defaultScope: 'all',
    permissions: ['platform:session:manage', 'platform:risk:read', 'platform:security:manage', 'platform:identity:manage'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M502', code: 'platform:audit:admin', name: '审计管理员', type: 'PLATFORM', application: '基础能力平台',
    memberCount: 1, permissionCount: 5, status: 'ACTIVE', defaultScope: 'all',
    permissions: ['platform:audit:read', 'platform:audit:export', 'platform:audit:archive'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M503', code: 'contract:contract:manager', name: '合同管理员', type: 'APPLICATION', application: '合同管理',
    memberCount: 0, permissionCount: 6, status: 'ACTIVE', defaultScope: 'org_tree',
    permissions: ['contract:contract:create', 'contract:contract:read', 'contract:contract:update', 'contract:contract:approve', 'contract:contract:export'],
  },
  {
    id: 'role_01J0A3KQ9ZP8R2N7M504', code: 'platform:operations:viewer', name: '平台运维查看员', type: 'CUSTOM', application: '基础能力平台',
    memberCount: 0, permissionCount: 3, status: 'DISABLED', defaultScope: 'self',
    permissions: ['platform:config:read', 'platform:audit:read', 'platform:log:read'],
  },
])

const bindings = reactive([
  { id: 'rb_01J0A3KQ9ZP8R2N7MB01', subjectType: 'USER', subject: '平台管理员', role: '系统管理员', application: '基础能力平台', scope: 'all', status: 'ACTIVE', effective: '2024-01-02 至 长期有效' },
  { id: 'rb_01J0A3KQ9ZP8R2N7MB02', subjectType: 'USER', subject: '王敏', role: '安全管理员', application: '基础能力平台', scope: 'all', status: 'ACTIVE', effective: '2025-03-21 至 长期有效' },
  { id: 'rb_01J0A3KQ9ZP8R2N7MB03', subjectType: 'ORG_UNIT', subject: '项目管理部', role: '合同管理员', application: '合同管理', scope: 'org_tree', status: 'ACTIVE', effective: '2026-07-01 至 2026-12-31' },
  { id: 'rb_01J0A3KQ9ZP8R2N7MB04', subjectType: 'SERVICE_ACCOUNT', subject: 'project.bot', role: '平台运维查看员', application: '基础能力平台', scope: 'self', status: 'DISABLED', effective: '2026-01-01 至 长期有效' },
])

const permissions = reactive([
  { id: 'perm_001', code: 'platform:user:manage', name: '用户管理', application: '基础能力平台', resource: 'user', action: 'manage', risk: 'HIGH', status: 'ACTIVE', description: '创建、停用与维护自然人用户信息。' },
  { id: 'perm_002', code: 'platform:account:manage', name: '账号管理', application: '基础能力平台', resource: 'account', action: 'manage', risk: 'HIGH', status: 'ACTIVE', description: '启停、解锁账号及处理外部身份绑定。' },
  { id: 'perm_003', code: 'platform:role:manage', name: '角色管理', application: '基础能力平台', resource: 'role', action: 'manage', risk: 'HIGH', status: 'ACTIVE', description: '创建角色、分配权限和调整角色绑定。' },
  { id: 'perm_004', code: 'platform:audit:read', name: '审计查询', application: '基础能力平台', resource: 'audit', action: 'read', risk: 'MEDIUM', status: 'ACTIVE', description: '查询不可篡改的审计事件。' },
  { id: 'perm_005', code: 'platform:audit:export', name: '审计导出', application: '基础能力平台', resource: 'audit', action: 'export', risk: 'HIGH', status: 'ACTIVE', description: '导出审计事件，必须额外留痕。' },
  { id: 'perm_006', code: 'contract:contract:create', name: '创建合同', application: '合同管理', resource: 'contract', action: 'create', risk: 'MEDIUM', status: 'ACTIVE', description: '合同管理应用注册的业务原子权限。' },
  { id: 'perm_007', code: 'contract:contract:approve', name: '审批合同', application: '合同管理', resource: 'contract', action: 'approve', risk: 'HIGH', status: 'ACTIVE', description: '高风险操作须由授权决策接口统一判定。' },
])

const panel = computed(() => panels.find((item) => item.key === activePanel.value) || panels[0])
const metrics = computed(() => [
  { label: '有效用户', value: users.filter((item) => item.status === 'ACTIVE').length, note: '自然人 User', icon: 'user', tone: 'blue' },
  { label: '正常账号', value: accounts.filter((item) => item.status === 'ACTIVE').length, note: 'Account 登录主体', icon: 'account', tone: 'violet' },
  { label: '有效角色绑定', value: bindings.filter((item) => item.status === 'ACTIVE').length, note: 'RoleBinding', icon: 'link', tone: 'green' },
  { label: '高风险权限', value: permissions.filter((item) => item.risk === 'HIGH').length, note: '需审计与失败关闭', icon: 'shield', tone: 'orange' },
])

const filteredUsers = computed(() => includesFilter(users, filters.user, ['displayName', 'employeeNo', 'primaryOrg', 'accounts', 'roles']))
const filteredAccounts = computed(() => includesFilter(accounts, filters.account, ['username', 'user', 'accountType', 'authSource', 'status']))
const filteredOrganizations = computed(() => includesFilter(organizations, filters.organization, ['name', 'code', 'parent', 'manager']))
const filteredRoles = computed(() => includesFilter(roles, filters.role, ['name', 'code', 'application', 'permissions']))
const filteredBindings = computed(() => includesFilter(bindings, filters.binding, ['subject', 'subjectType', 'role', 'application', 'scope']))
const filteredPermissions = computed(() => includesFilter(permissions, filters.permission, ['name', 'code', 'application', 'resource', 'action']))

function includesFilter(items, filter, fields) {
  const keyword = filter.trim().toLowerCase()
  if (!keyword) return items
  return items.filter((item) => fields
    .flatMap((field) => Array.isArray(item[field]) ? item[field] : [item[field]])
    .join(' ')
    .toLowerCase()
    .includes(keyword))
}

function displayStatus(status) {
  return ({ ACTIVE: '启用', DISABLED: '停用', LOCKED: '已锁定', EXPIRED: '已失效', BOUND: '已绑定', HISTORICAL: '历史任职' }[status] || status)
}

function displayRoleType(type) {
  return ({ PLATFORM: '平台角色', APPLICATION: '应用角色', CUSTOM: '自定义角色' }[type] || type)
}

function displaySubjectType(type) {
  return ({ USER: '用户', ORG_UNIT: '组织', POSITION: '岗位', GROUP: '用户组', SERVICE_ACCOUNT: '服务账号' }[type] || type)
}

function displayScope(scope) {
  return ({ all: '全部数据', self: '仅本人', org: '所属组织', org_tree: '所属组织及下级', custom_org: '指定组织', participant: '参与人相关' }[scope] || scope)
}

function displayEmployment(status) {
  return ({ ACTIVE: '在职', ON_LEAVE: '请假中', TERMINATED: '已离职' }[status] || status)
}

function emitToast(message) {
  emit('toast', message)
}

function selectPanel(key) {
  activePanel.value = key
  detail.value = null
}

function openDetail(kind, item) {
  detail.value = { kind, item }
}

function closeDetail() {
  detail.value = null
}

function resetFilters() {
  Object.keys(filters).forEach((key) => { filters[key] = '' })
  emitToast('已清空当前前端筛选条件。')
}

function openEditor(kind, context = null) {
  editor.value = { kind, context }
  const templates = {
    user: { displayName: '', employeeNo: '', primaryOrg: '平台运营部', employmentStatus: 'ACTIVE' },
    account: { username: '', user: '', accountType: 'HUMAN', authSource: 'LOCAL', status: 'ACTIVE' },
    organization: { code: '', name: '', type: 'DEPARTMENT', parent: '基础能力平台', manager: '' },
    role: { code: '', name: '', type: 'CUSTOM', application: '基础能力平台', defaultScope: 'self' },
    binding: { subjectType: 'USER', subject: '', role: '', application: '基础能力平台', scope: 'self', effective: '立即生效 至 长期有效' },
    permission: { code: '', name: '', application: '基础能力平台', resource: '', action: '', risk: 'MEDIUM' },
    identity: { provider: 'DINGTALK', subject: '' },
  }
  Object.keys(form).forEach((key) => delete form[key])
  Object.assign(form, templates[kind] || {})
}

function closeEditor() {
  editor.value = null
}

function saveEditor() {
  if (!editor.value) return
  const { kind, context } = editor.value
  const suffix = `${Date.now()}`.slice(-6)

  if (kind === 'user') {
    users.unshift({
      id: `usr_demo_${suffix}`, displayName: form.displayName || '未命名用户', legalName: form.displayName || '未命名用户', employeeNo: form.employeeNo || `BP-${suffix}`,
      primaryOrg: form.primaryOrg, membershipCount: 1, employmentStatus: form.employmentStatus, status: 'ACTIVE', accountCount: 0, accounts: [], roles: [], lastActiveAt: '尚未登录',
      memberships: [{ organization: form.primaryOrg, type: 'PRIMARY', position: '待分配', startedAt: '2026-07-16', endedAt: '—', status: 'ACTIVE' }],
    })
  } else if (kind === 'account') {
    accounts.unshift({
      id: `acc_demo_${suffix}`, username: form.username || `account.${suffix}`, user: form.user || '—', userId: '', accountType: form.accountType,
      authSource: form.authSource, status: form.status, lockedUntil: '—', lastLoginAt: '尚未登录', identities: [],
    })
  } else if (kind === 'organization') {
    organizations.push({ id: `org_demo_${suffix}`, level: form.parent === '基础能力平台' ? 1 : 2, code: form.code || `ORG_${suffix}`, name: form.name || '未命名组织', type: form.type, parent: form.parent, manager: form.manager || '—', status: 'ACTIVE', memberCount: 0 })
  } else if (kind === 'role') {
    roles.unshift({ id: `role_demo_${suffix}`, code: form.code || `platform:custom:${suffix}`, name: form.name || '未命名角色', type: form.type, application: form.application, memberCount: 0, permissionCount: 0, status: 'ACTIVE', defaultScope: form.defaultScope, permissions: [] })
  } else if (kind === 'binding') {
    bindings.unshift({ id: `rb_demo_${suffix}`, subjectType: form.subjectType, subject: form.subject || '待选择主体', role: form.role || '待选择角色', application: form.application, scope: form.scope, status: 'ACTIVE', effective: form.effective })
  } else if (kind === 'permission') {
    permissions.unshift({ id: `perm_demo_${suffix}`, code: form.code || `platform:resource:${form.action || 'read'}`, name: form.name || '未命名权限', application: form.application, resource: form.resource || 'resource', action: form.action || 'read', risk: form.risk, status: 'ACTIVE', description: '前端新增的权限注册示例，待权限注册 API 持久化。' })
  } else if (kind === 'identity' && context) {
    context.identities.push({ provider: form.provider, subject: form.subject || `${form.provider.toLowerCase()}_pending`, status: 'BOUND', boundAt: '2026-07-16 10:30' })
  }

  const labels = { user: '用户', account: '账号', organization: '组织单元', role: '角色', binding: '角色绑定', permission: '权限', identity: '外部身份绑定' }
  emitToast(`${labels[kind]}已在前端示例数据中创建；提交后需由 Go API 写入 MySQL，并生成审计事件。`)
  closeEditor()
}

function toggleStatus(kind, item) {
  if (kind === 'user') {
    item.status = item.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE'
  } else if (kind === 'account') {
    item.status = item.status === 'LOCKED' ? 'ACTIVE' : (item.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE')
    item.lockedUntil = item.status === 'ACTIVE' ? '—' : item.lockedUntil
  } else {
    item.status = item.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE'
  }
  emitToast('状态变更仅保存在当前前端会话；正式实现必须进行权限校验、乐观锁校验并写入审计。')
}

function unbindIdentity(account, identity) {
  const index = account.identities.indexOf(identity)
  if (index >= 0) account.identities.splice(index, 1)
  emitToast('外部身份已在前端解除绑定；正式操作应经二次确认并记录高风险审计事件。')
}

</script>

<template>
  <section class="iam-settings" aria-label="身份、组织与授权设置">

    <div class="iam-summary-grid">
      <article v-for="metric in metrics" :key="metric.label" class="iam-summary-card" :class="metric.tone">
        <span class="iam-summary-icon"><ConsoleIcon :name="metric.icon" /></span>
        <div><small>{{ metric.label }}</small><strong>{{ metric.value }}</strong><p>{{ metric.note }}</p></div>
      </article>
    </div>

    <div class="iam-workspace">
      <aside class="iam-panel-nav" aria-label="身份与授权功能导航">
        <button
          v-for="item in panels"
          :key="item.key"
          type="button"
          :class="{ active: activePanel === item.key }"
          @click="selectPanel(item.key)"
        >
          <ConsoleIcon :name="item.icon" />
          <span>{{ item.label }}</span>
        </button>
      </aside>

      <section class="iam-panel-content">
        <header class="iam-panel-head">
          <div><h3>{{ panel.label }}</h3><p>{{ panel.description }}</p></div>
          <div class="iam-panel-actions">
            <button class="console-button ghost small" type="button" @click="resetFilters"><ConsoleIcon name="reset" />清空筛选</button>
            <button class="console-button primary" type="button" @click="openEditor(activePanel === 'users' ? 'user' : activePanel === 'accounts' ? 'account' : activePanel === 'organizations' ? 'organization' : activePanel === 'roles' ? 'role' : activePanel === 'bindings' ? 'binding' : 'permission')">新增{{ activePanel === 'users' ? '用户' : activePanel === 'accounts' ? '账号' : activePanel === 'organizations' ? '组织' : activePanel === 'roles' ? '角色' : activePanel === 'bindings' ? '绑定' : '权限' }}</button>
          </div>
        </header>

        <section v-if="activePanel === 'users'" class="iam-table-section">
          <div class="iam-filter-row"><label class="console-search-field"><ConsoleIcon name="search" /><input v-model="filters.user" type="search" placeholder="姓名 / 工号 / 组织 / 账号 / 角色" /></label><span>展示 {{ filteredUsers.length }} / {{ users.length }} 位用户</span></div>
          <div class="console-table-card"><div class="console-table-scroll"><table class="console-data-table iam-data-table"><thead><tr><th>用户</th><th>主组织与任职</th><th>登录账号</th><th>角色</th><th>状态</th><th>最近活动</th><th class="console-actions-cell">操作</th></tr></thead><tbody>
            <tr v-for="item in filteredUsers" :key="item.id"><td><strong class="console-entity-name">{{ item.displayName }}</strong><span class="console-entity-meta console-mono">{{ item.employeeNo }}</span></td><td><strong>{{ item.primaryOrg }}</strong><span class="console-entity-meta">{{ item.membershipCount }} 条任职 · {{ displayEmployment(item.employmentStatus) }}</span></td><td><span v-for="account in item.accounts" :key="account" class="iam-inline-code">{{ account }}</span><span v-if="!item.accounts.length" class="console-entity-meta">暂无登录账号</span></td><td><span v-for="role in item.roles" :key="role" class="console-role-chip">{{ role }}</span></td><td><span class="console-badge" :class="item.status === 'ACTIVE' ? 'status-active' : 'status-disabled'">{{ displayStatus(item.status) }}</span></td><td class="console-mono">{{ item.lastActiveAt }}</td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="openDetail('user', item)">详情</button><button class="console-text-button" type="button" @click="toggleStatus('user', item)">{{ item.status === 'ACTIVE' ? '停用' : '启用' }}</button></td></tr>
            <tr v-if="!filteredUsers.length"><td class="console-empty" colspan="7">没有匹配的用户。</td></tr>
          </tbody></table></div></div>
          <p class="iam-footnote"><ConsoleIcon name="info" />用户离职或停用后保留历史用户、合同、审批与审计引用，不执行物理删除。</p>
        </section>

        <section v-else-if="activePanel === 'accounts'" class="iam-table-section">
          <div class="iam-filter-row"><label class="console-search-field"><ConsoleIcon name="search" /><input v-model="filters.account" type="search" placeholder="账号 / 用户 / 认证来源 / 状态" /></label><span>展示 {{ filteredAccounts.length }} / {{ accounts.length }} 个账号</span></div>
          <div class="console-table-card"><div class="console-table-scroll"><table class="console-data-table iam-data-table"><thead><tr><th>登录账号</th><th>关联用户</th><th>账号类型</th><th>认证来源</th><th>外部身份</th><th>状态</th><th>最近登录</th><th class="console-actions-cell">操作</th></tr></thead><tbody>
            <tr v-for="item in filteredAccounts" :key="item.id"><td><strong class="console-mono">{{ item.username }}</strong><span class="console-entity-meta">{{ item.id }}</span></td><td>{{ item.user }}</td><td><span class="iam-type-tag">{{ item.accountType === 'SERVICE' ? '服务账号' : '人类账号' }}</span></td><td><span class="iam-source-tag">{{ item.authSource }}</span></td><td><span v-if="item.identities.length" class="iam-identity-cell"><b>{{ item.identities[0].provider }}</b><small>{{ item.identities.length }} 个已绑定</small></span><span v-else class="console-entity-meta">未绑定</span></td><td><span class="console-badge" :class="item.status === 'ACTIVE' ? 'status-active' : item.status === 'LOCKED' ? 'iam-status-locked' : 'status-disabled'">{{ displayStatus(item.status) }}</span></td><td class="console-mono">{{ item.lastLoginAt }}</td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="openDetail('account', item)">详情</button><button class="console-text-button" type="button" @click="toggleStatus('account', item)">{{ item.status === 'LOCKED' ? '解锁' : item.status === 'ACTIVE' ? '停用' : '启用' }}</button></td></tr>
            <tr v-if="!filteredAccounts.length"><td class="console-empty" colspan="8">没有匹配的登录账号。</td></tr>
          </tbody></table></div></div>
          <p class="iam-footnote"><ConsoleIcon name="info" />外部身份由 IAM 管理；仅展示脱敏 subject，业务模块不持久化钉钉 unionid、企业微信身份等外部主键。</p>
        </section>

        <section v-else-if="activePanel === 'organizations'" class="iam-table-section">
          <div class="iam-filter-row"><label class="console-search-field"><ConsoleIcon name="search" /><input v-model="filters.organization" type="search" placeholder="组织名称 / 编码 / 上级组织 / 负责人" /></label><span>共 {{ filteredOrganizations.length }} 个组织单元</span></div>
          <div class="console-table-card"><div class="console-table-scroll"><table class="console-data-table iam-data-table"><thead><tr><th>组织结构</th><th>组织编码</th><th>类型</th><th>负责人</th><th>成员数</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead><tbody>
            <tr v-for="item in filteredOrganizations" :key="item.id"><td><span class="iam-tree-name" :style="{ paddingLeft: `${item.level * 24}px` }"><i v-if="item.level" />{{ item.name }}</span><span class="console-entity-meta" :style="{ paddingLeft: `${item.level * 24}px` }">上级：{{ item.parent }}</span></td><td class="console-mono">{{ item.code }}</td><td>{{ item.type === 'COMPANY' ? '主体' : item.type === 'TEAM' ? '团队' : '部门' }}</td><td>{{ item.manager }}</td><td>{{ item.memberCount }}</td><td><span class="console-badge status-active">启用</span></td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="openDetail('organization', item)">详情</button><button class="console-text-button" type="button" @click="emitToast('任职关系将通过 Membership 表维护，支持主组织、兼岗和历史任职。')">任职</button></td></tr>
          </tbody></table></div></div>
          <p class="iam-footnote"><ConsoleIcon name="info" />主组织是用户快照；真实组织关系以 Membership 为准，因此可表达兼岗、跨部门与历史任职。</p>
        </section>

        <section v-else-if="activePanel === 'roles'" class="iam-table-section">
          <div class="iam-filter-row"><label class="console-search-field"><ConsoleIcon name="search" /><input v-model="filters.role" type="search" placeholder="角色名称 / 编码 / 应用 / 权限" /></label><span>展示 {{ filteredRoles.length }} / {{ roles.length }} 个角色</span></div>
          <div class="console-table-card"><div class="console-table-scroll"><table class="console-data-table iam-data-table"><thead><tr><th>角色</th><th>角色类型</th><th>应用范围</th><th>默认数据范围</th><th>权限</th><th>已绑定主体</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead><tbody>
            <tr v-for="item in filteredRoles" :key="item.id"><td><strong class="console-entity-name">{{ item.name }}</strong><span class="console-entity-meta console-mono">{{ item.code }}</span></td><td><span class="console-role-type" :class="`role-${displayRoleType(item.type)}`">{{ displayRoleType(item.type) }}</span></td><td>{{ item.application }}</td><td>{{ displayScope(item.defaultScope) }}</td><td><span class="iam-permission-count">{{ item.permissionCount }} 项</span><span class="console-entity-meta">{{ item.permissions.slice(0, 2).join(' · ') }}{{ item.permissions.length > 2 ? ' …' : '' }}</span></td><td>{{ item.memberCount }}</td><td><span class="console-badge" :class="item.status === 'ACTIVE' ? 'status-active' : 'status-disabled'">{{ displayStatus(item.status) }}</span></td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="openDetail('role', item)">授权详情</button><button class="console-text-button" type="button" @click="toggleStatus('role', item)">{{ item.status === 'ACTIVE' ? '停用' : '启用' }}</button></td></tr>
            <tr v-if="!filteredRoles.length"><td class="console-empty" colspan="8">没有匹配的角色。</td></tr>
          </tbody></table></div></div>
          <p class="iam-footnote"><ConsoleIcon name="info" />角色表达权限集合；对某一具体主体的应用范围、数据范围和有效期，应配置在角色绑定中。</p>
        </section>

        <section v-else-if="activePanel === 'bindings'" class="iam-table-section">
          <div class="iam-filter-row"><label class="console-search-field"><ConsoleIcon name="search" /><input v-model="filters.binding" type="search" placeholder="主体 / 角色 / 应用 / 数据范围" /></label><span>展示 {{ filteredBindings.length }} / {{ bindings.length }} 条绑定</span></div>
          <div class="console-table-card"><div class="console-table-scroll"><table class="console-data-table iam-data-table"><thead><tr><th>授权主体</th><th>角色</th><th>应用范围</th><th>数据范围</th><th>有效期</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead><tbody>
            <tr v-for="item in filteredBindings" :key="item.id"><td><strong>{{ item.subject }}</strong><span class="console-entity-meta">{{ displaySubjectType(item.subjectType) }}</span></td><td>{{ item.role }}</td><td>{{ item.application }}</td><td><span class="iam-scope-tag">{{ displayScope(item.scope) }}</span></td><td class="console-mono">{{ item.effective }}</td><td><span class="console-badge" :class="item.status === 'ACTIVE' ? 'status-active' : 'status-disabled'">{{ displayStatus(item.status) }}</span></td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="openDetail('binding', item)">详情</button><button class="console-text-button" type="button" @click="toggleStatus('binding', item)">{{ item.status === 'ACTIVE' ? '停用' : '启用' }}</button></td></tr>
            <tr v-if="!filteredBindings.length"><td class="console-empty" colspan="7">没有匹配的角色绑定。</td></tr>
          </tbody></table></div></div>
          <p class="iam-footnote"><ConsoleIcon name="info" />支持用户、组织、岗位、用户组、服务账号等主体类型；授权检查需综合范围、有效期与数据策略决策。</p>
        </section>

        <section v-else class="iam-table-section">
          <div class="iam-filter-row"><label class="console-search-field"><ConsoleIcon name="search" /><input v-model="filters.permission" type="search" placeholder="权限编码 / 名称 / 应用 / 资源 / 动作" /></label><span>展示 {{ filteredPermissions.length }} / {{ permissions.length }} 项权限</span></div>
          <div class="console-table-card"><div class="console-table-scroll"><table class="console-data-table iam-data-table"><thead><tr><th>权限编码</th><th>权限名称</th><th>应用</th><th>资源 / 动作</th><th>风险等级</th><th>状态</th><th class="console-actions-cell">操作</th></tr></thead><tbody>
            <tr v-for="item in filteredPermissions" :key="item.id"><td><code class="iam-permission-code">{{ item.code }}</code></td><td><strong>{{ item.name }}</strong><span class="console-entity-meta">{{ item.description }}</span></td><td>{{ item.application }}</td><td><span class="iam-inline-code">{{ item.resource }}</span><span class="iam-arrow">/</span><span class="iam-inline-code">{{ item.action }}</span></td><td><span class="iam-risk" :class="item.risk.toLowerCase()">{{ item.risk === 'HIGH' ? '高' : item.risk === 'MEDIUM' ? '中' : '低' }}</span></td><td><span class="console-badge status-active">启用</span></td><td class="console-actions-cell"><button class="console-text-button" type="button" @click="openDetail('permission', item)">详情</button></td></tr>
            <tr v-if="!filteredPermissions.length"><td class="console-empty" colspan="7">没有匹配的权限。</td></tr>
          </tbody></table></div></div>
          <p class="iam-footnote"><ConsoleIcon name="info" />权限表示业务原子能力，不以 URL 作为权限编码；多个 API 可映射到同一权限。</p>
        </section>
      </section>
    </div>

    <div v-if="detail" class="iam-modal-backdrop" role="presentation" @click.self="closeDetail">
      <section class="iam-modal" role="dialog" aria-modal="true" aria-label="身份授权详情">
        <header><div><p>{{ detail.kind === 'user' ? '用户与任职' : detail.kind === 'account' ? '登录账号与外部身份' : detail.kind === 'organization' ? '组织单元' : detail.kind === 'role' ? '角色与权限' : detail.kind === 'binding' ? '角色绑定' : '权限注册' }}</p><h3>{{ detail.kind === 'user' ? detail.item.displayName : detail.kind === 'account' ? detail.item.username : detail.kind === 'organization' ? detail.item.name : detail.kind === 'role' ? detail.item.name : detail.kind === 'binding' ? detail.item.subject : detail.item.name }}</h3></div><button class="console-modal-close" type="button" aria-label="关闭详情" @click="closeDetail"><ConsoleIcon name="close" /></button></header>

        <template v-if="detail.kind === 'user'"><div class="iam-detail-grid"><div><span>统一用户 ID</span><strong class="console-mono">{{ detail.item.id }}</strong></div><div><span>员工编号</span><strong>{{ detail.item.employeeNo }}</strong></div><div><span>主组织快照</span><strong>{{ detail.item.primaryOrg }}</strong></div><div><span>任职状态</span><strong>{{ displayEmployment(detail.item.employmentStatus) }}</strong></div><div><span>用户状态</span><strong>{{ displayStatus(detail.item.status) }}</strong></div><div><span>关联账号</span><strong>{{ detail.item.accountCount }} 个</strong></div></div><section class="iam-detail-section"><h4>Membership 任职关系</h4><table class="iam-mini-table"><thead><tr><th>组织</th><th>关系</th><th>岗位</th><th>有效期</th></tr></thead><tbody><tr v-for="membership in detail.item.memberships" :key="`${membership.organization}-${membership.position}`"><td>{{ membership.organization }}</td><td>{{ membership.type === 'PRIMARY' ? '主组织' : '兼岗' }}</td><td>{{ membership.position }}</td><td>{{ membership.startedAt }} 至 {{ membership.endedAt }}</td></tr></tbody></table></section><section class="iam-detail-section"><h4>角色摘要</h4><span v-for="role in detail.item.roles" :key="role" class="console-role-chip large">{{ role }}</span></section></template>

        <template v-else-if="detail.kind === 'account'"><div class="iam-detail-grid"><div><span>账号 ID</span><strong class="console-mono">{{ detail.item.id }}</strong></div><div><span>关联用户</span><strong>{{ detail.item.user }}</strong></div><div><span>账号类型</span><strong>{{ detail.item.accountType === 'SERVICE' ? '服务账号' : '人类账号' }}</strong></div><div><span>认证来源</span><strong>{{ detail.item.authSource }}</strong></div><div><span>账号状态</span><strong>{{ displayStatus(detail.item.status) }}</strong></div><div><span>最近登录</span><strong class="console-mono">{{ detail.item.lastLoginAt }}</strong></div></div><section class="iam-detail-section"><div class="iam-detail-section-head"><div><h4>ExternalIdentity 外部身份</h4><p>外部身份 subject 只归属 IAM，绑定/解绑属于高风险操作。</p></div><button class="console-button primary small" type="button" @click="openEditor('identity', detail.item)">绑定身份</button></div><div v-if="detail.item.identities.length" class="iam-identity-list"><div v-for="identity in detail.item.identities" :key="`${identity.provider}-${identity.subject}`"><span><b>{{ identity.provider }}</b><small class="console-mono">{{ identity.subject }} · {{ identity.boundAt }}</small></span><button class="console-text-button" type="button" @click="unbindIdentity(detail.item, identity)">解绑</button></div></div><p v-else class="iam-empty-inline">当前账号未绑定外部身份。</p></section></template>

        <template v-else-if="detail.kind === 'organization'"><div class="iam-detail-grid"><div><span>组织 ID</span><strong class="console-mono">{{ detail.item.id }}</strong></div><div><span>组织编码</span><strong class="console-mono">{{ detail.item.code }}</strong></div><div><span>组织类型</span><strong>{{ detail.item.type }}</strong></div><div><span>上级组织</span><strong>{{ detail.item.parent }}</strong></div><div><span>负责人</span><strong>{{ detail.item.manager }}</strong></div><div><span>有效成员</span><strong>{{ detail.item.memberCount }} 人</strong></div></div><section class="iam-detail-section"><h4>组织关系说明</h4><p>组织单元只描述结构；用户任职关系通过 Membership 表维护，从而支持主组织、兼岗和历史任职。</p></section></template>

        <template v-else-if="detail.kind === 'role'"><div class="iam-detail-grid"><div><span>角色编码</span><strong class="console-mono">{{ detail.item.code }}</strong></div><div><span>角色类型</span><strong>{{ displayRoleType(detail.item.type) }}</strong></div><div><span>应用范围</span><strong>{{ detail.item.application }}</strong></div><div><span>默认数据范围</span><strong>{{ displayScope(detail.item.defaultScope) }}</strong></div><div><span>已绑定主体</span><strong>{{ detail.item.memberCount }} 个</strong></div><div><span>状态</span><strong>{{ displayStatus(detail.item.status) }}</strong></div></div><section class="iam-detail-section"><h4>权限清单</h4><p>角色维护的是权限集合，最终数据范围需由 RoleBinding 和 DataPolicy 合并决定。</p><code v-for="permission in detail.item.permissions" :key="permission" class="iam-permission-code">{{ permission }}</code></section></template>

        <template v-else-if="detail.kind === 'binding'"><div class="iam-detail-grid"><div><span>绑定 ID</span><strong class="console-mono">{{ detail.item.id }}</strong></div><div><span>主体类型</span><strong>{{ displaySubjectType(detail.item.subjectType) }}</strong></div><div><span>授权主体</span><strong>{{ detail.item.subject }}</strong></div><div><span>角色</span><strong>{{ detail.item.role }}</strong></div><div><span>应用范围</span><strong>{{ detail.item.application }}</strong></div><div><span>数据范围</span><strong>{{ displayScope(detail.item.scope) }}</strong></div><div><span>有效期</span><strong>{{ detail.item.effective }}</strong></div><div><span>状态</span><strong>{{ displayStatus(detail.item.status) }}</strong></div></div><section class="iam-detail-section"><h4>决策提示</h4><p>授权接口会校验绑定的主体、角色、应用范围、有效期与数据范围；高风险操作在授权服务不可用时必须拒绝。</p></section></template>

        <template v-else><div class="iam-detail-grid"><div><span>权限编码</span><strong class="console-mono">{{ detail.item.code }}</strong></div><div><span>权限名称</span><strong>{{ detail.item.name }}</strong></div><div><span>所属应用</span><strong>{{ detail.item.application }}</strong></div><div><span>资源 / 动作</span><strong>{{ detail.item.resource }} / {{ detail.item.action }}</strong></div><div><span>风险等级</span><strong>{{ detail.item.risk }}</strong></div><div><span>状态</span><strong>{{ displayStatus(detail.item.status) }}</strong></div></div><section class="iam-detail-section"><h4>权限定义</h4><p>{{ detail.item.description }}</p></section></template>
        <footer><button class="console-button ghost" type="button" @click="closeDetail">关闭</button></footer>
      </section>
    </div>

    <div v-if="editor" class="iam-modal-backdrop" role="presentation" @click.self="closeEditor">
      <section class="iam-modal iam-editor-modal" role="dialog" aria-modal="true" aria-label="新增身份授权配置">
        <header><div><p>前端模拟表单</p><h3>新增{{ { user: '用户', account: '登录账号', organization: '组织单元', role: '角色', binding: '角色绑定', permission: '权限注册', identity: '外部身份绑定' }[editor.kind] }}</h3></div><button class="console-modal-close" type="button" aria-label="关闭表单" @click="closeEditor"><ConsoleIcon name="close" /></button></header>
        <form class="iam-editor-form" @submit.prevent="saveEditor">
          <template v-if="editor.kind === 'user'"><label><span>展示姓名</span><input v-model="form.displayName" required placeholder="例如：张三" /></label><label><span>员工编号</span><input v-model="form.employeeNo" placeholder="例如：BP-0100" /></label><label><span>主组织</span><select v-model="form.primaryOrg"><option v-for="item in organizations.filter((org) => org.status === 'ACTIVE')" :key="item.id">{{ item.name }}</option></select></label><label><span>任职状态</span><select v-model="form.employmentStatus"><option value="ACTIVE">在职</option><option value="ON_LEAVE">请假中</option></select></label></template>
          <template v-else-if="editor.kind === 'account'"><label><span>登录账号</span><input v-model="form.username" required placeholder="例如：zhang.san" /></label><label><span>关联用户</span><select v-model="form.user"><option value="">不关联用户（服务账号）</option><option v-for="item in users" :key="item.id">{{ item.displayName }}</option></select></label><label><span>账号类型</span><select v-model="form.accountType"><option value="HUMAN">人类账号</option><option value="SERVICE">服务账号</option></select></label><label><span>认证来源</span><select v-model="form.authSource"><option>LOCAL</option><option>OIDC</option><option>LDAP</option><option>DINGTALK</option><option>WECOM</option></select></label></template>
          <template v-else-if="editor.kind === 'organization'"><label><span>组织名称</span><input v-model="form.name" required /></label><label><span>组织编码</span><input v-model="form.code" required /></label><label><span>组织类型</span><select v-model="form.type"><option value="DEPARTMENT">部门</option><option value="TEAM">团队</option></select></label><label><span>上级组织</span><select v-model="form.parent"><option v-for="item in organizations" :key="item.id">{{ item.name }}</option></select></label><label class="full"><span>负责人</span><input v-model="form.manager" placeholder="可在后端选择统一用户" /></label></template>
          <template v-else-if="editor.kind === 'role'"><label><span>角色名称</span><input v-model="form.name" required /></label><label><span>角色编码</span><input v-model="form.code" required placeholder="application:resource:role" /></label><label><span>角色类型</span><select v-model="form.type"><option value="PLATFORM">平台角色</option><option value="APPLICATION">应用角色</option><option value="CUSTOM">自定义角色</option></select></label><label><span>应用范围</span><select v-model="form.application"><option>基础能力平台</option><option>合同管理</option><option>项目管理（待接入）</option></select></label><label class="full"><span>默认数据范围</span><select v-model="form.defaultScope"><option value="all">全部数据</option><option value="self">仅本人</option><option value="org">所属组织</option><option value="org_tree">所属组织及下级</option><option value="custom_org">指定组织</option><option value="participant">参与人相关</option></select></label></template>
          <template v-else-if="editor.kind === 'binding'"><label><span>主体类型</span><select v-model="form.subjectType"><option value="USER">用户</option><option value="ORG_UNIT">组织</option><option value="POSITION">岗位</option><option value="GROUP">用户组</option><option value="SERVICE_ACCOUNT">服务账号</option></select></label><label><span>授权主体</span><input v-model="form.subject" required placeholder="例如：王敏 / 项目管理部" /></label><label><span>角色</span><select v-model="form.role"><option value="">请选择角色</option><option v-for="item in roles" :key="item.id">{{ item.name }}</option></select></label><label><span>应用范围</span><select v-model="form.application"><option>基础能力平台</option><option>合同管理</option><option>项目管理（待接入）</option></select></label><label><span>数据范围</span><select v-model="form.scope"><option value="all">全部数据</option><option value="self">仅本人</option><option value="org">所属组织</option><option value="org_tree">所属组织及下级</option><option value="custom_org">指定组织</option><option value="participant">参与人相关</option></select></label><label><span>有效期</span><input v-model="form.effective" /></label></template>
          <template v-else-if="editor.kind === 'permission'"><label><span>权限名称</span><input v-model="form.name" required /></label><label><span>权限编码</span><input v-model="form.code" required placeholder="application:resource:action" /></label><label><span>所属应用</span><select v-model="form.application"><option>基础能力平台</option><option>合同管理</option><option>项目管理（待接入）</option></select></label><label><span>资源</span><input v-model="form.resource" required placeholder="例如：contract" /></label><label><span>动作</span><input v-model="form.action" required placeholder="例如：approve" /></label><label><span>风险等级</span><select v-model="form.risk"><option value="LOW">低</option><option value="MEDIUM">中</option><option value="HIGH">高</option></select></label></template>
          <template v-else><label><span>身份提供方</span><select v-model="form.provider"><option>DINGTALK</option><option>WECOM</option><option>OIDC</option><option>LDAP</option></select></label><label><span>外部身份 subject</span><input v-model="form.subject" required placeholder="由统一身份服务返回的稳定标识" /></label><p class="iam-form-alert"><ConsoleIcon name="info" />正式接入应使用授权回调确认身份归属，不允许手工伪造外部 subject。</p></template>
          <p class="iam-form-alert"><ConsoleIcon name="info" />此表单仅模拟前端交互；生产环境需由 Go API 执行输入校验、权限校验、乐观锁及审计出站记录。</p>
          <footer><button class="console-button ghost" type="button" @click="closeEditor">取消</button><button class="console-button primary" type="submit"><ConsoleIcon name="save" />保存（前端模拟）</button></footer>
        </form>
      </section>
    </div>
  </section>
</template>
