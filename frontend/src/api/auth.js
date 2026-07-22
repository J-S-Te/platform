const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || '/api/v1').replace(/\/$/, '')

export class AuthError extends Error {
  constructor(message, options = {}) {
    super(message)
    this.name = 'AuthError'
    this.status = options.status || 0
    this.code = options.code || ''
    this.traceId = options.traceId || ''
  }
}

async function readResponseBody(response) {
  const contentType = response.headers.get('content-type') || ''

  if (contentType.includes('application/json')) {
    return response.json()
  }

  const text = await response.text()
  return text ? { message: text } : {}
}

/**
 * 账号密码登录。
 *
 * 安全约定：
 * 1. 前端不持久化 access token/refresh token；
 * 2. 浏览器通过 HttpOnly + Secure + SameSite Cookie 接收会话；
 * 3. 所有后续请求均携带 credentials: 'include'；
 * 4. 若服务端要求 MFA，预认证凭据只由调用方在当前页面内存中短暂持有。
 */
export async function loginWithPassword({
  account,
  password,
  applicationId = '',
  environmentId = '',
  loginTargetCode = '',
}) {
  let response

  try {
    response = await fetch(`${API_BASE_URL}/auth/login`, {
      method: 'POST',
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
      },
      body: JSON.stringify({
        account,
        password,
        login_type: 'password',
        ...(applicationId ? { application_id: applicationId } : {}),
        ...(environmentId ? { environment_id: environmentId } : {}),
        ...(loginTargetCode ? { login_target_code: loginTargetCode } : {}),
      }),
    })
  } catch (error) {
    throw new AuthError('无法连接登录服务，请确认后端服务已启动。', {
      code: 'NETWORK_ERROR',
    })
  }

  const body = await readResponseBody(response)

  if (!response.ok) {
    throw new AuthError(body.message || body.msg || '账号或密码错误，请重新输入。', {
      status: response.status,
      code: body.code,
      traceId: body.trace_id || body.traceId,
    })
  }

  return {
    body,
    status: response.status,
  }
}

/**
 * 完成登录前置 MFA 验证。
 *
 * 钉钉回调由服务端写入 HttpOnly 的 MFA 预认证 Cookie，浏览器只提交动态验证码或
 * 恢复码；本地密码登录返回的一次性预认证凭据仅在当前页面内存中短暂持有并随本次
 * 验证请求提交，绝不写入 Cookie 以外的浏览器存储、日志或埋点。
 */
export async function verifyMfaLogin({ code, preAuthenticationCredential = '' }) {
  let response

  const requestBody = { code }
  const normalizedCredential = String(preAuthenticationCredential || '').trim()
  if (normalizedCredential) {
    requestBody.pre_authentication_credential = normalizedCredential
  }

  try {
    response = await fetch(`${API_BASE_URL}/auth/login/mfa:verify`, {
      method: 'POST',
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
      },
      body: JSON.stringify(requestBody),
    })
  } catch {
    throw new AuthError('无法连接 MFA 验证服务，请稍后重试。', {
      code: 'NETWORK_ERROR',
    })
  }

  const body = await readResponseBody(response)

  if (!response.ok) {
    throw new AuthError(body.message || body.msg || '动态验证码或恢复码错误，请重新输入。', {
      status: response.status,
      code: body.code,
      traceId: body.trace_id || body.traceId,
    })
  }

  return body
}

/**
 * 创建钉钉扫码登录会话。
 *
 * 后端仅返回短期扫码会话和官方 DTFrameLogin SDK 所需的浏览器安全配置；
 * AppSecret、钉钉访问令牌及原始外部身份标识均不会经过浏览器。一次性 state
 * 仅用于当前扫码事务校验，不得写入日志、埋点或浏览器持久化存储。
 */
export async function createDingTalkQrSession({ tenantId, providerCode, returnTo }, options = {}) {
  let response

  try {
    response = await fetch(`${API_BASE_URL}/auth/dingtalk/qr-sessions`, {
      method: 'POST',
      credentials: 'include',
      signal: options.signal,
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
      },
      body: JSON.stringify({
        tenant_id: tenantId,
        provider_code: providerCode,
        return_to: returnTo,
      }),
    })
  } catch (error) {
    if (error.name === 'AbortError') {
      throw error
    }

    throw new AuthError('无法连接钉钉扫码服务，请确认后端服务已启动。', {
      code: 'NETWORK_ERROR',
    })
  }

  const body = await readResponseBody(response)

  if (!response.ok) {
    throw new AuthError(body.message || body.msg || '钉钉二维码初始化失败，请稍后重试。', {
      status: response.status,
      code: body.code,
      traceId: body.trace_id || body.traceId,
    })
  }

  return body
}
