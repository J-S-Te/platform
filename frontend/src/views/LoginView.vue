<script setup>
import { computed, onBeforeUnmount, ref } from 'vue'
import { loginWithPassword } from '@/api/auth'

const STORAGE_KEY = 'basic-platform.remembered-account'
const LOGIN_SUCCESS_URL = import.meta.env.VITE_LOGIN_SUCCESS_URL || '/'

const activeTab = ref('password')
const account = ref(localStorage.getItem(STORAGE_KEY) || '')
const password = ref('')
const rememberAccount = ref(Boolean(account.value))
const passwordVisible = ref(false)
const submitting = ref(false)
const formError = ref('')
const formSuccess = ref('')
const qrVersion = ref(1)
const qrSeconds = ref(300)
let qrTimer = null

const currentYear = new Date().getFullYear()
const countdown = computed(() => {
  const minutes = String(Math.floor(qrSeconds.value / 60)).padStart(2, '0')
  const seconds = String(qrSeconds.value % 60).padStart(2, '0')
  return `${minutes}:${seconds}`
})

function switchTab(tab) {
  activeTab.value = tab
  formError.value = ''
  formSuccess.value = ''

  if (tab === 'qrcode') {
    startQrCountdown()
  } else {
    stopQrCountdown()
  }
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
    const result = await loginWithPassword({
      account: account.value.trim(),
      password: password.value,
    })

    if (rememberAccount.value) {
      localStorage.setItem(STORAGE_KEY, account.value.trim())
    } else {
      localStorage.removeItem(STORAGE_KEY)
    }

    formSuccess.value = result.message || result.msg || '登录成功，正在进入平台…'

    const redirectUrl =
      result?.data?.redirect_url ||
      result?.data?.redirectUrl ||
      result?.redirect_url ||
      LOGIN_SUCCESS_URL

    window.setTimeout(() => window.location.assign(redirectUrl), 450)
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

function stopQrCountdown() {
  if (qrTimer) {
    window.clearInterval(qrTimer)
    qrTimer = null
  }
}

function startQrCountdown() {
  stopQrCountdown()

  if (qrSeconds.value <= 0) {
    qrSeconds.value = 300
  }

  qrTimer = window.setInterval(() => {
    if (qrSeconds.value <= 1) {
      qrSeconds.value = 0
      stopQrCountdown()
      return
    }

    qrSeconds.value -= 1
  }, 1000)
}

function refreshQrCode() {
  qrVersion.value += 1
  qrSeconds.value = 300
  startQrCountdown()
}

onBeforeUnmount(stopQrCountdown)
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
            <p>请在钉钉 App 中完成身份确认</p>
          </div>

          <div class="qr-shell" :class="{ expired: qrSeconds === 0 }">
            <svg :key="qrVersion" class="qr-code" viewBox="0 0 160 160" aria-label="钉钉登录二维码占位图" role="img">
              <rect width="160" height="160" rx="3" fill="#fff" />
              <g fill="#0f172a">
                <path d="M8 8h42v42H8V8Zm8 8v26h26V16H16Zm7 7h12v12H23V23ZM110 8h42v42h-42V8Zm8 8v26h26V16h-26Zm7 7h12v12h-12V23ZM8 110h42v42H8v-42Zm8 8v26h26v-26H16Zm7 7h12v12H23v-12Z" />
                <path d="M60 8h9v9h-9V8Zm17 0h9v17h-9V8Zm18 8h9v9h-9v-9ZM59 34h18v9H59v-9Zm27 0h9v17h-9V34Zm-26 25h9v9h-9v-9Zm17 0h17v9H77v-9Zm26 0h9v17h-9V59Zm18 0h9v9h-9v-9Zm17 8h9v9h-9v-9ZM59 80h17v9H59v-9Zm25 8h9v9h-9v-9Zm18-9h9v18h-9V79Zm18 8h18v9h-18v-9Zm-61 17h9v18h-9v-18Zm18 0h18v9H77v-9Zm26 8h9v9h-9v-9Zm18-9h9v18h-9v-18Zm18 8h18v9h-18v-9Zm-69 18h18v9H60v-9Zm26 0h9v18h-9v-18Zm18 9h18v9h-18v-9Zm26-9h9v18h-9v-18Z" />
              </g>
            </svg>
            <div v-if="qrSeconds === 0" class="qr-expired">
              <strong>二维码已失效</strong>
              <button type="button" @click="refreshQrCode">立即刷新</button>
            </div>
            <span class="ding-mark" aria-hidden="true">钉</span>
          </div>

          <p class="qr-status"><i></i>等待扫码 · {{ countdown }}</p>
          <button class="qr-refresh" type="button" @click="refreshQrCode">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M17.7 6.3A8 8 0 0 0 4.3 9H2l3 3 3-3H6.3a6 6 0 1 1-.1 6.8l-1.7 1A8 8 0 1 0 17.7 6.3Z" /></svg>
            刷新二维码
          </button>
          <p class="qr-note">启用扫码登录前，需由管理员在认证服务中配置钉钉应用。</p>
        </section>
      </div>

      <footer class="form-footer">
        <span>统一身份认证</span><i></i><span>权限管控</span><i></i><span>安全审计</span>
      </footer>
    </section>
  </main>
</template>
