<script setup lang="ts">
/**
 * 系统统一图标按钮
 * - 默认纯图标 + tooltip
 * - 支持 primary/success/warning/danger/info/default 语义(沿用 Element Plus)
 * - 支持 circle / square / text 三种变体
 * - 默认尺寸 16,按钮 size 默认 small
 */
import { computed } from 'vue'
import Icon from '@/components/Icon.vue'
import { type IconName } from '@/components/iconPaths'

const props = withDefaults(defineProps<{
  name: IconName
  tip?: string
  type?: 'primary' | 'success' | 'warning' | 'danger' | 'info' | 'default'
  plain?: boolean
  round?: boolean
  circle?: boolean
  text?: boolean
  bg?: boolean
  size?: 'small' | 'default' | 'large'
  iconSize?: number
  disabled?: boolean
  loading?: boolean
  /** 禁用 tooltip：用于 el-popconfirm 包裹场景，避免 tooltip 拦截 click 事件导致 confirm 不触发 */
  noTooltip?: boolean
}>(), {
  type: 'default',
  plain: false,
  round: false,
  circle: false,
  text: false,
  bg: false,
  size: 'small',
  iconSize: 16,
  disabled: false,
  loading: false,
  tip: '',
  noTooltip: false
})

defineEmits<{ (e: 'click', evt: MouseEvent): void }>()

// text 变体下,不同 type 决定图标颜色;否则继承 currentColor
const iconColor = computed(() => {
  if (!props.text) return 'currentColor'
  if (props.type === 'danger') return '#f56c6c'
  if (props.type === 'primary') return '#409eff'
  if (props.type === 'success') return '#67c23a'
  if (props.type === 'warning') return '#e6a23c'
  if (props.type === 'info') return '#909399'
  return '#64748b'
})
</script>

<template>
  <el-tooltip
    v-if="!noTooltip"
    :content="tip"
    :disabled="!tip"
    placement="top"
    :show-after="120"
  >
    <el-button
      :type="type"
      :plain="plain"
      :round="round"
      :circle="circle"
      :text="text"
      :bg="bg"
      :size="size"
      :disabled="disabled"
      :loading="loading"
      class="icon-btn"
      @click="(e: MouseEvent) => $emit('click', e)"
    >
      <Icon :name="name" :size="iconSize" :color="iconColor" />
    </el-button>
  </el-tooltip>
  <el-button
    v-else
    :type="type"
    :plain="plain"
    :round="round"
    :circle="circle"
    :text="text"
    :bg="bg"
    :size="size"
    :disabled="disabled"
    :loading="loading"
    class="icon-btn"
    @click="(e: MouseEvent) => $emit('click', e)"
  >
    <Icon :name="name" :size="iconSize" :color="iconColor" />
  </el-button>
</template>

<style scoped>
.icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 6px;
  min-height: 28px;
}
.icon-btn :deep(.el-icon) {
  font-size: 16px;
}
.icon-btn.is-circle {
  border-radius: 50%;
}
</style>
