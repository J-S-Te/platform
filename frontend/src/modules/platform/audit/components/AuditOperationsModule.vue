<script setup>
import { computed, onMounted, ref } from 'vue'
import {
  AuditOperationsError,
  createAuditRetentionTask,
  getAuditDeadLetterStatus,
  listAuditDeadLetters,
  listAuditIngestionReceipts,
  listAuditRetentionTasks,
  replayAuditDeadLetter,
  replayAuditDeadLetters,
} from '@/modules/platform/audit/api/auditOperations'
import ConsoleIcon from '@/modules/platform/shared/components/ConsoleIcon.vue'
import '@/modules/platform/audit/styles/audit-operations.css'

const emit = defineEmits(['toast'])
const activePanel = ref('receipts')
const loading = ref(false)
const errorMessage = ref('')
const receipts = ref([])
const deadLetters = ref([])
const retentionTasks = ref([])
const deadLetterStatus = ref({ pending: 0, replayed: 0, ignored: 0 })
const receiptFilters = ref({ applicationCode: '', environmentCode: '', status: '' })
const deadLetterFilters = ref({ applicationCode: '', status: 'PENDING' })
const retentionFilters = ref({ applicationId: '', mode: '', status: '' })
const retentionForm = ref({ applicationId: '', mode: 'ARCHIVE', archiveId: '', cutoffAt: '' })
const submittingRetention = ref(false)
const selectedDeadLetters = ref(new Set())

const selectedCount = computed(() => selectedDeadLetters.value.size)
const hasSelectedDeadLetters = computed(() => selectedCount.value > 0)

function dateTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

function showError(error, fallback) {
  errorMessage.value = error instanceof AuditOperationsError ? error.message : fallback
}

async function refreshReceipts() {
  loading.value = true
  errorMessage.value = ''
  try {
    const data = await listAuditIngestionReceipts(receiptFilters.value)
    receipts.value = data?.items || []
  } catch (error) {
    showError(error, '读取审计接收回执失败。')
  } finally {
    loading.value = false
  }
}

async function refreshDeadLetters() {
  loading.value = true
  errorMessage.value = ''
  try {
    const [list, status] = await Promise.all([
      listAuditDeadLetters(deadLetterFilters.value),
      getAuditDeadLetterStatus(deadLetterFilters.value.applicationCode),
    ])
    deadLetters.value = list?.items || []
    deadLetterStatus.value = status || { pending: 0, replayed: 0, ignored: 0 }
    selectedDeadLetters.value = new Set()
  } catch (error) {
    showError(error, '读取审计死信失败。')
  } finally {
    loading.value = false
  }
}

async function refreshRetentionTasks() {
  loading.value = true
  errorMessage.value = ''
  try {
    const data = await listAuditRetentionTasks(retentionFilters.value)
    retentionTasks.value = data?.items || []
  } catch (error) {
    showError(error, '读取审计保留任务失败。')
  } finally {
    loading.value = false
  }
}

async function refreshActivePanel() {
  if (activePanel.value === 'receipts') return refreshReceipts()
  if (activePanel.value === 'dead-letters') return refreshDeadLetters()
  return refreshRetentionTasks()
}

function toggleSelection(deadLetterID, checked) {
  const next = new Set(selectedDeadLetters.value)
  if (checked) next.add(deadLetterID)
  else next.delete(deadLetterID)
  selectedDeadLetters.value = next
}

async function replayOne(deadLetterID) {
  try {
    const result = await replayAuditDeadLetter(deadLetterID)
    emit('toast', { type: result.error_code ? 'error' : 'success', message: result.error_code ? `重放未完成：${result.error_code}` : '审计死信已重放。' })
    await refreshDeadLetters()
  } catch (error) {
    showError(error, '重放审计死信失败。')
  }
}

async function replaySelected() {
  const deadLetterIDs = [...selectedDeadLetters.value]
  if (!deadLetterIDs.length) return

  try {
    const results = await replayAuditDeadLetters(deadLetterIDs)
    const failedCount = results.filter((item) => item.error_code).length
    emit('toast', {
      type: failedCount ? 'error' : 'success',
      message: failedCount ? `${failedCount} 条审计死信未能完成重放。` : `${results.length} 条审计死信已重放。`,
    })
    await refreshDeadLetters()
  } catch (error) {
    showError(error, '批量重放审计死信失败。')
  }
}

async function submitRetentionTask() {
  if (submittingRetention.value) return
  submittingRetention.value = true
  errorMessage.value = ''
  try {
    const payload = { ...retentionForm.value, cutoffAt: retentionForm.value.cutoffAt ? new Date(retentionForm.value.cutoffAt).toISOString() : '' }
    await createAuditRetentionTask(payload)
    emit('toast', { type: 'success', message: '审计保留任务已创建，将由受控 Worker 异步执行。' })
    retentionForm.value.archiveId = ''
    await refreshRetentionTasks()
  } catch (error) {
    showError(error, '创建审计保留任务失败。')
  } finally {
    submittingRetention.value = false
  }
}

onMounted(refreshReceipts)
</script>

<template>
  <section class="audit-operations" aria-labelledby="audit-operations-heading">
    <header class="audit-operations__header">
      <div>
        <span class="audit-operations__eyebrow">AUDIT OPERATIONS</span>
        <h2 id="audit-operations-heading">审计运营</h2>
        <p>查询接收回执，运营平台侧死信，并通过受控异步任务归档或清理审计记录。</p>
      </div>
      <button class="console-button ghost" type="button" :disabled="loading" @click="refreshActivePanel"><ConsoleIcon name="refresh" /> 刷新</button>
    </header>

    <nav class="audit-operations__tabs" aria-label="审计运营功能">
      <button :class="{ active: activePanel === 'receipts' }" type="button" @click="activePanel = 'receipts'; refreshReceipts()">接收回执</button>
      <button :class="{ active: activePanel === 'dead-letters' }" type="button" @click="activePanel = 'dead-letters'; refreshDeadLetters()">上报死信</button>
      <button :class="{ active: activePanel === 'retention' }" type="button" @click="activePanel = 'retention'; refreshRetentionTasks()">归档与保留</button>
    </nav>

    <p v-if="errorMessage" class="audit-operations__error" role="alert">{{ errorMessage }}</p>

    <template v-if="activePanel === 'receipts'">
      <div class="audit-operations__toolbar">
        <label><span>应用编码</span><input v-model.trim="receiptFilters.applicationCode" placeholder="例如 contract" @keyup.enter="refreshReceipts" /></label>
        <label><span>环境</span><input v-model.trim="receiptFilters.environmentCode" placeholder="例如 production" @keyup.enter="refreshReceipts" /></label>
        <label><span>状态</span><input v-model.trim="receiptFilters.status" placeholder="例如 ACCEPTED" @keyup.enter="refreshReceipts" /></label>
        <button class="console-button primary small" type="button" @click="refreshReceipts">查询</button>
      </div>
      <div class="audit-operations__notice"><ConsoleIcon name="shield" /><span>回执仅记录平台侧接收结果和关联标识；不会展示审计事件正文或客户端凭据。</span></div>
      <div class="console-table-card audit-operations__table-card"><div class="console-table-scroll"><table class="console-data-table audit-operations__table"><thead><tr><th>接收时间</th><th>应用 / 环境</th><th>接收数量</th><th>接受</th><th>重复</th><th>状态</th><th>Request ID</th><th>关联 ID</th></tr></thead><tbody><tr v-for="item in receipts" :key="item.id"><td class="console-mono">{{ dateTime(item.received_at) }}</td><td><strong>{{ item.application_code }}</strong><small>{{ item.environment_code }}</small></td><td>{{ item.event_count }}</td><td>{{ item.accepted_count }}</td><td>{{ item.duplicate_count }}</td><td><span class="audit-operations__status success">{{ item.status }}</span></td><td class="console-mono">{{ item.request_id || '—' }}</td><td class="console-mono">{{ item.correlation_id || '—' }}</td></tr><tr v-if="!loading && !receipts.length"><td colspan="8" class="audit-operations__empty">暂无接收回执。</td></tr></tbody></table></div></div>
    </template>

    <template v-else-if="activePanel === 'dead-letters'">
      <div class="audit-operations__summary"><article><span>待处理</span><strong>{{ deadLetterStatus.pending || 0 }}</strong></article><article><span>已重放</span><strong>{{ deadLetterStatus.replayed || 0 }}</strong></article><article><span>已忽略</span><strong>{{ deadLetterStatus.ignored || 0 }}</strong></article></div>
      <div class="audit-operations__toolbar"><label><span>应用编码</span><input v-model.trim="deadLetterFilters.applicationCode" placeholder="全部应用" @keyup.enter="refreshDeadLetters" /></label><label><span>状态</span><select v-model="deadLetterFilters.status"><option value="">全部</option><option value="PENDING">待处理</option><option value="REPLAYED">已重放</option><option value="IGNORED">已忽略</option></select></label><button class="console-button primary small" type="button" @click="refreshDeadLetters">查询</button><button class="console-button ghost small" type="button" :disabled="!hasSelectedDeadLetters || loading" @click="replaySelected">批量重放（{{ selectedCount }}）</button></div>
      <div class="audit-operations__notice"><ConsoleIcon name="info" /><span>仅对待处理记录开放人工重放。原始 Payload、原始错误文本和认证信息永不返回至浏览器。</span></div>
      <div class="console-table-card audit-operations__table-card"><div class="console-table-scroll"><table class="console-data-table audit-operations__table"><thead><tr><th class="audit-operations__select">选择</th><th>死信 ID</th><th>应用 / 环境</th><th>事件 ID</th><th>状态</th><th>失败码</th><th>尝试次数</th><th>更新时间</th><th class="audit-operations__actions">操作</th></tr></thead><tbody><tr v-for="item in deadLetters" :key="item.dead_letter_id"><td class="audit-operations__select"><input v-if="item.status === 'PENDING'" :checked="selectedDeadLetters.has(item.dead_letter_id)" type="checkbox" :aria-label="`选择死信 ${item.dead_letter_id}`" @change="toggleSelection(item.dead_letter_id, $event.target.checked)" /><span v-else>—</span></td><td class="console-mono">{{ item.dead_letter_id }}</td><td><strong>{{ item.application_code }}</strong><small>{{ item.environment_code }}</small></td><td class="console-mono">{{ item.event_id }}</td><td><span class="audit-operations__status" :class="item.status === 'PENDING' ? 'warning' : 'success'">{{ item.status }}</span></td><td class="console-mono">{{ item.last_error_code || '—' }}</td><td>{{ item.attempts }}</td><td class="console-mono">{{ dateTime(item.updated_at) }}</td><td class="audit-operations__actions"><button v-if="item.status === 'PENDING'" class="console-text-button" type="button" @click="replayOne(item.dead_letter_id)">重放</button><span v-else>—</span></td></tr><tr v-if="!loading && !deadLetters.length"><td colspan="9" class="audit-operations__empty">暂无匹配的审计死信。</td></tr></tbody></table></div></div>
      <p v-if="hasSelectedDeadLetters" class="audit-operations__hint">已选择 {{ selectedCount }} 条待处理记录；单次批量重放上限为 100 条。</p>
    </template>

    <template v-else>
      <form class="audit-operations__retention-form" @submit.prevent="submitRetentionTask"><label><span>应用 ID</span><input v-model.trim="retentionForm.applicationId" required placeholder="精确应用 ID" /></label><label><span>操作</span><select v-model="retentionForm.mode"><option value="ARCHIVE">归档</option><option value="PURGE">清理已归档记录</option></select></label><label><span>截止时间</span><input v-model="retentionForm.cutoffAt" type="datetime-local" required /></label><label v-if="retentionForm.mode === 'PURGE'"><span>归档 ID</span><input v-model.trim="retentionForm.archiveId" required placeholder="已完成归档的 ID" /></label><button class="console-button primary" :disabled="submittingRetention" type="submit">创建任务</button></form>
      <div class="audit-operations__notice"><ConsoleIcon name="shield" /><span>清理只能依据已完成归档清单执行；系统没有按单条审计事件直接删除的接口。</span></div>
      <div class="audit-operations__toolbar"><label><span>应用 ID</span><input v-model.trim="retentionFilters.applicationId" placeholder="全部应用" @keyup.enter="refreshRetentionTasks" /></label><label><span>状态</span><select v-model="retentionFilters.status"><option value="">全部</option><option value="PENDING">待执行</option><option value="RUNNING">执行中</option><option value="COMPLETED">已完成</option><option value="FAILED">失败</option></select></label><button class="console-button ghost small" type="button" @click="refreshRetentionTasks">查询任务</button></div>
      <div class="console-table-card audit-operations__table-card"><div class="console-table-scroll"><table class="console-data-table audit-operations__table"><thead><tr><th>任务 ID</th><th>应用 ID</th><th>操作</th><th>状态</th><th>归档 ID</th><th>截止时间</th><th>已处理</th><th>失败码</th></tr></thead><tbody><tr v-for="item in retentionTasks" :key="item.task_id"><td class="console-mono">{{ item.task_id }}</td><td class="console-mono">{{ item.application_id }}</td><td>{{ item.mode }}</td><td><span class="audit-operations__status" :class="item.status === 'FAILED' ? 'danger' : item.status === 'COMPLETED' ? 'success' : 'warning'">{{ item.status }}</span></td><td class="console-mono">{{ item.archive_id || '—' }}</td><td class="console-mono">{{ dateTime(item.cutoff_at) }}</td><td>{{ item.processed_count }}</td><td class="console-mono">{{ item.failure_code || '—' }}</td></tr><tr v-if="!loading && !retentionTasks.length"><td colspan="8" class="audit-operations__empty">暂无审计保留任务。</td></tr></tbody></table></div></div>
    </template>
  </section>
</template>
