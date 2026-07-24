import { createRouter, createWebHistory } from 'vue-router'
import LoginView from '@/modules/platform/auth/views/LoginView.vue'
import PlatformConsoleView from '@/modules/platform/views/PlatformConsoleView.vue'
import SubsystemPortalView from '@/modules/platform/views/SubsystemPortalView.vue'

const settingsSections = new Set([
  'base',
  'iam',
  'login-targets',
  'notify',
  'security',
  'observability',
  'files',
  'dict',
])

function normalizeSettingsSection(section) {
  return settingsSections.has(section) ? section : 'iam'
}

const router = createRouter({
  history: createWebHistory(),
  scrollBehavior() {
    return { top: 0 }
  },
  routes: [
    {
      path: '/',
      redirect: { name: 'portal' },
    },
    {
      path: '/login',
      alias: '/login.html',
      name: 'login',
      component: LoginView,
      meta: { title: '登录' },
    },
    {
      path: '/portal',
      name: 'portal',
      component: SubsystemPortalView,
      meta: { title: '子系统门户' },
    },
    {
      path: '/settings/:section?',
      name: 'settings',
      component: PlatformConsoleView,
      meta: { title: '系统设置' },
    },
    {
      path: '/audit',
      name: 'audit',
      component: PlatformConsoleView,
      meta: { title: '审计日志' },
    },
    {
      path: '/:pathMatch(.*)*',
      redirect: { name: 'portal' },
    },
  ],
})

router.beforeEach((to) => {
  if (to.name !== 'settings') {
    return true
  }

  const section = normalizeSettingsSection(to.params.section)
  if (to.params.section === section) {
    return true
  }

  return {
    name: 'settings',
    params: { section },
    query: to.query,
    hash: to.hash,
  }
})

router.afterEach((to) => {
  document.title = `${to.meta.title || '基础能力平台'} · 基础能力平台`
})

export default router
