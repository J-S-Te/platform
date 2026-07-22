<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { createDingTalkQrSession, loginWithPassword, verifyMfaLogin } from '@/api/auth'
import {
  isDingTalkFrameRenderMode,
  mountDingTalkFrameLogin,
  normalizeDingTalkQrSDKConfig,
} from '@/utils/dingtalkQr'
import { resolveSameOriginRedirect, resolveServerApprovedRedirect } from '@/utils/navigation'

const STORAGE_KEY = 'basic-platform.remembered-account'
const LOGIN_SUCCESS_URL = import.meta.env.VITE_LOGIN_SUCCESS_URL || '/'
const DINGTALK_PROVIDER_CODE = import.meta.env.VITE_DINGTALK_PROVIDER_CODE || 'DINGTALK_QR'
const DINGTALK_QR_CONTAINER_ID = 'dingtalk-qr-frame-login'

const activeTab = ref('password')
const account = ref(localStorage.getItem(STORAGE_KEY) || '')
const password = ref('')
const rememberAccount = ref(Boolean(account.value))
const passwordVisible = ref(false)
const submitting = ref(false)
const formError = ref('')
const formSuccess = ref('')
const tenantId = ref(new URLSearchParams(window.location.search).get('tenant_id') || '')
const qrStatus = ref('idle')
const qrError = ref('')
const qrSession = ref(null)
const qrSeconds = ref(0)
const qrContainer = ref(null)
const mfaRequired = ref(false)
const mfaCode = ref('')
const mfaError = ref('')
const mfaSubmitting = ref(false)
const mfaReturnTo = ref('')
const mfaLoginSource = ref('')
let mfaPreAuthenticationCredential = ''
let qrTimer = null
let qrRequestController = null
let qrRequestVersion = 0

const currentYear = new Date().getFullYear()
const countdown = computed(() => {
  const minutes = String(Math.floor(qrSeconds.value / 60)).padStart(2, '0')
  const seconds = String(qrSeconds.value % 60).padStart(2, '0')
  return `${minutes}:${seconds}`
})
const qrIsReady = computed(() => qrStatus.value === 'ready' && Boolean(qrSession.value))
const qrIsExpired = computed(() => qrStatus.value === 'expired')

function getLoginReturnTo() {
  const oidcReturnTo = new URLSearchParams(window.location.search).get('return_to')
  return resolveSameOriginRedirect(oidcReturnTo || LOGIN_SUCCESS_URL)
}

function getLoginTargetSelection() {
  const parameters = new URLSearchParams(window.location.search)
  return {
    applicationId: parameters.get('application_id')?.trim() || '',
    environmentId: parameters.get('environment_id')?.trim() || '',
    loginTargetCode: parameters.get('login_target_code')?.trim() || '',
  }
}

function redirectTopLevel(candidate, allowApprovedCrossOrigin = false) {
  const redirectUrl = allowApprovedCrossOrigin
    ? resolveServerApprovedRedirect(candidate)
    : resolveSameOriginRedirect(candidate)

  if (window.top === window) {
    window.location.assign(redirectUrl)
    return
  }

  try {
    window.top.location.assign(redirectUrl)
  } catch {
    // 嵌入宿主无权导航顶层窗口时，仍只在当前同源页面完成受控跳转。
    window.location.assign(redirectUrl)
  }
}

function switchTab(tab) {
  activeTab.value = tab
  formError.value = ''
  formSuccess.value = ''

  if (tab === 'qrcode') {
    void ensureDingTalkQrSession()
    return
  }

  abortQrSessionRequest()
  resetQrSession()
  qrStatus.value = 'idle'
  qrError.value = ''
}

function handleTenantIdInput() {
  if (!qrSession.value && qrStatus.value === 'idle') {
    qrError.value = ''
    return
  }

  abortQrSessionRequest()
  resetQrSession()
  qrStatus.value = 'idle'
  qrError.value = ''
}

function validateForm() {
  const normalizedAccount = account.value.trim()

  if (!normalizedAccount) {
    return '请输入用户名、手机号或邮箱。'
  }

  if (!password.value) {
    return '请输入登录密码。'
  }

  if (password.value.length < 6) {
    return '密码长度不能少于 6 位。'
  }

  return ''
}

async function submitLogin() {
  formError.value = validateForm()
  formSuccess.value = ''

  if (formError.value || submitting.value) {
    return
  }

  submitting.value = true

  try {
    const response = await loginWithPassword({
      account: account.value.trim(),
      password: password.value,
      ...getLoginTargetSelection(),
    })
    const result = response.body
    const data = result?.data || result

    if (rememberAccount.value) {
      localStorage.setItem(STORAGE_KEY, account.value.trim())
    } else {
      localStorage.removeItem(STORAGE_KEY)
    }

    if (response.status === 202 || data?.mfa_required === true) {
      const preAuthenticationCredential =
        typeof data?.pre_authentication_credential === 'string' ? data.pre_authentication_credential.trim() : ''

      // 此凭据只保存在当前组件闭包中，用于下一次 MFA 请求；不进入响应式状态或浏览器存储。
      enterMfa({
        returnTo: data?.redirect_url || getLoginReturnTo(),
        source: 'password',
        preAuthenticationCredential,
      })
      password.value = ''
      return
    }

    formSuccess.value = result?.message || result?.msg || '登录成功，正在进入平台…'

    const redirectUrl =
      data?.redirect_url ||
      data?.redirectUrl ||
      result?.redirect_url ||
      LOGIN_SUCCESS_URL

    window.setTimeout(() => redirectTopLevel(redirectUrl, true), 450)
  } catch (error) {
    const traceText = error.traceId ? `（追踪号：${error.traceId}）` : ''
    formError.value = `${error.message || '登录失败，请稍后重试。'}${traceText}`
  } finally {
    submitting.value = false
  }
}

function showAccountHelp() {
  formSuccess.value = ''
  formError.value = '请联系平台管理员重置密码；重置操作将记录到安全审计日志。'
}

function validateTenantId() {
  const normalizedTenantId = tenantId.value.trim()

  if (!normalizedTenantId) {
    return '请输入租户 ID 后再获取钉钉二维码。'
  }

  if (normalizedTenantId.length > 128) {
    return '租户 ID 长度不能超过 128 个字符。'
  }

  return ''
}

function abortQrSessionRequest() {
  qrRequestVersion += 1

  if (qrRequestController) {
    qrRequestController.abort()
    qrRequestController = null
  }
}

function stopQrCountdown() {
  if (qrTimer) {
    window.clearInterval(qrTimer)
    qrTimer = null
  }
}

function startQrCountdown(sessionID, expiresAt) {
  stopQrCountdown()

  const expiresAtMs = Date.parse(expiresAt)
  const updateCountdown = () => {
    if (qrSession.value?.sessionId !== sessionID || qrStatus.value !== 'ready') {
      stopQrCountdown()
      return
    }

    const secondsRemaining = Math.max(0, Math.ceil((expiresAtMs - Date.now()) / 1000))
    qrSeconds.value = secondsRemaining

    if (secondsRemaining === 0) {
      resetQrSession()
      qrStatus.value = 'expired'
    }
  }

  updateCountdown()
  if (qrStatus.value === 'ready') {
    qrTimer = window.setInterval(updateCountdown, 1000)
  }
}

function resetQrSession() {
  stopQrCountdown()
  qrContainer.value?.replaceChildren()
  qrSession.value = null
  qrSeconds.value = 0
}

async function ensureDingTalkQrSession() {
  if (mfaRequired.value || activeTab.value !== 'qrcode' || qrIsReady.value || qrStatus.value === 'loading') {
    return
  }

  await requestDingTalkQrSession()
}

function handleDingTalkQrSuccess(sessionID, callbackURL) {
  if (qrSession.value?.sessionId !== sessionID || qrStatus.value !== 'ready') {
    return
  }

  qrStatus.value = 'completing'
  abortQrSessionRequest()
  resetQrSession()

  // callbackURL 已由 SDK 适配器校验为后端登记的 redirect_uri，授权码仅随本次顶层跳转发送。
  if (window.top === window) {
    window.location.assign(callbackURL)
    return
  }

  try {
    window.top.location.assign(callbackURL)
  } catch {
    window.location.assign(callbackURL)
  }
}

function handleDingTalkQrFailure(sessionID, error) {
  if (qrSession.value?.sessionId !== sessionID || qrStatus.value === 'completing') {
    return
  }

  abortQrSessionRequest()
  resetQrSession()
  qrStatus.value = 'error'
  qrError.value = error instanceof Error && error.message ? error.message : '钉钉二维码初始化失败，请稍后重试。'
}

async function requestDingTalkQrSession() {
  if (mfaRequired.value || activeTab.value !== 'qrcode') {
    return
  }

  const tenantError = validateTenantId()
  if (tenantError) {
    resetQrSession()
    qrStatus.value = 'idle'
    qrError.value = tenantError
    return
  }

  abortQrSessionRequest()
  resetQrSession()
  qrStatus.value = 'loading'
  qrError.value = ''
  const requestVersion = qrRequestVersion + 1
  qrRequestVersion = requestVersion
  const requestController = new AbortController()
  qrRequestController = requestController

  try {
    const result = await createDingTalkQrSession(
      {
        tenantId: tenantId.value.trim(),
        providerCode: DINGTALK_PROVIDER_CODE,
        returnTo: getLoginReturnTo(),
      },
      { signal: requestController.signal },
    )
    if (requestVersion !== qrRequestVersion || mfaRequired.value || activeTab.value !== 'qrcode') {
      return
    }

    const data = result?.data || result
    const sessionId = typeof data?.session_id === 'string' ? data.session_id.trim() : ''
    const expiresAt = typeof data?.expires_at === 'string' ? data.expires_at : ''
    const renderMode = typeof data?.render_mode === 'string' ? data.render_mode : ''
    const expiresAtMs = Date.parse(expiresAt)
    const sdkConfig = normalizeDingTalkQrSDKConfig(data?.sdk_config)

    if (!sessionId || !Number.isFinite(expiresAtMs) || expiresAtMs <= Date.now()) {
      throw new Error('扫码服务返回的数据不完整或已失效。')
    }
    if (!isDingTalkFrameRenderMode(renderMode)) {
      throw new Error('扫码服务返回的呈现方式不受支持。')
    }

    qrSession.value = {
      sessionId,
      expiresAt,
      sdkConfig,
    }
    qrStatus.value = 'ready'
    startQrCountdown(sessionId, expiresAt)
    await nextTick()

    if (requestVersion !== qrRequestVersion || qrSession.value?.sessionId !== sessionId || qrStatus.value !== 'ready') {
      return
    }

    await mountDingTalkFrameLogin({
      containerID: DINGTALK_QR_CONTAINER_ID,
      sdkConfig,
      onSuccess: (callbackURL) => handleDingTalkQrSuccess(sessionId, callbackURL),
      onError: (error) => handleDingTalkQrFailure(sessionId, error),
      isActive: () =>
        requestVersion === qrRequestVersion &&
        qrSession.value?.sessionId === sessionId &&
        qrStatus.value === 'ready' &&
        activeTab.value === 'qrcode' &&
        !mfaRequired.value,
    })
  } catch (error) {
    if (error?.name === 'AbortError' || requestVersion !== qrRequestVersion) {
      return
    }

    resetQrSession()
    qrStatus.value = 'error'
    qrError.value = error.message || '钉钉二维码初始化失败，请稍后重试。'
  } finally {
    if (qrRequestController === requestController) {
      qrRequestController = null
    }
  }
}

function refreshQrCode() {
  void requestDingTalkQrSession()
}

function getDingTalkFailureMessage(errorCode) {
  switch (errorCode) {
    case 'AUTH_DINGTALK_QR_SESSION_INVALID':
      return '二维码已失效或已使用，请重新获取后扫码。'
    case 'AUTH_DINGTALK_EXTERNAL_IDENTITY_NOT_BOUND':
      return '当前钉钉账号尚未绑定平台账号，请联系管理员。'
    case 'AUTH_DINGTALK_PROVIDER_DISABLED':
    case 'AUTH_DINGTALK_PROVIDER_NOT_AVAILABLE':
      return '钉钉扫码登录暂不可用，请稍后重试。'
    default:
      return '无法完成钉钉登录，请重新获取二维码后重试。'
  }
}

function enterMfa({ returnTo, source, preAuthenticationCredential = '' }) {
  const normalizedCredential =
    typeof preAuthenticationCredential === 'string' ? preAuthenticationCredential.trim() : ''

  if (source !== 'password' && source !== 'dingtalk') {
    throw new Error('不支持的 MFA 登录来源。')
  }
  if (source === 'password' && (normalizedCredential.length < 32 || normalizedCredential.length > 512)) {
    throw new Error('登录服务未返回有效的 MFA 验证信息，请重新登录。')
  }

  abortQrSessionRequest()
  resetQrSession()
  qrStatus.value = 'idle'
  qrError.value = ''
  mfaReturnTo.value = source === 'password'
    ? resolveServerApprovedRedirect(returnTo)
    : resolveSameOriginRedirect(returnTo)
  mfaLoginSource.value = source
  mfaPreAuthenticationCredential = normalizedCredential
  mfaCode.value = ''
  mfaError.value = ''
  mfaRequired.value = true
}

function clearMfaLoginState() {
  mfaPreAuthenticationCredential = ''
  mfaLoginSource.value = ''
  mfaCode.value = ''
  mfaError.value = ''
  mfaReturnTo.value = ''
}

function validateMfaCode() {
  const code = mfaCode.value.trim()

  if (!code) {
    return '请输入动态验证码或恢复码。'
  }
  if (code.length > 64) {
    return '动态验证码或恢复码长度不能超过 64 个字符。'
  }

  return ''
}

async function submitMfaLogin() {
  mfaError.value = validateMfaCode()
  if (mfaError.value || mfaSubmitting.value) {
    return
  }

  mfaSubmitting.value = true
  try {
    const result = await verifyMfaLogin({
      code: mfaCode.value.trim(),
      preAuthenticationCredential: mfaPreAuthenticationCredential,
    })
    const data = result?.data || result
    const redirectUrl = mfaReturnTo.value || (typeof data?.redirect_url === 'string' ? data.redirect_url : '')
    const allowApprovedCrossOrigin = mfaLoginSource.value === 'password'

    // 登录完成后立即清除仅存在于页面内存中的本地密码 MFA 预认证凭据。
    mfaPreAuthenticationCredential = ''
    mfaLoginSource.value = ''
    redirectTopLevel(redirectUrl || getLoginReturnTo(), allowApprovedCrossOrigin)
  } catch (error) {
    const traceText = error.traceId ? `（追踪号：${error.traceId}）` : ''
    mfaError.value = `${error.message || 'MFA 验证失败，请重新输入。'}${traceText}`
  } finally {
    mfaCode.value = ''
    mfaSubmitting.value = false
  }
}

function returnToLoginChoices() {
  clearMfaLoginState()
  mfaRequired.value = false
  activeTab.value = 'password'
}

function clearDingTalkCallbackParameters() {
  const currentURL = new URL(window.location.href)
  currentURL.searchParams.delete('dingtalk_mfa')
  currentURL.searchParams.delete('dingtalk_error')
  currentURL.searchParams.delete('return_to')
  window.history.replaceState(window.history.state, '', `${currentURL.pathname}${currentURL.search}${currentURL.hash}`)
}

function consumeDingTalkCallbackResult() {
  const parameters = new URLSearchParams(window.location.search)
  const dingtalkError = parameters.get('dingtalk_error')
  const requiresMfa = parameters.get('dingtalk_mfa')
  const returnTo = parameters.get('return_to')

  if (!dingtalkError && requiresMfa !== '1') {
    return
  }

  abortQrSessionRequest()
  resetQrSession()
  clearDingTalkCallbackParameters()

  if (dingtalkError) {
    activeTab.value = 'qrcode'
    qrStatus.value = dingtalkError === 'AUTH_DINGTALK_QR_SESSION_INVALID' ? 'expired' : 'error'
    qrError.value = getDingTalkFailureMessage(dingtalkError)
    return
  }

  enterMfa({ returnTo, source: 'dingtalk' })
}

onMounted(() => {
  consumeDingTalkCallbackResult()
})

onBeforeUnmount(() => {
  abortQrSessionRequest()
  stopQrCountdown()
  resetQrSession()
  clearMfaLoginState()
})
</script>

<template>
  <main class="login-page">
    <section class="brand-panel" aria-labelledby="platform-heading">
      <div class="brand-glow brand-glow-top" aria-hidden="true"></div>
      <div class="brand-glow brand-glow-bottom" aria-hidden="true"></div>
      <div class="brand-grid" aria-hidden="true"></div>

      <header class="brand-logo">
        <span class="brand-logo-mark" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none">
            <path d="M7.5 7.2 12 4.6l4.5 2.6v5.2L12 15l-4.5-2.6V7.2Z" stroke="currentColor" stroke-width="1.8" />
            <path d="M7.5 12.4v4.4l4.5 2.6 4.5-2.6v-4.4M12 15v4.4" stroke="currentColor" stroke-width="1.8" />
          </svg>
        </span>
        <span>
          <strong>基础能力平台</strong>
          <small>Basic Capability Platform</small>
        </span>
      </header>

      <div class="brand-content">
        <p class="brand-eyebrow"><span></span>统一可信的数字化底座</p>
        <h1 id="platform-heading">
          让每一次访问<br />
          <span>安全、可控、可追溯</span>
        </h1>
        <p class="brand-description">
          提供统一身份认证、细粒度权限控制、全链路安全审计与集中配置能力，支撑合同、项目、报销等业务系统快速接入。
        </p>

        <ul class="capability-list" aria-label="平台核心能力">
          <li>
            <span class="capability-icon">
              <svg viewBox="0 0 24 24"><path d="M12 2 4.5 5.4v5.1c0 4.7 3.2 9 7.5 10.2 4.3-1.2 7.5-5.5 7.5-10.2V5.4L12 2Zm0 2.2 5.5 2.5v3.8c0 3.5-2.2 6.8-5.5 8-3.3-1.2-5.5-4.5-5.5-8V6.7L12 4.2Z" /></svg>
            </span>
            <span><strong>统一身份</strong><small>一个账号访问多个业务系统</small></span>
          </li>
          <li>
            <span class="capability-icon">
              <svg viewBox="0 0 24 24"><path d="M18 9h-1V7a5 5 0 0 0-10 0v2H6a2 2 0 0 0-2 2v9h16v-9a2 2 0 0 0-2-2Zm-9-2a3 3 0 1 1 6 0v2H9V7Zm9 11H6v-7h12v7Zm-6-1.5a2 2 0 1 0 0-4 2 2 0 0 0 0 4Z" /></svg>
            </span>
            <span><strong>权限管控</strong><small>角色、资源与数据权限统一治理</small></span>
          </li>
          <li>
            <span class="capability-icon">
              <svg viewBox="0 0 24 24"><path d="M5 3h14a2 2 0 0 1 2 2v15a1 1 0 0 1-1.5.87L17 19.43l-2.5 1.44a1 1 0 0 1-1 0L11 19.43l-2.5 1.44a1 1 0 0 1-1 0L5 19.43l-1.5.87A1 1 0 0 1 2 19.43V6a3 3 0 0 1 3-3Zm0 2a1 1 0 0 0-1 1v11.7l.5-.28a1 1 0 0 1 1 0L8 18.86l2.5-1.44a1 1 0 0 1 1 0l2.5 1.44 2.5-1.44a1 1 0 0 1 1 0l1.5.87V5H5Zm2 3h10v2H7V8Zm0 4h7v2H7v-2Z" /></svg>
            </span>
            <span><strong>审计与日志</strong><small>跨系统事件、操作与运行信息完整留痕</small></span>
          </li>
        </ul>
      </div>

      <footer class="brand-footer">
        <span>© {{ currentYear }} 基础能力平台</span>
        <span class="brand-footer-status"><i></i>安全服务运行中</span>
      </footer>
    </section>

    <section class="form-panel" aria-label="用户登录">
      <div class="mobile-brand">
        <span class="mobile-logo">基</span>
        <span>基础能力平台</span>
      </div>

      <div class="login-card">
        <header class="login-header">
          <span class="login-kicker">WELCOME BACK</span>
          <h2>欢迎回来</h2>
          <p>请验证您的身份以继续访问平台</p>
        </header>

        <section v-if="mfaRequired" class="mfa-panel" aria-labelledby="mfa-heading">
          <div class="mfa-heading">
            <span class="login-kicker">SECURITY CHECK</span>
            <h3 id="mfa-heading">完成安全验证</h3>
            <p>
              {{
                mfaLoginSource === 'dingtalk'
                  ? '钉钉身份已确认，请输入动态验证码或恢复码以继续登录。'
                  : '账号密码已验证，请输入动态验证码或恢复码以继续登录。'
              }}
            </p>
          </div>

          <form class="mfa-form" novalidate @submit.prevent="submitMfaLogin">
            <div class="field-group">
              <label for="mfa-code">动态验证码或恢复码</label>
              <div class="input-wrap">
                <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2 4.5 5.4v5.1c0 4.7 3.2 9 7.5 10.2 4.3-1.2 7.5-5.5 7.5-10.2V5.4L12 2Zm0 2.2 5.5 2.5v3.8c0 3.5-2.2 6.8-5.5 8-3.3-1.2-5.5-4.5-5.5-8V6.7L12 4.2Zm-1 4v5.6h2V8.2h-2Zm0 7.3v2h2v-2h-2Z" /></svg>
                <input
                  id="mfa-code"
                  v-model.trim="mfaCode"
                  type="text"
                  name="mfa-code"
                  inputmode="numeric"
                  autocomplete="one-time-code"
                  maxlength="64"
                  placeholder="输入 6 位动态码或恢复码"
                  :disabled="mfaSubmitting"
                />
              </div>
            </div>

            <p v-if="mfaError" class="form-message error" role="alert">
              <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20Zm0 18a8 8 0 1 1 0-16 8 8 0 0 1 0 16Zm-1-5h2v2h-2v-2Zm0-8h2v6h-2V7Z" /></svg>
              <span>{{ mfaError }}</span>
            </p>

            <button class="login-button" type="submit" :disabled="mfaSubmitting">
              <span v-if="mfaSubmitting" class="button-spinner" aria-hidden="true"></span>
              <span>{{ mfaSubmitting ? '正在验证…' : '完成验证' }}</span>
              <svg v-if="!mfaSubmitting" viewBox="0 0 24 24" aria-hidden="true"><path d="m13 5-1.4 1.4 4.6 4.6H4v2h12.2l-4.6 4.6L13 19l7-7-7-7Z" /></svg>
            </button>

            <button class="mfa-back" type="button" :disabled="mfaSubmitting" @click="returnToLoginChoices">
              返回其他登录方式
            </button>

            <div class="security-tip">
              <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2 4.5 5.4v5.1c0 4.7 3.2 9 7.5 10.2 4.3-1.2 7.5-5.5 7.5-10.2V5.4L12 2Zm0 2.2 5.5 2.5v3.8c0 3.5-2.2 6.8-5.5 8-3.3-1.2-5.5-4.5-5.5-8V6.7L12 4.2Zm-1 4v5.6h2V8.2h-2Zm0 7.3v2h2v-2h-2Z" /></svg>
              <span>
                {{
                  mfaLoginSource === 'password'
                    ? '本次 MFA 预认证凭据仅保存在当前页面内存，完成或离开页面后即清除'
                    : 'MFA 预认证凭据仅由安全 Cookie 管理，不会暴露给前端'
                }}
              </span>
            </div>
          </form>
        </section>

        <template v-else>
        <div class="login-tabs" role="tablist" aria-label="登录方式">
          <button
            id="password-tab"
            class="login-tab"
            :class="{ active: activeTab === 'password' }"
            type="button"
            role="tab"
            :aria-selected="activeTab === 'password'"
            aria-controls="password-panel"
            @click="switchTab('password')"
          >
            账号密码
          </button>
          <button
            id="qrcode-tab"
            class="login-tab"
            :class="{ active: activeTab === 'qrcode' }"
            type="button"
            role="tab"
            :aria-selected="activeTab === 'qrcode'"
            aria-controls="qrcode-panel"
            @click="switchTab('qrcode')"
          >
            钉钉扫码
          </button>
        </div>

        <form
          v-show="activeTab === 'password'"
          id="password-panel"
          class="password-panel"
          role="tabpanel"
          aria-labelledby="password-tab"
          novalidate
          @submit.prevent="submitLogin"
        >
          <div class="field-group">
            <label for="account">账号</label>
            <div class="input-wrap">
              <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 12a5 5 0 1 0 0-10 5 5 0 0 0 0 10Zm0-2a3 3 0 1 1 0-6 3 3 0 0 1 0 6Zm0 4c-4.4 0-8 2.2-8 5v2h16v-2c0-2.8-3.6-5-8-5Zm-5.8 5c.5-1.4 2.9-3 5.8-3 3 0 5.3 1.6 5.8 3H6.2Z" /></svg>
              <input
                id="account"
                v-model="account"
                type="text"
                name="account"
                placeholder="用户名 / 手机号 / 邮箱"
                autocomplete="username"
                maxlength="100"
                :disabled="submitting"
              />
            </div>
          </div>

          <div class="field-group">
            <label for="password">密码</label>
            <div class="input-wrap">
              <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M17 8V6a5 5 0 0 0-10 0v2H5a2 2 0 0 0-2 2v11h18V10a2 2 0 0 0-2-2h-2ZM9 6a3 3 0 1 1 6 0v2H9V6Zm10 13H5v-9h14v9Zm-7-2a2 2 0 1 0 0-4 2 2 0 0 0 0 4Z" /></svg>
              <input
                id="password"
                v-model="password"
                :type="passwordVisible ? 'text' : 'password'"
                name="password"
                placeholder="请输入登录密码"
                autocomplete="current-password"
                maxlength="128"
                :disabled="submitting"
              />
              <button
                class="password-toggle"
                type="button"
                :aria-label="passwordVisible ? '隐藏密码' : '显示密码'"
                :title="passwordVisible ? '隐藏密码' : '显示密码'"
                @click="passwordVisible = !passwordVisible"
              >
                <svg v-if="!passwordVisible" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5c-5 0-9.3 3-11 7 1.7 4 6 7 11 7s9.3-3 11-7c-1.7-4-6-7-11-7Zm0 12c-3.8 0-7.2-2-8.8-5C4.8 9 8.2 7 12 7s7.2 2 8.8 5c-1.6 3-5 5-8.8 5Zm0-8a3 3 0 1 0 0 6 3 3 0 0 0 0-6Zm0 4a1 1 0 1 1 0-2 1 1 0 0 1 0 2Z" /></svg>
                <svg v-else viewBox="0 0 24 24" aria-hidden="true"><path d="m3.3 2-1.4 1.4 3 3A12.7 12.7 0 0 0 1 12c1.7 4 6 7 11 7 1.8 0 3.5-.4 5-1l3.6 3.6 1.4-1.4L3.3 2ZM12 17c-3.8 0-7.2-2-8.8-5 .8-1.5 1.9-2.7 3.2-3.5l2.1 2.1A3.5 3.5 0 0 0 13.4 15l2 2c-1 .3-2.2.5-3.4.5V17Zm-1.6-4.5 2.1 2.1a1.6 1.6 0 0 1-2.1-2.1ZM12 7c3.8 0 7.2 2 8.8 5a9.5 9.5 0 0 1-2.1 2.8l1.4 1.4A12 12 0 0 0 23 12c-1.7-4-6-7-11-7-.8 0-1.6.1-2.4.2l1.7 1.7.7.1Zm.9 2.1 3 3a4 4 0 0 0-3-3Z" /></svg>
              </button>
            </div>
          </div>

          <div class="form-options">
            <label class="remember-option">
              <input v-model="rememberAccount" type="checkbox" :disabled="submitting" />
              <span>记住账号</span>
            </label>
            <button type="button" class="text-button" @click="showAccountHelp">忘记密码？</button>
          </div>

          <p v-if="formError" class="form-message error" role="alert">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20Zm0 18a8 8 0 1 1 0-16 8 8 0 0 1 0 16Zm-1-5h2v2h-2v-2Zm0-8h2v6h-2V7Z" /></svg>
            <span>{{ formError }}</span>
          </p>
          <p v-if="formSuccess" class="form-message success" role="status">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20Zm0 18a8 8 0 1 1 0-16 8 8 0 0 1 0 16Zm-2-3.6-3.7-3.7 1.4-1.4 2.3 2.3 6.3-6.3 1.4 1.4-7.7 7.7Z" /></svg>
            <span>{{ formSuccess }}</span>
          </p>

          <button class="login-button" type="submit" :disabled="submitting">
            <span v-if="submitting" class="button-spinner" aria-hidden="true"></span>
            <span>{{ submitting ? '正在验证…' : '登 录' }}</span>
            <svg v-if="!submitting" viewBox="0 0 24 24" aria-hidden="true"><path d="m13 5-1.4 1.4 4.6 4.6H4v2h12.2l-4.6 4.6L13 19l7-7-7-7Z" /></svg>
          </button>

          <div class="security-tip">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2 4.5 5.4v5.1c0 4.7 3.2 9 7.5 10.2 4.3-1.2 7.5-5.5 7.5-10.2V5.4L12 2Zm0 2.2 5.5 2.5v3.8c0 3.5-2.2 6.8-5.5 8-3.3-1.2-5.5-4.5-5.5-8V6.7L12 4.2Zm-1 4v5.6h2V8.2h-2Zm0 7.3v2h2v-2h-2Z" /></svg>
            <span>会话凭证由安全 Cookie 管理，前端不保存访问令牌</span>
          </div>

          <p class="contact-line">首次使用或无法登录？请联系平台管理员</p>
        </form>

        <section
          v-show="activeTab === 'qrcode'"
          id="qrcode-panel"
          class="qrcode-panel"
          role="tabpanel"
          aria-labelledby="qrcode-tab"
        >
          <div class="qr-heading">
            <h3>使用钉钉扫码登录</h3>
            <p>确认租户后，请在钉钉 App 中完成身份确认</p>
          </div>

          <div class="qr-tenant-field">
            <label for="dingtalk-tenant-id">租户 ID</label>
            <input
              id="dingtalk-tenant-id"
              v-model.trim="tenantId"
              type="text"
              maxlength="128"
              autocomplete="organization"
              placeholder="请输入管理员提供的租户 ID"
              :disabled="qrStatus === 'loading' || qrStatus === 'completing'"
              @input="handleTenantIdInput"
              @blur="ensureDingTalkQrSession"
              @keydown.enter.prevent="refreshQrCode"
            />
            <p>仅允许管理员已预先绑定的平台账号完成扫码登录。</p>
          </div>

          <div
            class="qr-shell"
            :class="{
              loading: qrStatus === 'loading',
              expired: qrIsExpired,
              error: qrStatus === 'error',
            }"
          >
            <div v-if="qrStatus === 'loading'" class="qr-state" role="status">
              <span class="qr-spinner" aria-hidden="true"></span>
              <strong>正在初始化二维码…</strong>
              <span>请稍候</span>
            </div>
            <div
              v-else-if="qrIsReady"
              :id="DINGTALK_QR_CONTAINER_ID"
              :key="qrSession.sessionId"
              ref="qrContainer"
              class="qr-frame"
              aria-label="钉钉扫码登录二维码"
            ></div>
            <div v-else-if="qrIsExpired" class="qr-state">
              <strong>二维码已失效</strong>
              <span>请重新获取后扫码</span>
              <button type="button" @click="refreshQrCode">立即刷新</button>
            </div>
            <div v-else-if="qrStatus === 'error'" class="qr-state qr-state-error" role="alert">
              <strong>二维码初始化失败</strong>
              <span>请检查租户信息或稍后重试</span>
              <button type="button" @click="refreshQrCode">重新获取</button>
            </div>
            <div v-else class="qr-state">
              <strong>等待获取二维码</strong>
              <span>输入租户 ID 后获取</span>
            </div>
          </div>

          <p v-if="qrIsReady" class="qr-status"><i></i>请使用钉钉扫码 · {{ countdown }}</p>
          <p v-else-if="qrStatus === 'loading'" class="qr-status"><i></i>正在生成安全扫码会话</p>
          <p v-else-if="qrStatus === 'completing'" class="qr-status"><i></i>登录已确认，正在进入平台…</p>
          <p v-else class="qr-status"><i></i>二维码有效期由认证服务控制</p>
          <p v-if="qrError" class="qr-inline-error" role="alert">{{ qrError }}</p>

          <button
            class="qr-refresh"
            type="button"
            :disabled="qrStatus === 'loading' || qrStatus === 'completing'"
            @click="refreshQrCode"
          >
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M17.7 6.3A8 8 0 0 0 4.3 9H2l3 3 3-3H6.3a6 6 0 1 1-.1 6.8l-1.7 1A8 8 0 1 0 17.7 6.3Z" /></svg>
            {{ qrIsReady ? '刷新二维码' : '获取二维码' }}
          </button>
          <p class="qr-note">扫码授权仅用于完成本次平台登录；授权完成后会在当前窗口进入平台。</p>
        </section>
        </template>
      </div>

      <footer class="form-footer">
        <span>统一身份认证</span><i></i><span>权限管控</span><i></i><span>安全审计</span>
      </footer>
    </section>
  </main>
</template>
