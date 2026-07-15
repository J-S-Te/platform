<script setup>
import { computed } from 'vue'

const props = defineProps({
  name: { type: String, required: true },
})

const icons = {
  logo: [
    'M7.5 7.2 12 4.6l4.5 2.6v5.2L12 15l-4.5-2.6V7.2Z',
    'M7.5 12.4v4.4l4.5 2.6 4.5-2.6v-4.4M12 15v4.4',
  ],
  settings: ['M19.1 13c.1-.3.1-.7.1-1s0-.7-.1-1l2-1.6-1.9-3.3-2.4 1a7.4 7.4 0 0 0-1.7-1l-.3-2.6H11l-.4 2.6c-.6.2-1.1.6-1.6 1l-2.4-1-2 3.3 2.1 1.6a6 6 0 0 0 0 2l-2 1.6 1.9 3.3 2.4-1c.5.4 1 .8 1.7 1l.3 2.6h3.8l.4-2.6c.6-.2 1.1-.6 1.6-1l2.4 1 2-3.3-2.1-1.6ZM13 16a4 4 0 1 1 0-8 4 4 0 0 1 0 8Zm0-2a2 2 0 1 0 0-4 2 2 0 0 0 0 4Z'],
  audit: ['M12 1 3 5v6c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V5l-9-4Zm-2 16-4-4 1.41-1.41L10 14.17l6.59-6.59L18 9l-8 8Z'],
  search: ['M10.8 4a6.8 6.8 0 1 0 4.3 12.1l4.4 4.4 1.4-1.4-4.4-4.4A6.8 6.8 0 0 0 10.8 4Zm0 2a4.8 4.8 0 1 1 0 9.6 4.8 4.8 0 0 1 0-9.6Z'],
  bell: ['M12 22a2.3 2.3 0 0 0 2.2-1.7H9.8A2.3 2.3 0 0 0 12 22Zm7-5-1.8-2.1V10a5.3 5.3 0 0 0-4.2-5.2V4a1 1 0 0 0-2 0v.8A5.3 5.3 0 0 0 6.8 10v4.9L5 17v1h14v-1Zm-11.3-1 .9-1V10a3.4 3.4 0 1 1 6.8 0v5l.9 1H7.7Z'],
  logout: ['M10 4H4v16h6v-2H6V6h4V4Zm5.6 3.6L14.2 9l2 2H9v2h7.2l-2 2 1.4 1.4L20 12l-4.4-4.4Z'],
  menu: ['M4 6h16v2H4V6Zm0 5h16v2H4v-2Zm0 5h16v2H4v-2Z'],
  close: ['m6.4 5 5.6 5.6L17.6 5 19 6.4 13.4 12l5.6 5.6-1.4 1.4-5.6-5.6L6.4 19 5 17.6l5.6-5.6L5 6.4 6.4 5Z'],
  export: ['M19 9h-4V3H9v6H5l7 7 7-7ZM5 18v2h14v-2H5Z'],
  reset: ['M12 5V1L7 6l5 5V7c3.3 0 6 2.7 6 6 0 1-.3 2-.8 2.8l1.5 1.5A8 8 0 0 0 12 5Zm-6.7.7A8 8 0 0 0 12 21v-4c-3.3 0-6-2.7-6-6 0-1 .3-2 .8-2.8L5.3 5.7Z'],
  save: ['m9.2 16.6-4-4 1.4-1.4 2.6 2.6 8.2-8.2L18.8 7l-9.6 9.6Z'],
  chevron: ['m9 18 6-6-6-6-1.4 1.4 4.6 4.6-4.6 4.6L9 18Z'],
  info: ['M11 10h2v7h-2v-7Zm1-7a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 16a7 7 0 1 1 0-14 7 7 0 0 1 0 14Zm-1-11V6h2v2h-2Z'],
}

const paths = computed(() => icons[props.name] || icons.info)
const outlineIcons = new Set(['logo'])
const isOutline = computed(() => outlineIcons.has(props.name))
</script>

<template>
  <svg viewBox="0 0 24 24" :fill="isOutline ? 'none' : 'currentColor'" aria-hidden="true">
    <path v-for="path in paths" :key="path" :d="path" :stroke="isOutline ? 'currentColor' : 'none'" :stroke-width="isOutline ? '1.8' : '0'" />
  </svg>
</template>
