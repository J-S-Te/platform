/**
 * 钉钉官方内嵌二维码 SDK 的固定可信来源。
 *
 * 该地址对应钉钉提供的 DTFrameLogin 浏览器 SDK。不要根据接口返回值动态拼接脚本地址，
 * 也不要将授权参数写入 LocalStorage、SessionStorage、日志或埋点。
 */
export const DINGTALK_FRAME_LOGIN_SDK_URL =
  'https://g.alicdn.com/dingding/h5-dingtalk-login/0.30.0/ddlogin.js'

const DINGTALK_FRAME_RENDER_MODE = 'dingtalk_frame'
const DINGTALK_FRAME_LOGIN_SCRIPT_ATTRIBUTE = 'data-basic-platform-dingtalk-frame-login'
const DINGTALK_QR_SIZE = 228
const SAFE_CLIENT_ID = /^[A-Za-z0-9._-]{1,256}$/
const SAFE_STATE = /^[A-Za-z0-9._~-]{16,1024}$/
const SAFE_PROMPT = /^[A-Za-z0-9._-]{1,64}$/

let frameLoginSDKPromise = null

function createValidationError(message) {
  return new Error(message)
}

function requireString(value, message, maxLength = 4096) {
  if (typeof value !== 'string') {
    throw createValidationError(message)
  }

  const normalizedValue = value.trim()
  if (!normalizedValue || normalizedValue.length > maxLength) {
    throw createValidationError(message)
  }

  return normalizedValue
}

function parseHTTPSURL(value, message) {
  let parsedURL
  try {
    parsedURL = new URL(value)
  } catch {
    throw createValidationError(message)
  }

  if (parsedURL.protocol !== 'https:' || parsedURL.username || parsedURL.password || parsedURL.hash) {
    throw createValidationError(message)
  }

  return parsedURL
}

function createQueryMap(searchParams) {
  const queryMap = new Map()

  for (const [key, value] of searchParams.entries()) {
    const values = queryMap.get(key) || []
    values.push(value)
    queryMap.set(key, values)
  }

  return queryMap
}

function haveSameValues(expectedValues, actualValues) {
  if (expectedValues.length !== actualValues.length) {
    return false
  }

  return expectedValues.every((value, index) => value === actualValues[index])
}

function decodeSDKValue(value) {
  if (typeof value !== 'string' || !value || value.length > 4096) {
    throw createValidationError('钉钉扫码返回的数据不符合安全要求。')
  }

  try {
    return decodeURIComponent(value.replace(/\+/g, ' '))
  } catch {
    throw createValidationError('钉钉扫码返回的数据不符合安全要求。')
  }
}

function validateCallbackQuery(callbackURL, redirectURI, state, sdkPayload) {
  const expectedQuery = createQueryMap(redirectURI.searchParams)
  const callbackQuery = createQueryMap(callbackURL.searchParams)

  if (expectedQuery.has('authCode') || expectedQuery.has('state')) {
    throw createValidationError('钉钉扫码回调地址配置不符合安全要求。')
  }

  for (const [key, values] of expectedQuery.entries()) {
    const callbackValues = callbackQuery.get(key)
    if (!callbackValues || !haveSameValues(values, callbackValues)) {
      throw createValidationError('钉钉扫码回调地址校验失败。')
    }
  }

  for (const key of callbackQuery.keys()) {
    if (key !== 'authCode' && key !== 'state' && !expectedQuery.has(key)) {
      throw createValidationError('钉钉扫码回调地址校验失败。')
    }
  }

  const authorizationCodes = callbackQuery.get('authCode') || []
  const states = callbackQuery.get('state') || []
  if (authorizationCodes.length !== 1 || states.length !== 1) {
    throw createValidationError('钉钉扫码回调数据不完整。')
  }

  const callbackState = states[0]
  const callbackAuthorizationCode = authorizationCodes[0]
  if (!callbackAuthorizationCode || callbackState !== state) {
    throw createValidationError('钉钉扫码状态校验失败，请重新获取二维码。')
  }

  // SDK 的 success 参数来自钉钉 iframe。仅在当前调用栈中校验其内容，
  // 不向组件状态、浏览器存储或日志返回授权码和 state。
  if (!sdkPayload || typeof sdkPayload !== 'object' || Array.isArray(sdkPayload)) {
    throw createValidationError('钉钉扫码返回的数据不符合安全要求。')
  }

  const payloadState = decodeSDKValue(sdkPayload.state)
  const payloadAuthorizationCode = decodeSDKValue(sdkPayload.authCode)
  if (payloadState !== state || payloadState !== callbackState || payloadAuthorizationCode !== callbackAuthorizationCode) {
    throw createValidationError('钉钉扫码状态校验失败，请重新获取二维码。')
  }
}

/**
 * 校验创建扫码会话接口下发给浏览器的 SDK 配置。
 *
 * 服务端仍是可信边界；浏览器侧校验用于防止错误配置使登录页加载或跳转到非预期地址。
 */
export function normalizeDingTalkQrSDKConfig(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw createValidationError('扫码服务未返回 SDK 配置。')
  }

  const clientID = requireString(value.client_id, '扫码服务返回的客户端标识无效。', 256)
  const redirectURIValue = requireString(value.redirect_uri, '扫码服务返回的回调地址无效。', 2048)
  const responseType = requireString(value.response_type, '扫码服务返回的授权类型无效。', 64)
  const scope = requireString(value.scope, '扫码服务返回的授权范围无效。', 512)
  const state = requireString(value.state, '扫码服务返回的状态参数无效。', 1024)
  const redirectURI = parseHTTPSURL(redirectURIValue, '扫码服务返回的回调地址无效。')

  if (!SAFE_CLIENT_ID.test(clientID) || responseType !== 'code' || !SAFE_STATE.test(state)) {
    throw createValidationError('扫码服务返回的 SDK 配置不符合安全要求。')
  }

  const scopes = scope.split(/\s+/).filter(Boolean)
  if (!scopes.includes('openid')) {
    throw createValidationError('扫码服务返回的授权范围不符合安全要求。')
  }

  const config = {
    client_id: clientID,
    redirect_uri: redirectURI.toString(),
    response_type: responseType,
    scope: scopes.join(' '),
    state,
  }

  if (value.prompt !== undefined && value.prompt !== null && value.prompt !== '') {
    const prompt = requireString(value.prompt, '扫码服务返回的授权提示无效。', 64)
    if (!SAFE_PROMPT.test(prompt)) {
      throw createValidationError('扫码服务返回的授权提示无效。')
    }
    config.prompt = prompt
  }

  return config
}

/**
 * 钉钉 SDK success 回调的严格校验。
 *
 * 只返回已验证的回调 URL；调用方不得记录其中的 authCode 或 state，而应立即顶层跳转至
 * 后端回调接口，由服务端完成授权码交换和会话签发。
 */
export function validateDingTalkQrSDKSuccess(sdkPayload, sdkConfig) {
  const config = normalizeDingTalkQrSDKConfig(sdkConfig)
  const redirectURLValue = requireString(sdkPayload?.redirectUrl, '钉钉扫码未返回有效回调地址。', 4096)
  const callbackURL = parseHTTPSURL(redirectURLValue, '钉钉扫码未返回有效回调地址。')
  const redirectURI = parseHTTPSURL(config.redirect_uri, '扫码服务返回的回调地址无效。')

  if (callbackURL.origin !== redirectURI.origin || callbackURL.pathname !== redirectURI.pathname) {
    throw createValidationError('钉钉扫码回调地址校验失败。')
  }

  validateCallbackQuery(callbackURL, redirectURI, config.state, sdkPayload)
  return callbackURL.toString()
}

function getSDKScript(documentRef) {
  const scripts = documentRef.querySelectorAll(`script[src="${DINGTALK_FRAME_LOGIN_SDK_URL}"]`)
  return scripts[0] || null
}

function schedule(callback) {
  if (typeof queueMicrotask === 'function') {
    queueMicrotask(callback)
    return
  }

  Promise.resolve().then(callback)
}

/**
 * 动态加载固定版本的钉钉 DTFrameLogin SDK，并在同一页面内去重。
 */
export function loadDingTalkFrameLoginSDK(documentRef = document, windowRef = window) {
  if (typeof windowRef.DTFrameLogin === 'function') {
    return Promise.resolve(windowRef.DTFrameLogin)
  }

  if (frameLoginSDKPromise) {
    return frameLoginSDKPromise
  }

  frameLoginSDKPromise = new Promise((resolve, reject) => {
    const complete = () => {
      if (typeof windowRef.DTFrameLogin === 'function') {
        resolve(windowRef.DTFrameLogin)
        return
      }

      reject(createValidationError('钉钉扫码组件加载失败，请稍后重试。'))
    }
    const fail = () => reject(createValidationError('钉钉扫码组件加载失败，请稍后重试。'))
    const script = getSDKScript(documentRef) || documentRef.createElement('script')
    const isNewScript = !script.parentNode

    script.addEventListener('load', complete, { once: true })
    script.addEventListener('error', fail, { once: true })

    if (!isNewScript && script.getAttribute(DINGTALK_FRAME_LOGIN_SCRIPT_ATTRIBUTE) === 'loaded') {
      schedule(complete)
      return
    }

    if (isNewScript) {
      script.src = DINGTALK_FRAME_LOGIN_SDK_URL
      script.async = true
      script.referrerPolicy = 'no-referrer'
      script.setAttribute(DINGTALK_FRAME_LOGIN_SCRIPT_ATTRIBUTE, 'loading')
      const container = documentRef.head || documentRef.body
      if (!container) {
        fail()
        return
      }
      container.appendChild(script)
    }

    script.addEventListener(
      'load',
      () => script.setAttribute(DINGTALK_FRAME_LOGIN_SCRIPT_ATTRIBUTE, 'loaded'),
      { once: true },
    )
  }).catch((error) => {
    frameLoginSDKPromise = null
    throw error
  })

  return frameLoginSDKPromise
}

/**
 * 使用钉钉官方 DTFrameLogin SDK 渲染内嵌二维码。
 *
 * SDK 负责创建内部 iframe；平台代码不再直接嵌入授权 URL，也不使用 postMessage 桥。
 */
export async function mountDingTalkFrameLogin({
  containerID,
  sdkConfig,
  onSuccess,
  onError,
  isActive = () => true,
  documentRef = document,
  windowRef = window,
}) {
  if (typeof containerID !== 'string' || !containerID.trim()) {
    throw createValidationError('钉钉二维码容器无效。')
  }
  if (typeof onSuccess !== 'function' || typeof onError !== 'function') {
    throw createValidationError('钉钉二维码处理器无效。')
  }
  if (typeof isActive !== 'function') {
    throw createValidationError('钉钉二维码渲染状态无效。')
  }

  const container = documentRef.getElementById(containerID)
  if (!container) {
    throw createValidationError('钉钉二维码容器未准备完成。')
  }

  const config = normalizeDingTalkQrSDKConfig(sdkConfig)
  const frameLogin = await loadDingTalkFrameLoginSDK(documentRef, windowRef)

  // SDK 加载期间可能已切换租户、刷新二维码或离开扫码页。过期请求不得清空或覆盖新二维码。
  if (!isActive()) {
    return false
  }

  container.replaceChildren()
  if (!isActive()) {
    return false
  }

  frameLogin(
    {
      id: containerID,
      width: DINGTALK_QR_SIZE,
      height: DINGTALK_QR_SIZE,
    },
    config,
    (sdkPayload) => {
      try {
        onSuccess(validateDingTalkQrSDKSuccess(sdkPayload, config))
      } catch (error) {
        onError(error)
      }
    },
    () => onError(createValidationError('钉钉扫码未能完成，请重新获取二维码后重试。')),
  )

  return true
}

export function isDingTalkFrameRenderMode(renderMode) {
  return String(renderMode || '').trim().toLowerCase() === DINGTALK_FRAME_RENDER_MODE
}
