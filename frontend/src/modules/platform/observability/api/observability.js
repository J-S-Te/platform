const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || '/api/v1').replace(/\/$/, '')

export class ObservabilityError extends Error {
  constructor(message, options = {}) {
    super(message)
    this.name = 'ObservabilityError'
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
    throw new ObservabilityError('无法连接可观测性服务，请确认后端服务已启动。', { code: 'NETWORK_ERROR' })
  }

  const body = await response.json().catch(() => ({}))
  if (!response.ok) {
    throw new ObservabilityError(body.message || '可观测性服务请求失败。', {
      status: response.status,
      code: body.code,
      requestId: body.request_id,
    })
  }
  return body.data
}

function buildQuery(parameters = {}) {
  const query = new URLSearchParams()
  Object.entries(parameters).forEach(([key, value]) => {
    if (value !== undefined && value !== null && String(value).trim() !== '') {
      query.set(key, String(value).trim())
    }
  })
  const encoded = query.toString()
  return encoded ? `?${encoded}` : ''
}

/** 查询当前租户有权限查看的运行日志。 */
export function listRuntimeLogs(parameters = {}) {
  return request(`/observability/logs${buildQuery(parameters)}`)
}

/** 查询当前租户有权限查看的 Trace Span。 */
export function listTraceSpans(parameters = {}) {
  return request(`/observability/traces${buildQuery(parameters)}`)
}

/** 查询当前租户有权限查看的指标观测值。 */
export function listMetricPoints(parameters = {}) {
  return request(`/observability/metrics${buildQuery(parameters)}`)
}

/** 查询当前租户的可观测性告警规则。 */
export function listAlertRules() {
  return request('/observability/alert-rules')
}

/** 创建指标阈值告警规则。 */
export function createAlertRule(payload) {
  return request('/observability/alert-rules', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

/** 使用乐观锁版本完整更新一条告警规则。 */
export function updateAlertRule(ruleId, payload) {
  return request(`/observability/alert-rules/${encodeURIComponent(ruleId)}`, {
    method: 'PATCH',
    body: JSON.stringify(payload),
  })
}

/** 触发一次受权限保护的单规则执行，供管理员诊断规则配置。 */
export function executeAlertRule(ruleId) {
  return request(`/observability/alert-rules/${encodeURIComponent(ruleId)}:execute`, {
    method: 'POST',
    body: '{}',
  })
}
