const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || '/api/v1').replace(/\/$/, '')

export class AuditOperationsError extends Error {
  constructor(message, options = {}) {
    super(message)
    this.name = 'AuditOperationsError'
    this.status = options.status || 0
    this.code = options.code || ''
    this.requestId = options.requestId || ''
  }
}

async function request(path, options = {}) {
  let response
  try {
    response = await fetch(`${API_BASE_URL}${path}`, {
      credentials: 'include',
      headers: {
        Accept: 'application/json',
        ...(options.body ? { 'Content-Type': 'application/json' } : {}),
        ...(options.headers || {}),
      },
      ...options,
    })
  } catch {
    throw new AuditOperationsError('无法连接审计运营服务。', { code: 'NETWORK_ERROR' })
  }

  const body = await response.json().catch(() => ({}))
  if (!response.ok) {
    throw new AuditOperationsError(body.message || '审计运营请求失败。', {
      status: response.status,
      code: body.code,
      requestId: body.request_id || '',
    })
  }
  return body.data
}

function queryString(params = {}) {
  const search = new URLSearchParams()
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== null && String(value).trim() !== '') search.set(key, String(value))
  })
  const value = search.toString()
  return value ? `?${value}` : ''
}

/** 查询平台接收其他应用审计事件后的批次回执；不返回审计事件正文。 */
export function listAuditIngestionReceipts(filters = {}) {
  return request(`/audit/ingestion-receipts${queryString({
    page: filters.page || 1,
    page_size: filters.pageSize || 20,
    'filter[application_code]': filters.applicationCode,
    'filter[environment_code]': filters.environmentCode,
    'filter[status]': filters.status,
    'filter[request_id]': filters.requestId,
    'filter[correlation_id]': filters.correlationId,
  })}`)
}

/** 查询平台侧审计上报死信元数据，浏览器永远不会收到死信原始 Payload。 */
export function listAuditDeadLetters(filters = {}) {
  return request(`/audit/dead-letters${queryString({
    page: filters.page || 1,
    page_size: filters.pageSize || 20,
    'filter[application_code]': filters.applicationCode,
    'filter[status]': filters.status,
  })}`)
}

export function getAuditDeadLetterStatus(applicationCode = '') {
  return request(`/audit/dead-letters/status${queryString({ 'filter[application_code]': applicationCode })}`)
}

/** 单条重放使用受保护的管理端接口；服务端重新校验租户和状态。 */
export function replayAuditDeadLetter(deadLetterID) {
  return request(`/audit/dead-letters/${encodeURIComponent(deadLetterID)}/replay`, {
    method: 'POST',
    body: '{}',
  })
}

/** 批量重放上限由服务端强制为 100 条。 */
export function replayAuditDeadLetters(deadLetterIDs) {
  return request('/audit/dead-letters:replay', {
    method: 'POST',
    body: JSON.stringify({ dead_letter_ids: deadLetterIDs }),
  })
}

export function listAuditRetentionTasks(filters = {}) {
  return request(`/audit/retention-tasks${queryString({
    page: filters.page || 1,
    page_size: filters.pageSize || 20,
    'filter[application_id]': filters.applicationId,
    'filter[mode]': filters.mode,
    'filter[status]': filters.status,
  })}`)
}

/** 创建异步归档或基于归档清单的清理任务；不提供按审计事件直接删除接口。 */
export function createAuditRetentionTask({ applicationId, mode, archiveId = '', cutoffAt }) {
  return request('/audit/retention-tasks', {
    method: 'POST',
    body: JSON.stringify({
      application_id: applicationId,
      mode,
      archive_id: archiveId,
      cutoff_at: cutoffAt,
    }),
  })
}
