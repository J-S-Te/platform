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
 * 3. 所有后续请求均携带 credentials: 'include'。
 */
export async function loginWithPassword({ account, password }) {
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

  return body
}
