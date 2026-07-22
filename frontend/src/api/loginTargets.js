const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || '/api/v1').replace(/\/$/, '')

/**
 * ApplicationLoginTargetError retains server error metadata so the hosting settings view can show
 * a safe message without exposing response bodies or transport implementation details.
 */
export class ApplicationLoginTargetError extends Error {
  constructor(message, options = {}) {
    super(message)
    this.name = 'ApplicationLoginTargetError'
    this.status = options.status || 0
    this.code = options.code || ''
    this.traceId = options.traceId || ''
  }
}

async function readResponse(response) {
  const contentType = response.headers.get('content-type') || ''
  if (contentType.includes('application/json')) {
    return response.json()
  }

  const text = await response.text()
  return text ? { message: text } : {}
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
    throw new ApplicationLoginTargetError('无法连接统一登录目标服务，请确认后端服务已启动。', {
      code: 'NETWORK_ERROR',
    })
  }

  const body = await readResponse(response)
  if (!response.ok) {
    throw new ApplicationLoginTargetError(body.message || '统一登录目标请求失败。', {
      status: response.status,
      code: body.code,
      traceId: body.trace_id || body.traceId,
    })
  }

  return body.data
}

function loginTargetCollectionPath(applicationId, environmentId) {
  return `/applications/${encodeURIComponent(applicationId)}/environments/${encodeURIComponent(environmentId)}/login-targets`
}

function loginTargetItemPath(applicationId, environmentId, loginTargetId) {
  return `${loginTargetCollectionPath(applicationId, environmentId)}/${encodeURIComponent(loginTargetId)}`
}

/** Lists approved post-login landing targets within one exact application environment. */
export function listApplicationLoginTargets({ applicationId, environmentId, page = 1, pageSize = 20, status = '' }) {
  const query = new URLSearchParams({ page: String(page), page_size: String(pageSize) })
  if (status) query.set('status', status)
  return request(`${loginTargetCollectionPath(applicationId, environmentId)}?${query.toString()}`)
}

/** Registers one exact allowlisted business landing URI. */
export function createApplicationLoginTarget({ applicationId, environmentId, targetCode, name, targetUri, status }) {
  return request(loginTargetCollectionPath(applicationId, environmentId), {
    method: 'POST',
    body: JSON.stringify({
      target_code: targetCode,
      name,
      target_uri: targetUri,
      status,
    }),
  })
}

/** Updates a target under optimistic locking; target_code remains immutable. */
export function updateApplicationLoginTarget({ applicationId, environmentId, loginTargetId, name, targetUri, status, version }) {
  return request(loginTargetItemPath(applicationId, environmentId, loginTargetId), {
    method: 'PATCH',
    body: JSON.stringify({
      name,
      target_uri: targetUri,
      status,
      version,
    }),
  })
}
