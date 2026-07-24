<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import ConsoleIcon from '@/modules/platform/shared/components/ConsoleIcon.vue'
import {
  FileTaskError,
  cancelAsyncJob,
  cleanupExpiredFiles,
  createAsyncJob,
  downloadLocalFile,
  listAsyncJobs,
  rerunAsyncJob,
  retryAsyncJob,
  uploadLocalFile,
} from '@/modules/platform/files/api/fileTasks'
import '@/modules/platform/files/styles/file-task-operations.css'

const emit = defineEmits(['toast'])

const selectedFile = ref(null)
const uploadInput = ref(null)
const uploading = ref(false)
const downloadId = ref('')
const downloading = ref(false)
const loading = ref(false)
const actionJobId = ref('')
const errorMessage = ref('')
const jobs = ref([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

const uploadForm = reactive({ applicationId: '', classification: 'INTERNAL' })
const filters = reactive({ status: '', jobType: '', applicationId: '', query: '' })
const jobForm = reactive({ applicationId: '', jobType: '', aggregateType: '', aggregateId: '', payloadText: '{\n  \n}', priority: 100, maxAttempts: 3 })
const cleanupForm = reactive({ before: '', maxFiles: 50 })

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))

function toast(message) { emit('toast', message) }
function formatDate(value) { const date = new Date(value); return value && !Number.isNaN(date.getTime()) ? date.toLocaleString('zh-CN', { hour12: false }) : '—' }
function statusClass(status) { return `filetask-status--${String(status || '').toLowerCase()}` }
function onFileSelected(event) { selectedFile.value = event.target.files?.[0] || null }
function normalizeError(error, fallback) { return error instanceof FileTaskError ? error.message : fallback }

async function upload() {
  if (!selectedFile.value || !uploadForm.applicationId.trim() || uploading.value) { errorMessage.value = '请选择文件并填写所属应用 ID。'; return }
  uploading.value = true; errorMessage.value = ''
  try {
    const result = await uploadLocalFile({ applicationId: uploadForm.applicationId, file: selectedFile.value, classification: uploadForm.classification })
    downloadId.value = result.file_id || ''
    toast(`文件“${result.original_name || selectedFile.value.name}”已安全落盘。`)
    selectedFile.value = null
    if (uploadInput.value) uploadInput.value.value = ''
  } catch (error) { errorMessage.value = normalizeError(error, '文件上传失败。') } finally { uploading.value = false }
}

async function download() {
  const fileId = downloadId.value.trim()
  if (!fileId || downloading.value) return
  downloading.value = true; errorMessage.value = ''
  try {
    const { blob, filename } = await downloadLocalFile(fileId)
    const objectUrl = URL.createObjectURL(blob); const link = document.createElement('a')
    link.href = objectUrl; link.download = filename || fileId; link.click(); URL.revokeObjectURL(objectUrl)
  } catch (error) { errorMessage.value = normalizeError(error, '文件下载失败。') } finally { downloading.value = false }
}

async function loadJobs() {
  loading.value = true; errorMessage.value = ''
  try {
    const result = await listAsyncJobs({ ...filters, page: page.value, pageSize: pageSize.value })
    jobs.value = Array.isArray(result?.items) ? result.items : []; total.value = Number(result?.total || 0)
  } catch (error) { errorMessage.value = normalizeError(error, '异步任务查询失败。') } finally { loading.value = false }
}

async function createJob() {
  let payload
  try { payload = JSON.parse(jobForm.payloadText) } catch { errorMessage.value = '任务负载必须是有效 JSON，且不得包含密码、密钥或令牌。'; return }
  errorMessage.value = ''
  try {
    const job = await createAsyncJob({ applicationId: jobForm.applicationId.trim(), jobType: jobForm.jobType.trim(), aggregateType: jobForm.aggregateType.trim(), aggregateId: jobForm.aggregateId.trim(), payload, priority: Number(jobForm.priority), maxAttempts: Number(jobForm.maxAttempts) })
    toast(`异步任务 ${job.job_id || ''} 已创建。`); await loadJobs()
  } catch (error) { errorMessage.value = normalizeError(error, '创建异步任务失败。') }
}

async function operateJob(job, operation) {
  if (!job?.job_id || actionJobId.value) return
  actionJobId.value = job.job_id; errorMessage.value = ''
  try {
    if (operation === 'cancel') await cancelAsyncJob(job.job_id)
    if (operation === 'retry') await retryAsyncJob(job.job_id)
    if (operation === 'rerun') await rerunAsyncJob(job.job_id)
    toast(operation === 'cancel' ? '任务已取消。' : operation === 'retry' ? '任务已重新入队。' : '已创建新的重跑任务。'); await loadJobs()
  } catch (error) { errorMessage.value = normalizeError(error, '任务操作失败。') } finally { actionJobId.value = '' }
}

async function cleanup() {
  const parsed = new Date(cleanupForm.before)
  if (!cleanupForm.before || Number.isNaN(parsed.getTime()) || !Number.isInteger(Number(cleanupForm.maxFiles)) || Number(cleanupForm.maxFiles) < 1) { errorMessage.value = '请填写 UTC 清理截止时间和正整数批次大小。'; return }
  errorMessage.value = ''
  try { const result = await cleanupExpiredFiles({ before: parsed.toISOString(), maxFiles: Number(cleanupForm.maxFiles) }); toast(`清理完成：删除 ${result.deleted_files || 0} 个文件，清理 ${result.removed_temp_files || 0} 个临时文件。`) } catch (error) { errorMessage.value = normalizeError(error, '文件清理执行失败。') }
}

function changePage(next) { page.value = Math.min(Math.max(next, 1), totalPages.value); loadJobs() }
onMounted(loadJobs)
</script>

<template>
  <section class="filetask-module" aria-labelledby="filetask-heading">
    <header class="filetask-module__header"><div><span class="filetask-module__eyebrow">LOCAL FILES · MYSQL JOBS</span><h2 id="filetask-heading">文件与异步任务</h2><p>文件仅写入平台本地目录，元数据和任务状态持久化在 MySQL；不使用对象存储或 Redis。</p></div><button class="console-button ghost" type="button" :disabled="loading" @click="loadJobs"><ConsoleIcon name="reset" /> 刷新任务</button></header>
    <p v-if="errorMessage" class="filetask-module__error">{{ errorMessage }}</p>
    <div class="filetask-module__grid">
      <article class="filetask-card"><header><ConsoleIcon name="export" /><div><h3>安全上传</h3><p>限制服务端白名单类型、最大大小与原子落盘。</p></div></header><label><span>所属应用 ID</span><input v-model="uploadForm.applicationId" autocomplete="off" placeholder="例如 contract-management" /></label><label><span>密级</span><select v-model="uploadForm.classification"><option value="INTERNAL">内部</option><option value="CONFIDENTIAL">机密</option><option value="RESTRICTED">受限</option></select></label><label><span>选择文件</span><input ref="uploadInput" type="file" accept=".pdf,.jpg,.jpeg,.png,.txt,.csv,application/pdf,image/jpeg,image/png,text/plain,text/csv" @change="onFileSelected" /></label><div class="filetask-card__actions"><button class="console-button primary" type="button" :disabled="uploading" @click="upload">{{ uploading ? '上传中…' : '上传文件' }}</button></div></article>
      <article class="filetask-card"><header><ConsoleIcon name="export" /><div><h3>授权下载</h3><p>服务端同时校验租户、文件状态和文件所有者/下载权限。</p></div></header><label><span>文件 ID</span><input v-model="downloadId" autocomplete="off" placeholder="上传成功后自动填入" /></label><div class="filetask-card__actions"><button class="console-button ghost" type="button" :disabled="downloading || !downloadId.trim()" @click="download">{{ downloading ? '下载中…' : '下载文件' }}</button></div></article>
      <article class="filetask-card filetask-card--cleanup"><header><ConsoleIcon name="shield" /><div><h3>受控清理</h3><p>仅清理无有效绑定且早于策略截止时间的文件；应由具备高风险操作授权的管理员执行。</p></div></header><label><span>截止时间（本地输入，提交 UTC）</span><input v-model="cleanupForm.before" type="datetime-local" /></label><label><span>本次最多处理</span><input v-model.number="cleanupForm.maxFiles" type="number" min="1" max="500" /></label><div class="filetask-card__actions"><button class="console-button danger" type="button" @click="cleanup">执行清理</button></div></article>
    </div>
    <article class="filetask-card filetask-card--job-create"><header><ConsoleIcon name="info" /><div><h3>创建异步任务</h3><p>仅可创建已注册的任务类型。负载不得包含密码、Token、客户端密钥或原始文件内容。</p></div></header><div class="filetask-form-grid"><label><span>应用 ID</span><input v-model="jobForm.applicationId" placeholder="可选" /></label><label><span>任务类型</span><input v-model="jobForm.jobType" placeholder="例如 REPORT_EXPORT" /></label><label><span>聚合类型</span><input v-model="jobForm.aggregateType" placeholder="可选" /></label><label><span>聚合 ID</span><input v-model="jobForm.aggregateId" placeholder="可选" /></label><label><span>优先级</span><input v-model.number="jobForm.priority" type="number" /></label><label><span>最大尝试次数</span><input v-model.number="jobForm.maxAttempts" type="number" min="1" max="100" /></label></div><label><span>JSON 负载</span><textarea v-model="jobForm.payloadText" rows="5" spellcheck="false" /></label><div class="filetask-card__actions"><button class="console-button primary" type="button" @click="createJob">创建任务</button></div></article>
    <article class="filetask-card filetask-card--jobs"><header class="filetask-jobs__header"><div><h3>任务运营查询</h3><p>运行中的任务不可直接取消；失败任务可重试，终态任务可创建新的重跑记录。</p></div><div class="filetask-jobs__filters"><select v-model="filters.status" @change="page = 1; loadJobs()"><option value="">全部状态</option><option value="PENDING">等待</option><option value="RUNNING">运行中</option><option value="SUCCEEDED">成功</option><option value="FAILED">失败</option><option value="DEAD">死信</option><option value="CANCELLED">已取消</option></select><input v-model="filters.jobType" placeholder="任务类型" @keyup.enter="page = 1; loadJobs()" /><button class="console-button ghost small" type="button" @click="page = 1; loadJobs()">查询</button></div></header><div class="filetask-table-wrap"><table class="console-data-table filetask-table"><thead><tr><th>任务 ID</th><th>类型</th><th>状态</th><th>尝试</th><th>可执行时间</th><th>错误摘要</th><th>操作</th></tr></thead><tbody><tr v-if="loading"><td colspan="7">正在加载…</td></tr><tr v-else-if="!jobs.length"><td colspan="7">暂无任务记录。</td></tr><tr v-for="job in jobs" :key="job.job_id"><td><code>{{ job.job_id }}</code></td><td>{{ job.job_type }}</td><td><span class="filetask-status" :class="statusClass(job.status)">{{ job.status }}</span></td><td>{{ job.attempts }}/{{ job.max_attempts }}</td><td>{{ formatDate(job.available_at) }}</td><td>{{ job.last_error_message || '—' }}</td><td class="filetask-table__actions"><button v-if="['PENDING','FAILED','DEAD'].includes(job.status)" class="console-button ghost small" type="button" :disabled="Boolean(actionJobId)" @click="operateJob(job, 'cancel')">取消</button><button v-if="['FAILED','DEAD'].includes(job.status)" class="console-button ghost small" type="button" :disabled="Boolean(actionJobId)" @click="operateJob(job, 'retry')">重试</button><button v-if="['SUCCEEDED','FAILED','DEAD','CANCELLED'].includes(job.status)" class="console-button ghost small" type="button" :disabled="Boolean(actionJobId)" @click="operateJob(job, 'rerun')">重跑</button></td></tr></tbody></table></div><footer class="filetask-pagination"><span>共 {{ total }} 条</span><span><button class="console-button ghost small" type="button" :disabled="page <= 1" @click="changePage(page - 1)">上一页</button><strong>{{ page }} / {{ totalPages }}</strong><button class="console-button ghost small" type="button" :disabled="page >= totalPages" @click="changePage(page + 1)">下一页</button></span></footer></article>
  </section>
</template>
