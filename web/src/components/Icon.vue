<script setup lang="ts">
/**
 * 系统统一图标库入口
 * - 风格:Feather Icons (stroke=2, viewBox 0 0 24 24)
 * - 全部 path 数据在 iconPaths.ts 维护
 * - 当图标升级或 stroke-width 改变时,只改这里一处即可
 */
import { iconPaths, type IconName } from './iconPaths'

defineProps<{
  name: IconName
  size?: number
  color?: string
}>()

const strokeWidth = 2
</script>

<template>
  <svg
    xmlns="http://www.w3.org/2000/svg"
    :width="size ?? 14"
    :height="size ?? 14"
    viewBox="0 0 24 24"
    fill="none"
    :stroke="color ?? 'currentColor'"
    :stroke-width="strokeWidth"
    stroke-linecap="round"
    stroke-linejoin="round"
    v-html="iconPaths[name]"
  />
</template>
