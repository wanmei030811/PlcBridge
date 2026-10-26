<script setup lang="ts">
// 根组件只负责挂载路由出口。
//
// 全局快捷键（Ctrl+N / Ctrl+S / Ctrl+R / Escape / Ctrl+D / Ctrl+Shift+C / Ctrl+K）
// 统一挂在布局层 Default.vue —— 那是"已登录的 app shell"，
// 在 App.vue 挂会让登录页也响应这些键（未登录时还会被路由守卫弹回），语义不对。
</script>

<template>
  <router-view />
</template>

<style>
html, body, #app {
  height: 100%;
  margin: 0;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
}
</style>
