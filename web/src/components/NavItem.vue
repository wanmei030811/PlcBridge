<script setup lang="ts">
/**
 * 通用的导航项组件。
 * - 有 href 时渲染 <a target="_blank">（外部链接 / Swagger 等）
 * - 无 href 时渲染 <router-link>
 */
const props = defineProps<{
  path: string
  icon: string
  label: string
  collapsed?: boolean
  href?: string
  active?: boolean
}>()
</script>

<template>
  <a
    v-if="props.href"
    :href="props.href"
    target="_blank"
    rel="noopener noreferrer"
    class="nav-item nav-external"
  >
    <span class="nav-icon">
      <Icon :name="props.icon as any" :size="18" />
    </span>
    <span class="nav-label">{{ props.label }}</span>
  </a>
  <router-link
    v-else
    :to="props.path"
    class="nav-item"
    :class="{ active: props.active }"
  >
    <span class="nav-icon">
      <Icon :name="props.icon as any" :size="18" />
    </span>
    <span class="nav-label">{{ props.label }}</span>
  </router-link>
</template>
