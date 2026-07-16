<script setup>
import { computed } from 'vue'
import LoginView from './views/LoginView.vue'
import PlatformConsoleView from './views/PlatformConsoleView.vue'

const currentPath = computed(() => window.location.pathname.toLowerCase())
const isLoginPage = computed(
  () => currentPath.value === '/login' || currentPath.value.endsWith('/login.html'),
)
</script>

<template>
  <LoginView v-if="isLoginPage" />
  <PlatformConsoleView v-else />
</template>
