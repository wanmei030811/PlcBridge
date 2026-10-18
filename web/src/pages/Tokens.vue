<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { listTokens, createToken, revokeToken, type Token, type CreatedToken } from '@/api/auth'
import IconButton from '@/components/IconButton.vue'

const items = ref<Token[]>([])
const loading = ref(false)
const dialogOpen = ref(false)
const newName = ref('')

// 一次性明文展示
const revealOpen = ref(false)
const revealedToken = ref<CreatedToken | null>(null)
const savedChecked = ref(false)
const revealCopied = ref(false)

async function refresh() {
  loading.value = true
  try {
    const r = await listTokens()
    items.value = r.items
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '加载失败')
  } finally { loading.value = false }
}

async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try { await navigator.clipboard.writeText(text); return true } catch { /* fallthrough */ }
  }
  // fallback：临时 textarea + execCommand
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch { return false }
}

async function onCreate() {
  if (!newName.value.trim()) { ElMessage.warning('请填写用途名'); return }
  try {
    const tk = await createToken(newName.value)
    newName.value = ''
    dialogOpen.value = false
    // 弹出一次性明文展示对话框（必须勾选"我已保存"才能关闭）
    revealedToken.value = tk
    revealCopied.value = false
    savedChecked.value = false
    revealOpen.value = true
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '创建失败')
  }
}

async function onCopyRevealed() {
  if (!revealedToken.value) return
  const ok = await copyText(revealedToken.value.token)
  if (ok) {
    revealCopied.value = true
    ElMessage.success('已复制到剪贴板')
  } else {
    ElMessage.warning('复制失败，请手动选中后 Ctrl+C')
  }
}

function closeReveal() {
  if (!savedChecked.value) {
    ElMessage.warning('请先勾选确认已保存')
    return
  }
  revealOpen.value = false
  revealedToken.value = null
  revealCopied.value = false
  savedChecked.value = false
}

async function onRevoke(t: Token) {
  try {
    await revokeToken(t.id)
    ElMessage.success('已撤销')
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '撤销失败')
  }
}

function maskToken(t: string): string {
  if (!t) return ''
  if (t.length <= 8) return '****'
  return t.slice(0, 4) + '****' + t.slice(-4)
}

onMounted(refresh)
</script>

<template>
  <div>
    <div style="display:flex; justify-content:space-between; margin-bottom:16px">
      <h2 style="margin:0">API Token（plcbridge §8.6）</h2>
      <div>
        <IconButton name="refresh" tip="刷新" :loading="loading" @click="refresh" />
        <IconButton name="plus" type="primary" tip="新建 Token" @click="dialogOpen = true" />
      </div>
    </div>

    <el-alert
      type="info" show-icon :closable="false" style="margin-bottom:16px"
      description="Bearer Token 用于外部系统调用 plcbridge 的回写 API。请在 192.168.x.x:8082/api/v1/write 接口使用 Header 'Authorization: Bearer <token>'。"
    />

    <el-table :data="items" v-loading="loading" stripe>
      <el-table-column prop="name" label="用途" />
      <el-table-column label="Token" min-width="220">
        <template #default="{ row }">
          <template v-if="row.revoked_at">
            <code style="color:#999; font-style:italic">已撤销</code>
          </template>
          <template v-else>
            <code style="background:#f3f3f3; padding:2px 6px; border-radius:3px; font-size:12px">{{ maskToken(row.token) }}</code>
          </template>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="120">
        <template #default="{ row }">
          <el-tag v-if="row.revoked_at" type="danger">已撤销</el-tag>
          <el-tag v-else type="success">活跃</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="created_at" label="创建于" width="200" />
      <el-table-column label="操作" width="100" align="right">
        <template #default="{ row }">
          <el-popconfirm v-if="!row.revoked_at" title="撤销后不可恢复，确认?" @confirm="onRevoke(row)">
            <template #reference>
              <IconButton name="x-circle" type="danger" text tip="撤销" :icon-size="14" no-tooltip />
            </template>
          </el-popconfirm>
        </template>
      </el-table-column>
    </el-table>

    <!-- 一次性明文展示对话框 -->
    <el-dialog
      v-model="revealOpen"
      title="Token 已生成"
      width="560px"
      :close-on-click-modal="false"
      :close-on-press-escape="false"
      :show-close="false"
    >
      <el-alert
        v-if="revealedToken"
        type="warning" show-icon :closable="false"
        style="margin-bottom:12px"
        :title="'明文仅显示一次。请立即复制到安全位置（密码管理器 / 配置文件）。'"
      />
      <div v-if="revealedToken" class="token-reveal">
        <div class="token-name">{{ revealedToken.name }}</div>
        <div class="token-row">
          <code class="token-text">{{ revealedToken.token }}</code>
          <el-button size="small" :type="revealCopied ? 'success' : 'primary'" @click="onCopyRevealed">
            {{ revealCopied ? '已复制' : '复制' }}
          </el-button>
        </div>
        <el-checkbox v-model="savedChecked" style="margin-top:16px">
          我已保存该 Token 明文，关闭后将无法再次查看
        </el-checkbox>
      </div>
      <template #footer>
        <el-button type="primary" :disabled="!savedChecked" @click="closeReveal">我已保存，关闭</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="dialogOpen" title="新建 Token" width="480px">
      <el-form label-width="100px">
        <el-form-item label="用途名" required><el-input v-model="newName" placeholder="例：MES-回写-2026" /></el-form-item>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="dialogOpen = false" />
        <IconButton name="check" type="primary" tip="生成" @click="onCreate" />
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.token-reveal { padding: 8px 0; }
.token-name { font-weight: 600; margin-bottom: 8px; color: #111827; }
.token-row { display: flex; align-items: center; gap: 8px; }
.token-text {
  flex: 1; min-width: 0; word-break: break-all;
  background: #0f172a; color: #f1f5f9;
  padding: 8px 10px; border-radius: 4px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px; line-height: 1.4;
  user-select: all;
}
</style>

