<script setup>
import { computed, reactive, ref, watch } from 'vue'
import ConsoleIcon from '@/modules/platform/shared/components/ConsoleIcon.vue'
import {
  ApplicationLoginTargetError,
  createApplicationLoginTarget,
  listApplicationLoginTargets,
  updateApplicationLoginTarget,
} from '@/modules/platform/login-targets/api/loginTargets'
import '@/modules/platform/login-targets/styles/application-login-target.css'

const props = defineProps({
  applicationId: { type: String, default: '' },
  environmentId: { type: String, default: '' },
  applicationName: { type: String, default: '' },
  environmentName: { type: String, default: '' },
})

const emit = defineEmits(['toast'])

const loading = ref(false)
const submitting = ref(false)
const errorMessage = ref('')
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const statusFilter = ref('')
const targets = ref([])
const editorOpen = ref(false)
const editingTarget = ref(null)
const form = reactive(createEmptyForm())

const hasBoundary = computed(() => Boolean(props.applicationId && props.environmentId))
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))
const heading = computed(() => {
  const labels = [props.applicationName, props.environmentName].filter(Boolean)
  return labels.length ? `${labels.join(' / ')} 的登录目标` : '跨应用统一登录目标'
})

function createEmptyForm() {
  return {
    targetCode: '',
    name: '',
    targetUri: '',
    status: 'ACTIVE',
    version: 0,
  }
}

function showToast(message) {
  emit('toast', message)
}

function resetForm() {
  Object.assign(form, createEmptyForm())
  editingTarget.value = null
}

function openCreateEditor() {
  if (!hasBoundary.value) return
  errorMessage.value = ''
  resetForm()
  editorOpen.value = true
}

function openEditEditor(target) {
  errorMessage.value = ''
  editingTarget.value = target
  Object.assign(form, {
    targetCode: target.target_code,
    name: target.name,
    targetUri: target.target_uri,
    status: target.status,
    version: target.version,
  })
  editorOpen.value = true
}

function closeEditor() {
  if (submitting.value) return
  editorOpen.value = false
  resetForm()
}

function formatDate(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

function normalizedList(payload) {
  if (Array.isArray(payload)) {
    return { items: payload, total: payload.length }
  }
  return {
    items: Array.isArray(payload?.items) ? payload.items : [],
    total: Number(payload?.total || 0),
  }
}

async function loadTargets() {
  if (!hasBoundary.value) {
    targets.value = []
    total.value = 0
    return
  }

  loading.value = true
  errorMessage.value = ''
  try {
    const data = await listApplicationLoginTargets({
      applicationId: props.applicationId,
      environmentId: props.environmentId,
      page: page.value,
      pageSize: pageSize.value,
      status: statusFilter.value,
    })
    const list = normalizedList(data)
    targets.value = list.items
    total.value = list.total
    if (page.value > totalPages.value) {
      page.value = totalPages.value
      await loadTargets()
    }
  } catch (error) {
    errorMessage.value = error instanceof ApplicationLoginTargetError ? error.message : '读取登录目标失败。'
  } finally {
    loading.value = false
  }
}

async function submitEditor() {
  if (!hasBoundary.value || submitting.value) return

  submitting.value = true
  errorMessage.value = ''
  try {
    if (editingTarget.value) {
      await updateApplicationLoginTarget({
        applicationId: props.applicationId,
        environmentId: props.environmentId,
        loginTargetId: editingTarget.value.id,
        name: form.name.trim(),
        targetUri: form.targetUri.trim(),
        status: form.status,
        version: form.version,
      })
      showToast('登录目标已更新。')
    } else {
      await createApplicationLoginTarget({
        applicationId: props.applicationId,
        environmentId: props.environmentId,
        targetCode: form.targetCode.trim(),
        name: form.name.trim(),
        targetUri: form.targetUri.trim(),
        status: form.status,
      })
      showToast('登录目标已创建。')
    }
    closeEditor()
    await loadTargets()
  } catch (error) {
    if (error instanceof ApplicationLoginTargetError && error.code === 'VERSION_CONFLICT') {
      errorMessage.value = '该目标已被其他管理员更新，请刷新列表后再提交。'
    } else {
      errorMessage.value = error instanceof ApplicationLoginTargetError ? error.message : '保存登录目标失败。'
    }
  } finally {
    submitting.value = false
  }
}

function setPage(nextPage) {
  const safePage = Math.min(Math.max(nextPage, 1), totalPages.value)
  if (safePage !== page.value) page.value = safePage
}

watch(
  () => [props.applicationId, props.environmentId, statusFilter.value, page.value, pageSize.value],
  () => { loadTargets() },
  { immediate: true },
)
</script>

<template>
  <section class="login-target-module" aria-labelledby="login-target-heading">
    <header class="login-target-module__header">
      <div>
        <span class="login-target-module__eyebrow">APPLICATION ACCESS</span>
        <h2 id="login-target-heading">{{ heading }}</h2>
        <p>仅登记登录成功后允许跳转的业务地址；它与 OAuth 回调地址完全独立。</p>
      </div>
      <button class="console-button primary" type="button" :disabled="!hasBoundary" @click="openCreateEditor">
        <ConsoleIcon name="plus" /> 新建登录目标
      </button>
    </header>

    <div v-if="!hasBoundary" class="login-target-module__empty">
      <ConsoleIcon name="link" />
      <div>
        <strong>请先选择应用和环境</strong>
        <p>目标编码必须在精确的租户、应用和环境边界内维护，不能使用默认环境或任意 URL 代替。</p>
      </div>
    </div>

    <template v-else>
      <div class="login-target-module__guardrail">
        <ConsoleIcon name="shield" />
        <span>登录时仅按 <code>application_id + environment_id + target_code</code> 精确解析 ACTIVE 目标；未命中、禁用或校验失败时不得跳转。</span>
      </div>

      <div class="login-target-module__toolbar">
        <label>
          <span>状态</span>
          <select v-model="statusFilter">
            <option value="">全部</option>
            <option value="ACTIVE">启用</option>
            <option value="DISABLED">停用</option>
          </select>
        </label>
        <button class="console-button ghost small" type="button" :disabled="loading" @click="loadTargets">
          <ConsoleIcon name="refresh" /> 刷新
        </button>
      </div>

      <p v-if="errorMessage" class="login-target-module__error" role="alert">{{ errorMessage }}</p>

      <div class="console-table-card login-target-module__table-card">
        <div class="console-table-scroll">
          <table class="console-data-table login-target-module__table">
            <thead>
              <tr>
                <th>目标编码</th>
                <th>名称</th>
                <th>批准跳转地址</th>
                <th>状态</th>
                <th>更新时间</th>
                <th class="login-target-module__action-column">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="loading">
                <td colspan="6" class="login-target-module__state">正在读取登录目标…</td>
              </tr>
              <tr v-else-if="!targets.length">
                <td colspan="6" class="login-target-module__state">当前应用环境尚未登记登录目标。</td>
              </tr>
              <tr v-for="target in targets" :key="target.id">
                <td><code>{{ target.target_code }}</code></td>
                <td>{{ target.name }}</td>
                <td class="login-target-module__uri"><code>{{ target.target_uri }}</code></td>
                <td><span class="login-target-status" :class="`is-${String(target.status || '').toLowerCase()}`">{{ target.status === 'ACTIVE' ? '启用' : '停用' }}</span></td>
                <td>{{ formatDate(target.updated_at) }}</td>
                <td><button class="console-button ghost small" type="button" @click="openEditEditor(target)">编辑</button></td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <footer class="login-target-module__pagination">
        <span>共 {{ total }} 条</span>
        <div>
          <button class="console-button ghost small" type="button" :disabled="page <= 1 || loading" @click="setPage(page - 1)">上一页</button>
          <span>{{ page }} / {{ totalPages }}</span>
          <button class="console-button ghost small" type="button" :disabled="page >= totalPages || loading" @click="setPage(page + 1)">下一页</button>
        </div>
      </footer>
    </template>

    <div v-if="editorOpen" class="login-target-dialog-backdrop" @click.self="closeEditor">
      <form class="login-target-dialog" @submit.prevent="submitEditor">
        <header>
          <div>
            <span class="login-target-module__eyebrow">ALLOWLISTED DESTINATION</span>
            <h3>{{ editingTarget ? '编辑登录目标' : '新建登录目标' }}</h3>
          </div>
          <button class="login-target-dialog__close" type="button" aria-label="关闭" @click="closeEditor">×</button>
        </header>

        <label>
          <span>目标编码</span>
          <input v-model="form.targetCode" :disabled="Boolean(editingTarget)" maxlength="64" autocomplete="off" required placeholder="例如 contract-home" />
          <small>创建后不可修改；登录请求只可携带编码，不能提交目标 URL。</small>
        </label>
        <label>
          <span>名称</span>
          <input v-model="form.name" maxlength="128" autocomplete="off" required placeholder="例如 合同管理首页" />
        </label>
        <label>
          <span>批准跳转地址</span>
          <input v-model="form.targetUri" type="url" maxlength="2048" autocomplete="off" required placeholder="https://contract.example.com/workbench" />
          <small>必须为精确的 http/https 绝对地址。不要填写 OAuth redirect_uri。</small>
        </label>
        <label>
          <span>状态</span>
          <select v-model="form.status">
            <option value="ACTIVE">启用</option>
            <option value="DISABLED">停用</option>
          </select>
        </label>

        <p v-if="errorMessage" class="login-target-module__error" role="alert">{{ errorMessage }}</p>
        <footer>
          <button class="console-button ghost" type="button" :disabled="submitting" @click="closeEditor">取消</button>
          <button class="console-button primary" type="submit" :disabled="submitting">{{ submitting ? '保存中…' : '保存' }}</button>
        </footer>
      </form>
    </div>
  </section>
</template>
