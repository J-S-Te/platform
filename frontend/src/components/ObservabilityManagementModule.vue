<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import {
  ObservabilityError,
  createAlertRule,
  executeAlertRule,
  listAlertRules,
  listMetricPoints,
  listRuntimeLogs,
  listTraceSpans,
  updateAlertRule,
} from '@/api/observability'
import '@/styles/observability-management.css'

const emit = defineEmits(['toast'])

const activePanel = ref('logs')
const loading = ref(false)
const savingRule = ref(false)
const errorMessage = ref('')
const runtimeItems = ref([])
const alertRules = ref([])
const editingRule = ref(null)
const editorOpen = ref(false)

const query = reactive({
  application_id: '',
  trace_id: '',
  request_id: '',
  name: '',
  severity: '',
  keyword: '',
  from: '',
  to: '',
  limit: 100,
})

const ruleForm = reactive(emptyRuleForm())

const panelDescription = computed(() => ({
  logs: '查看经脱敏处理的结构化运行日志；浏览器不会提交租户、请求正文或凭据。',
  traces: '按 Trace ID 或 Request ID 定位受当前租户权限保护的服务端调用链。',
  metrics: '查看短期指标观测值，用于规则校验与异常诊断。',
  alerts: '管理阈值告警规则。触发、恢复和受控重复提醒仅通过站内信投递。',
}[activePanel.value]))

function emptyRuleForm() {
  return {
    application_id: '',
    name: '',
    metric_name: 'http.server.errors',
    comparator: 'GTE',
    severity: 'HIGH',
    status: 'ENABLED',
    threshold: 1,
    window_seconds: 60,
    version: 0,
  }
}

function normaliseItems(data) {
  if (Array.isArray(data)) return data
  return Array.isArray(data?.items) ? data.items : []
}

function showToast(type, message) {
  emit('toast', { type, message })
}

function visibleValue(value) {
  if (value === null || value === undefined || value === '') return '—'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

function formatTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

function resetRuntimeItems() {
  runtimeItems.value = []
}

function resetRuleForm() {
  Object.assign(ruleForm, emptyRuleForm())
  editingRule.value = null
}

function selectPanel(panel) {
  activePanel.value = panel
  errorMessage.value = ''
  resetRuntimeItems()
  if (panel === 'alerts') loadAlertRules()
}

async function loadRuntimeData() {
  loading.value = true
  errorMessage.value = ''
  try {
    const baseQuery = {
      application_id: query.application_id,
      trace_id: query.trace_id,
      request_id: query.request_id,
      from: query.from ? new Date(query.from).toISOString() : '',
      to: query.to ? new Date(query.to).toISOString() : '',
      limit: query.limit,
    }
    let data
    if (activePanel.value === 'logs') {
      data = await listRuntimeLogs({ ...baseQuery, severity: query.severity, keyword: query.keyword })
    } else if (activePanel.value === 'traces') {
      data = await listTraceSpans(baseQuery)
    } else {
      data = await listMetricPoints({ ...baseQuery, name: query.name })
    }
    runtimeItems.value = normaliseItems(data)
  } catch (error) {
    errorMessage.value = userFacingError(error)
  } finally {
    loading.value = false
  }
}

async function loadAlertRules() {
  loading.value = true
  errorMessage.value = ''
  try {
    alertRules.value = normaliseItems(await listAlertRules())
  } catch (error) {
    errorMessage.value = userFacingError(error)
  } finally {
    loading.value = false
  }
}

function openCreateRule() {
  resetRuleForm()
  errorMessage.value = ''
  editorOpen.value = true
}

function openEditRule(rule) {
  editingRule.value = rule
  Object.assign(ruleForm, {
    application_id: rule.application_id,
    name: rule.name,
    metric_name: rule.metric_name,
    comparator: rule.comparator,
    severity: rule.severity,
    status: rule.status,
    threshold: rule.threshold,
    window_seconds: rule.window_seconds,
    version: rule.version,
  })
  errorMessage.value = ''
  editorOpen.value = true
}

function closeRuleEditor() {
  if (savingRule.value) return
  editorOpen.value = false
  resetRuleForm()
}

async function saveRule() {
  if (savingRule.value) return
  savingRule.value = true
  errorMessage.value = ''
  const payload = {
    application_id: ruleForm.application_id.trim(),
    name: ruleForm.name.trim(),
    metric_name: ruleForm.metric_name.trim(),
    comparator: ruleForm.comparator,
    severity: ruleForm.severity,
    status: ruleForm.status,
    threshold: Number(ruleForm.threshold),
    window_seconds: Number(ruleForm.window_seconds),
    version: Number(ruleForm.version),
  }
  try {
    if (editingRule.value) {
      await updateAlertRule(editingRule.value.rule_id, payload)
      showToast('success', '告警规则已更新。')
    } else {
      await createAlertRule(payload)
      showToast('success', '告警规则已创建。')
    }
    closeRuleEditor()
    await loadAlertRules()
  } catch (error) {
    errorMessage.value = userFacingError(error)
  } finally {
    savingRule.value = false
  }
}

async function runRule(rule) {
  try {
    const data = await executeAlertRule(rule.rule_id)
    const state = data?.execution?.evaluation?.state || '已完成'
    showToast('success', `规则执行完成：${state}`)
    await loadAlertRules()
  } catch (error) {
    showToast('error', userFacingError(error))
  }
}

function userFacingError(error) {
  if (error instanceof ObservabilityError && error.code === 'PLATFORM_NOT_FOUND') {
    return '可观测性接口尚未接入共享路由或当前环境未启用，请完成服务端集成后重试。'
  }
  return error?.message || '可观测性请求失败，请稍后重试。'
}

onMounted(loadAlertRules)
</script>

<template>
  <section class="observability-management" aria-labelledby="observability-management-heading">
    <header class="observability-management__header">
      <div>
        <span class="observability-management__eyebrow">RUNTIME OBSERVABILITY</span>
        <h2 id="observability-management-heading">可观测性管理</h2>
        <p>统一查看运行日志、Trace、Metric 与指标阈值告警；运行数据按租户隔离且只保留短期诊断窗口。</p>
      </div>
      <button class="console-button ghost" type="button" :disabled="loading" @click="activePanel === 'alerts' ? loadAlertRules() : loadRuntimeData()">
        刷新当前视图
      </button>
    </header>

    <nav class="observability-management__tabs" aria-label="可观测性分类">
      <button v-for="panel in ['logs', 'traces', 'metrics', 'alerts']" :key="panel" type="button" :class="{ active: activePanel === panel }" @click="selectPanel(panel)">
        {{ { logs: '运行日志', traces: '链路追踪', metrics: '指标', alerts: '告警规则' }[panel] }}
      </button>
    </nav>

    <p v-if="errorMessage" class="observability-management__error">{{ errorMessage }}</p>

    <section v-if="activePanel !== 'alerts'" class="observability-management__panel">
      <header class="observability-management__panel-head"><p>{{ panelDescription }}</p></header>
      <form class="observability-management__filters" @submit.prevent="loadRuntimeData">
        <label>应用标识<input v-model="query.application_id" placeholder="可选，例如 console" maxlength="64"></label>
        <label v-if="activePanel !== 'metrics'">Trace ID<input v-model="query.trace_id" placeholder="可选" maxlength="64"></label>
        <label v-if="activePanel !== 'metrics'">Request ID<input v-model="query.request_id" placeholder="可选" maxlength="128"></label>
        <label v-if="activePanel === 'logs'">级别<select v-model="query.severity"><option value="">全部</option><option>DEBUG</option><option>INFO</option><option>WARN</option><option>ERROR</option></select></label>
        <label v-if="activePanel === 'logs'">关键字<input v-model="query.keyword" placeholder="消息、模块或错误码" maxlength="100"></label>
        <label v-if="activePanel === 'metrics'">指标名<input v-model="query.name" placeholder="例如 http.server.duration" maxlength="128"></label>
        <label>开始时间<input v-model="query.from" type="datetime-local"></label>
        <label>结束时间<input v-model="query.to" type="datetime-local"></label>
        <label>数量上限<input v-model.number="query.limit" type="number" min="1" max="1000"></label>
        <button class="console-button primary" type="submit" :disabled="loading">{{ loading ? '查询中…' : '查询' }}</button>
      </form>

      <div v-if="loading" class="observability-management__empty">正在读取运行数据…</div>
      <div v-else-if="!runtimeItems.length" class="observability-management__empty">尚无可展示数据。运行数据采集完成并接入查询路由后会显示在这里。</div>
      <div v-else class="observability-management__table-wrap">
        <table class="observability-management__table">
          <thead><tr><th>时间</th><th>{{ activePanel === 'logs' ? '事件' : activePanel === 'traces' ? '调用链' : '指标' }}</th><th>状态 / 数值</th><th>应用</th><th>关联信息</th></tr></thead>
          <tbody>
            <tr v-for="item in runtimeItems" :key="item.span_id || `${item.timestamp}-${item.name || item.message}`">
              <td>{{ formatTime(item.timestamp || item.started_at) }}</td>
              <td><strong>{{ item.message || item.name }}</strong><small>{{ item.operation || item.kind || item.unit || '—' }}</small></td>
              <td>{{ visibleValue(item.severity || item.status_code || item.value) }}</td>
              <td>{{ item.resource?.application_id || item.application || '—' }}</td>
              <td><code>{{ item.trace_id || item.request_id || '—' }}</code></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section v-else class="observability-management__panel">
      <header class="observability-management__panel-head">
        <p>{{ panelDescription }}</p>
        <button class="console-button primary" type="button" @click="openCreateRule">新建告警规则</button>
      </header>
      <div v-if="loading" class="observability-management__empty">正在读取告警规则…</div>
      <div v-else-if="!alertRules.length" class="observability-management__empty">暂无告警规则。</div>
      <div v-else class="observability-management__table-wrap">
        <table class="observability-management__table">
          <thead><tr><th>规则</th><th>指标 / 阈值</th><th>级别</th><th>运行状态</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="rule in alertRules" :key="rule.rule_id">
              <td><strong>{{ rule.name }}</strong><small>{{ rule.application_id }}</small></td>
              <td><code>{{ rule.metric_name }} {{ rule.comparator }} {{ rule.threshold }}</code><small>{{ rule.window_seconds }} 秒窗口</small></td>
              <td><span class="observability-management__badge" :class="rule.severity?.toLowerCase()">{{ rule.severity }}</span></td>
              <td><span class="observability-management__badge" :class="rule.last_state?.toLowerCase()">{{ rule.status }} · {{ rule.last_state }}</span></td>
              <td class="observability-management__actions"><button type="button" @click="openEditRule(rule)">编辑</button><button type="button" @click="runRule(rule)">执行</button></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <div v-if="editorOpen" class="observability-management__dialog-backdrop" @click.self="closeRuleEditor">
      <form class="observability-management__dialog" @submit.prevent="saveRule">
        <header><div><span>ALERT RULE</span><h3>{{ editingRule ? '编辑告警规则' : '新建告警规则' }}</h3></div><button type="button" aria-label="关闭" @click="closeRuleEditor">×</button></header>
        <p>触发、恢复和受控重复提醒只会写入站内信投递链路，不发送邮件或短信。</p>
        <div class="observability-management__form-grid">
          <label>应用标识<input v-model="ruleForm.application_id" required maxlength="64"></label>
          <label>规则名称<input v-model="ruleForm.name" required maxlength="120"></label>
          <label>指标名称<input v-model="ruleForm.metric_name" required maxlength="128"></label>
          <label>比较方式<select v-model="ruleForm.comparator"><option>GT</option><option>GTE</option><option>LT</option><option>LTE</option></select></label>
          <label>阈值<input v-model.number="ruleForm.threshold" required type="number" step="any"></label>
          <label>时间窗口（秒）<input v-model.number="ruleForm.window_seconds" required type="number" min="10" max="86400"></label>
          <label>告警级别<select v-model="ruleForm.severity"><option>LOW</option><option>MEDIUM</option><option>HIGH</option><option>CRITICAL</option></select></label>
          <label>状态<select v-model="ruleForm.status"><option>ENABLED</option><option>DISABLED</option></select></label>
        </div>
        <footer><button class="console-button ghost" type="button" :disabled="savingRule" @click="closeRuleEditor">取消</button><button class="console-button primary" type="submit" :disabled="savingRule">{{ savingRule ? '保存中…' : '保存规则' }}</button></footer>
      </form>
    </div>
  </section>
</template>
