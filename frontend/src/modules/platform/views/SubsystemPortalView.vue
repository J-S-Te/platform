<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { AuthError, getCurrentPrincipal, logoutCurrentSession } from '@/modules/platform/auth/api/auth'
import ConsoleIcon from '@/modules/platform/shared/components/ConsoleIcon.vue'
import '@/modules/platform/styles/subsystem-portal.css'

const router = useRouter()
const canvasRef = ref(null)
const userMenuRef = ref(null)
const toast = ref({ visible: false, message: '', type: '' })
const currentPrincipal = ref(null)
const isPrincipalLoading = ref(true)
const isLoggingOut = ref(false)
const userMenuOpen = ref(false)
const principalLoadFailed = ref(false)

const userDisplayName = computed(() => {
  const principal = currentPrincipal.value
  if (principal?.user?.name) {
    return principal.user.name
  }
  if (principal?.account?.name) {
    return principal.account.name
  }
  return principalLoadFailed.value ? '用户信息暂不可用' : '当前登录用户'
})

const accountDisplayName = computed(() => {
  const account = currentPrincipal.value?.account
  if (!account) {
    return isPrincipalLoading.value ? '正在读取账号信息' : '账号信息暂不可用'
  }
  return account.code || account.name || '账号信息暂不可用'
})

const userAvatarText = computed(() => {
  const name = userDisplayName.value.trim()
  return Array.from(name).slice(0, 2).join('') || '用户'
})

const roleNames = computed(() => {
  const roles = currentPrincipal.value?.roles
  if (!Array.isArray(roles) || roles.length === 0) {
    return '未分配角色'
  }
  return roles.map((role) => role.name || role.code || role.id).filter(Boolean).join('、') || '未分配角色'
})

// 子系统名称和待办数量沿用“子系统门户_升级版.html”。
// 基础能力平台使用当前已实现的系统设置路由；其余子系统尚未在项目中登记跳转地址。
const subsystems = [
  {
    key: 'basic-platform',
    name: '基础能力平台',
    icon: 'settings',
    allowed: true,
    route: { name: 'settings', params: { section: 'iam' } },
  },
  { key: 'customer-opportunity', name: '客户与商机管理', todo: 8, icon: 'user', allowed: true },
  { key: 'contract', name: '合同管理系统', todo: 5, icon: 'audit', allowed: true },
  { key: 'project-service', name: '项目与服务管理', todo: 3, icon: 'save', allowed: true },
  { key: 'report', name: '报告管理系统', icon: 'dashboard', allowed: true },
  { key: 'invoice-settlement', name: '开票与结算系统', todo: 2, icon: 'account', allowed: true },
]

let toastTimer = 0
let animationFrame = 0
let resizeCanvas = null
let particles = []

function showToast(message, type = 'enter') {
  toast.value = { visible: true, message, type }
  window.clearTimeout(toastTimer)
  toastTimer = window.setTimeout(() => {
    toast.value.visible = false
  }, 2200)
}

async function loadCurrentPrincipal() {
  isPrincipalLoading.value = true
  principalLoadFailed.value = false

  try {
    currentPrincipal.value = await getCurrentPrincipal()
  } catch (error) {
    principalLoadFailed.value = true

    if (error instanceof AuthError && error.status === 401) {
      await router.replace({ name: 'login' })
      return
    }

    showToast(error.message || '当前登录用户信息读取失败，请稍后重试。', 'deny')
  } finally {
    isPrincipalLoading.value = false
  }
}

function toggleUserMenu() {
  userMenuOpen.value = !userMenuOpen.value
}

function closeUserMenuWhenClickOutside(event) {
  if (userMenuRef.value && !userMenuRef.value.contains(event.target)) {
    userMenuOpen.value = false
  }
}

function closeUserMenuOnEscape(event) {
  if (event.key === 'Escape') {
    userMenuOpen.value = false
  }
}

async function logoutApplication() {
  if (isLoggingOut.value) {
    return
  }

  isLoggingOut.value = true

  try {
    await logoutCurrentSession()
    userMenuOpen.value = false
    await router.replace({ name: 'login' })
  } catch (error) {
    if (error instanceof AuthError && error.status === 401) {
      await router.replace({ name: 'login' })
      return
    }

    showToast(error.message || '退出登录失败，请稍后重试。', 'deny')
  } finally {
    isLoggingOut.value = false
  }
}

function openSubsystem(subsystem) {
  if (!subsystem.allowed) {
    showToast(`您暂无「${subsystem.name}」的访问权限`, 'deny')
    return
  }

  if (subsystem.route) {
    // 使用当前 Vue Router 的解析结果打开新标签页，确保部署在子路径时仍能得到正确的完整地址。
    const target = router.resolve(subsystem.route)
    window.open(target.href, '_blank', 'noopener')
    return
  }

  // 原始 HTML 仅展示进入提示，未登记其他子系统地址；此处保持该行为，避免构造不存在的路由。
  showToast(`正在进入「${subsystem.name}」…`)
}

function handleCardPointerMove(event) {
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    return
  }

  const card = event.currentTarget
  const bounds = card.getBoundingClientRect()
  const x = (event.clientX - bounds.left) / bounds.width
  const y = (event.clientY - bounds.top) / bounds.height
  const rotateX = (y - 0.5) * -8
  const rotateY = (x - 0.5) * 10

  card.style.transform = `perspective(900px) rotateX(${rotateX}deg) rotateY(${rotateY}deg) translateY(-8px)`
  card.style.setProperty('--portal-mouse-x', `${x * 100}%`)
  card.style.setProperty('--portal-mouse-y', `${y * 100}%`)
}

function resetCardTransform(event) {
  const card = event.currentTarget
  card.style.transform = ''
  card.style.setProperty('--portal-mouse-x', '50%')
  card.style.setProperty('--portal-mouse-y', '50%')
}

function startParticleBackground() {
  const canvas = canvasRef.value
  const context = canvas?.getContext('2d')
  if (!canvas || !context) {
    return
  }

  const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches
  let width = 0
  let height = 0

  resizeCanvas = () => {
    const pixelRatio = Math.min(window.devicePixelRatio || 1, 2)
    width = window.innerWidth
    height = window.innerHeight
    canvas.width = Math.floor(width * pixelRatio)
    canvas.height = Math.floor(height * pixelRatio)
    canvas.style.width = `${width}px`
    canvas.style.height = `${height}px`
    context.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0)

    const particleCount = Math.min(90, Math.floor((width * height) / 18000))
    particles = Array.from({ length: particleCount }, () => ({
      x: Math.random() * width,
      y: Math.random() * height,
      velocityX: (Math.random() - 0.5) * 0.22,
      velocityY: (Math.random() - 0.5) * 0.22,
    }))
  }

  const draw = () => {
    context.clearRect(0, 0, width, height)

    particles.forEach((particle, index) => {
      if (!reducedMotion) {
        particle.x += particle.velocityX
        particle.y += particle.velocityY

        if (particle.x < 0 || particle.x > width) {
          particle.velocityX *= -1
        }
        if (particle.y < 0 || particle.y > height) {
          particle.velocityY *= -1
        }
      }

      context.beginPath()
      context.arc(particle.x, particle.y, 1.2, 0, Math.PI * 2)
      context.fillStyle = 'rgba(147, 197, 253, 0.52)'
      context.fill()

      for (let nextIndex = index + 1; nextIndex < particles.length; nextIndex += 1) {
        const nextParticle = particles[nextIndex]
        const distanceX = particle.x - nextParticle.x
        const distanceY = particle.y - nextParticle.y
        const distanceSquared = distanceX * distanceX + distanceY * distanceY
        const connectionDistance = 145

        if (distanceSquared >= connectionDistance * connectionDistance) {
          continue
        }

        const opacity = 0.12 * (1 - Math.sqrt(distanceSquared) / connectionDistance)
        context.beginPath()
        context.moveTo(particle.x, particle.y)
        context.lineTo(nextParticle.x, nextParticle.y)
        context.strokeStyle = `rgba(96, 165, 250, ${opacity})`
        context.lineWidth = 1
        context.stroke()
      }
    })

    if (!reducedMotion) {
      animationFrame = window.requestAnimationFrame(draw)
    }
  }

  resizeCanvas()
  window.addEventListener('resize', resizeCanvas)
  draw()
}

onMounted(() => {
  startParticleBackground()
  loadCurrentPrincipal()
  document.addEventListener('click', closeUserMenuWhenClickOutside)
  document.addEventListener('keydown', closeUserMenuOnEscape)
})

onBeforeUnmount(() => {
  window.clearTimeout(toastTimer)
  window.cancelAnimationFrame(animationFrame)
  document.removeEventListener('click', closeUserMenuWhenClickOutside)
  document.removeEventListener('keydown', closeUserMenuOnEscape)
  if (resizeCanvas) {
    window.removeEventListener('resize', resizeCanvas)
  }
})
</script>

<template>
  <main class="subsystem-portal" aria-label="子系统门户">
    <canvas ref="canvasRef" class="subsystem-portal__particles" aria-hidden="true"></canvas>
    <div class="subsystem-portal__grid" aria-hidden="true"></div>
    <div class="subsystem-portal__glow subsystem-portal__glow--top" aria-hidden="true"></div>
    <div class="subsystem-portal__glow subsystem-portal__glow--bottom" aria-hidden="true"></div>

    <header class="subsystem-portal__header">
      <div class="subsystem-portal__brand">
        <span class="subsystem-portal__logo"><ConsoleIcon name="logo" /></span>
        <span>
          <strong>基础能力平台</strong>
          <small>BASIC PLATFORM</small>
        </span>
      </div>
      <div ref="userMenuRef" class="subsystem-portal__user-menu">
        <button
          class="subsystem-portal__user-trigger"
          type="button"
          aria-haspopup="dialog"
          aria-controls="portal-user-panel"
          :aria-expanded="userMenuOpen"
          @click="toggleUserMenu"
        >
          <span class="subsystem-portal__user-avatar" aria-hidden="true">{{ userAvatarText }}</span>
          <span class="subsystem-portal__user-summary">
            <strong>{{ userDisplayName }}</strong>
            <small>{{ accountDisplayName }}</small>
          </span>
          <ConsoleIcon class="subsystem-portal__user-chevron" :class="{ 'is-open': userMenuOpen }" name="chevron" />
        </button>

        <Transition name="portal-user-panel">
          <section v-if="userMenuOpen" id="portal-user-panel" class="subsystem-portal__user-panel" aria-label="个人信息">
            <p class="subsystem-portal__user-panel-title">个人信息</p>

            <p v-if="isPrincipalLoading" class="subsystem-portal__user-panel-status">正在读取当前登录用户信息…</p>
            <template v-else-if="currentPrincipal">
              <dl class="subsystem-portal__user-details">
                <div>
                  <dt>用户名</dt>
                  <dd>{{ currentPrincipal.user?.name || '—' }}</dd>
                </div>
                <div>
                  <dt>账号</dt>
                  <dd>{{ currentPrincipal.account?.code || currentPrincipal.account?.name || '—' }}</dd>
                </div>
                <div>
                  <dt>租户</dt>
                  <dd>{{ currentPrincipal.tenant?.name || currentPrincipal.tenant?.code || '—' }}</dd>
                </div>
                <div>
                  <dt>角色</dt>
                  <dd>{{ roleNames }}</dd>
                </div>
              </dl>
            </template>
            <p v-else class="subsystem-portal__user-panel-status is-error">当前用户信息暂不可用。</p>

            <button class="subsystem-portal__logout-button" type="button" :disabled="isLoggingOut" @click="logoutApplication">
              {{ isLoggingOut ? '正在退出…' : '退出应用系统' }}
            </button>
          </section>
        </Transition>
      </div>
    </header>

    <section class="subsystem-portal__content" aria-labelledby="portal-title">
      <div class="subsystem-portal__title-group">
        <p class="subsystem-portal__eyebrow">UNIFIED ACCESS</p>
        <h1 id="portal-title">子系统门户</h1>
        <p>请选择需要访问的业务子系统</p>
      </div>

      <div class="subsystem-portal__cards" aria-label="子系统列表">
        <button
          v-for="(subsystem, index) in subsystems"
          :key="subsystem.key"
          class="subsystem-card"
          :style="{ '--portal-card-delay': `${(index + 1) * 0.06}s` }"
          type="button"
          :aria-label="`进入${subsystem.name}`"
          @click="openSubsystem(subsystem)"
          @pointermove="handleCardPointerMove"
          @pointerleave="resetCardTransform"
        >
          <span v-if="subsystem.todo" class="subsystem-card__todo">待办 {{ subsystem.todo }}</span>
          <span class="subsystem-card__icon"><ConsoleIcon :name="subsystem.icon" /></span>
          <span class="subsystem-card__name">{{ subsystem.name }}</span>
          <span class="subsystem-card__action">进入系统 <ConsoleIcon name="chevron" /></span>
        </button>
      </div>
    </section>

    <footer class="subsystem-portal__footer">
      <span>建议分辨率：1920×1080 · 浏览器：Chrome / Edge 最新版</span>
      <span>© 2026 V2.1.0</span>
    </footer>

    <Transition name="portal-toast">
      <div v-if="toast.visible" class="subsystem-portal__toast" :class="`is-${toast.type}`" role="status" aria-live="polite">
        {{ toast.message }}
      </div>
    </Transition>
  </main>
</template>
