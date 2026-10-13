<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed, reactive } from 'vue'
import { useWebSocket } from '../composables/useWebSocket'

interface DiscoveredDevice {
  address: string
  types: string[]
  scopes: string[]
  xaddrs: string[]
  metadata_version: number
}

interface DeviceInfo {
  Manufacturer: string
  Model: string
  FirmwareVersion: string
  SerialNumber: string
  HardwareID: string
}

interface DeviceState {
  connected: boolean
  streaming: boolean
  address: string
  snapshot_mode: boolean
}

const devices = ref<DiscoveredDevice[]>([])
const deviceState = ref<DeviceState>({ connected: false, streaming: false, address: '未连接', snapshot_mode: false })
const discovering = ref(false)
const connecting = ref(false)
const discoverError = ref('')
const showLLMSettings = ref(false)
const expandedAddress = ref<string | null>(null)
const deviceInfoMap = reactive<Record<string, { loading: boolean; data?: DeviceInfo; error?: string }>>({})

/* 设备凭证:按地址区分(localStorage 持久化),连接与设备信息查询共用;
 * 无认证设备留空即可,不影响原有流程 */
interface DeviceCred { username: string; password: string }
const CREDS_KEY = 'onvif-ai-creds'
const deviceCreds = reactive<Record<string, DeviceCred>>({})

function loadAllCreds(): Record<string, DeviceCred> {
  try { return JSON.parse(localStorage.getItem(CREDS_KEY) || '{}') } catch { return {} }
}

function credOf(addr: string): DeviceCred {
  if (!deviceCreds[addr]) {
    const saved = loadAllCreds()[addr]
    deviceCreds[addr] = { username: saved?.username || '', password: saved?.password || '' }
  }
  return deviceCreds[addr]
}

function persistCred(addr: string) {
  const all = loadAllCreds()
  all[addr] = deviceCreds[addr]
  localStorage.setItem(CREDS_KEY, JSON.stringify(all))
}

async function fetchDeviceInfo(addr: string) {
  if (deviceInfoMap[addr]?.data || deviceInfoMap[addr]?.loading) return
  deviceInfoMap[addr] = { loading: true }
  try {
    const cred = deviceCreds[addr]
    const q = new URLSearchParams({ address: addr })
    if (cred?.username) {
      q.set('username', cred.username)
      q.set('password', cred.password)
    }
    const resp = await fetch(`/api/camera/device-info?${q}`)
    if (resp.ok) {
      deviceInfoMap[addr] = { loading: false, data: await resp.json() }
    } else {
      const err = await resp.json().catch(() => ({ error: 'unknown' }))
      deviceInfoMap[addr] = { loading: false, error: err.error || '获取失败' }
    }
  } catch {
    deviceInfoMap[addr] = { loading: false, error: '网络错误' }
  }
}

function scopeValue(scopes: string[], key: string): string {
  const prefix = `onvif://www.onvif.org/${key}/`
  for (const s of scopes || []) {
    if (s.startsWith(prefix)) return s.slice(prefix.length)
  }
  return ''
}

function friendlyAddress(addr: string): string {
  try {
    const u = new URL(addr.startsWith('http') ? addr : 'http://' + addr)
    return u.hostname
  } catch {
    return addr
  }
}

function toggleDetail(addr: string) {
  const isExpanding = expandedAddress.value !== addr
  expandedAddress.value = isExpanding ? addr : null
  if (isExpanding) fetchDeviceInfo(addr)
}

function getDeviceName(d: DiscoveredDevice): string {
  return scopeValue(d.scopes, 'name') || scopeValue(d.scopes, 'hardware') || friendlyAddress(d.address)
}

function getDeviceType(d: DiscoveredDevice): string {
  for (const t of d.types || []) {
    // e.g. "dn:NetworkVideoTransmitter" → "Network Video Transmitter"
    const parts = t.split(':')
    const name = parts[parts.length - 1]
    if (name) return name.replace(/([A-Z])/g, ' $1').trim()
  }
  return ''
}

function scopeTags(scopes: string[]): string[] {
  return (scopes || []).map(s => (s || '').split('/').pop() || '').filter(Boolean)
}
const llmBaseURL = ref('')
const llmApiKey = ref('')
const llmModel = ref('')
const llmSaving = ref(false)
const snapshotFps = ref(1)

const { subscribe } = useWebSocket('/ws')

const streamStatus = computed(() => {
  if (deviceState.value.snapshot_mode) return '快照模式'
  if (deviceState.value.streaming) return '实时流'
  if (deviceState.value.connected) return '已连接'
  return '未连接'
})

const statusClass = computed(() => {
  if (deviceState.value.streaming) return 'online'
  if (deviceState.value.snapshot_mode) return 'snapshot'
  if (deviceState.value.connected) return 'connected'
  return 'offline'
})

subscribe(['device_state'], (msg) => {
  if (msg.type === 'device_state' && msg.payload) {
    const state = msg.payload as unknown as DeviceState
    if (state && typeof state.connected === 'boolean') {
      deviceState.value = state
    }
  }
})

async function discoverDevices() {
  discovering.value = true
  discoverError.value = ''
  try {
    const resp = await fetch('/api/camera/discover')
    const data = await resp.json()
    devices.value = data.devices || []
    if (devices.value.length === 0) {
      discoverError.value = '未发现 ONVIF 设备'
    }
  } catch (e) {
    discoverError.value = '搜索失败: ' + (e instanceof Error ? e.message : '网络错误')
  } finally {
    discovering.value = false
  }
}

async function connectDevice(addr: string) {
  connecting.value = true
  const cred = credOf(addr)
  persistCred(addr)
  try {
    await fetch('/api/camera/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ address: addr, username: cred.username, password: cred.password }),
    })
  } catch {
    /* error handled by WebSocket status update */
  } finally {
    connecting.value = false
  }
}

onMounted(async () => {
  try {
    const resp = await fetch('/api/camera/info')
    if (resp.ok) deviceState.value = await resp.json()
  } catch { /* WebSocket will update */ }

  discoverDevices()
  fetchLLMConfig()
  loadSettings()
})

function loadSettings() {
  try {
    const saved = localStorage.getItem('onvif-ai-settings')
    if (saved) {
      const s = JSON.parse(saved)
      snapshotFps.value = s.snapshotFps || 1
    }
  } catch {}
  fetchSettings()
}

async function fetchSettings() {
  try {
    const resp = await fetch('/api/settings')
    if (resp.ok) {
      const s = await resp.json()
      snapshotFps.value = s.snapshot_fps || 1
    }
  } catch {}
}

async function saveSettings() {
  try {
    await fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ snapshot_fps: snapshotFps.value }),
    })
    localStorage.setItem('onvif-ai-settings', JSON.stringify({
      snapshotFps: snapshotFps.value,
    }))
  } catch (e) {
    console.error('Save settings failed:', e)
  }
}

async function fetchLLMConfig() {
  try {
    const resp = await fetch('/api/llm/config')
    if (resp.ok) {
      const cfg = await resp.json()
      llmBaseURL.value = cfg.base_url || ''
      llmApiKey.value = cfg.api_key || ''
      llmModel.value = cfg.model || ''
    }
  } catch { /* ignore */ }
}

async function saveLLMConfig() {
  llmSaving.value = true
  try {
    await fetch('/api/llm/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        base_url: llmBaseURL.value,
        api_key: llmApiKey.value,
        model: llmModel.value,
      }),
    })
  } catch (e) {
    console.error('Save LLM config failed:', e)
  } finally {
    llmSaving.value = false
  }
}

/* 转发设置：把摄像头 RTSP 流转发到流媒体服务器（如 mediamtx）。
 * 密码回显为脱敏值，保存时原样回传即视为「未修改」（后端保留旧值） */
interface ForwardConfig {
  enabled: boolean
  target_url: string
  username: string
  password: string
  use_tls: boolean
  auto_discover: boolean
  discover_interval: number
}

interface ForwardItem {
  source: string
  path: string
  state: string
  last_error?: string
}

const showForwardSettings = ref(false)
const fwdEnabled = ref(false)
const fwdTargetURL = ref('')
const fwdUsername = ref('')
const fwdPassword = ref('')
const fwdUseTLS = ref(false)
const fwdAutoDiscover = ref(false)
const fwdSaving = ref(false)
const fwdStatuses = ref<ForwardItem[]>([])
let fwdStatusTimer: ReturnType<typeof setInterval> | undefined

async function fetchForwardConfig() {
  try {
    const resp = await fetch('/api/forward/config')
    if (resp.ok) {
      const cfg: ForwardConfig = await resp.json()
      fwdEnabled.value = cfg.enabled
      fwdTargetURL.value = cfg.target_url || ''
      fwdUsername.value = cfg.username || ''
      fwdPassword.value = cfg.password || ''
      fwdUseTLS.value = cfg.use_tls
      fwdAutoDiscover.value = cfg.auto_discover
    }
  } catch { /* ignore */ }
}

async function saveForwardConfig() {
  fwdSaving.value = true
  try {
    const resp = await fetch('/api/forward/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        enabled: fwdEnabled.value,
        target_url: fwdTargetURL.value,
        username: fwdUsername.value,
        password: fwdPassword.value,
        use_tls: fwdUseTLS.value,
        auto_discover: fwdAutoDiscover.value,
      }),
    })
    if (!resp.ok) {
      const err = await resp.json().catch(() => ({ error: 'unknown' }))
      alert(err.error || '保存失败')
    } else {
      fetchForwardStatus()
    }
  } catch {
    alert('网络错误，保存失败')
  } finally {
    fwdSaving.value = false
  }
}

async function fetchForwardStatus() {
  try {
    const resp = await fetch('/api/forward/status')
    if (resp.ok) {
      const data = await resp.json()
      fwdStatuses.value = data.forwards || []
    }
  } catch { /* ignore */ }
}

/* 面板打开时轮询各路转发状态，关闭即停（低频展示，无需实时推送） */
function toggleForwardSettings() {
  showForwardSettings.value = !showForwardSettings.value
  if (showForwardSettings.value) {
    fetchForwardConfig()
    fetchForwardStatus()
    fwdStatusTimer = setInterval(fetchForwardStatus, 3000)
  } else if (fwdStatusTimer !== undefined) {
    clearInterval(fwdStatusTimer)
    fwdStatusTimer = undefined
  }
}

function forwardStateText(s: ForwardItem): string {
  switch (s.state) {
    case 'running': return '转发中'
    case 'retrying': return '重试中'
    default: return '已停止'
  }
}

onUnmounted(() => {
  if (fwdStatusTimer !== undefined) clearInterval(fwdStatusTimer)
})
</script>

<template>
  <div class="device-info">
    <div class="device-info__header">
      <span class="device-info__title">设备管理</span>
    </div>
    <div class="device-info__status" :class="`device-info__status--${statusClass}`">
      <span class="device-info__status-dot"></span>
      <span class="device-info__status-text">{{ streamStatus }}</span>
      <span v-if="deviceState.address !== '未连接'" class="device-info__status-addr">{{ deviceState.address }}</span>
    </div>
    <button class="device-info__discover-btn" @click="discoverDevices" :disabled="discovering">
      <span v-if="discovering" class="device-info__spinner"></span>
      <span>{{ discovering ? '搜索中...' : '🔍 搜索设备' }}</span>
    </button>
    <div v-if="discoverError" class="device-info__error">{{ discoverError }}</div>
    <div v-if="devices.length > 0" class="device-info__list">
      <div v-for="d in devices" :key="d.address" class="device-info__device"
        :class="{
          'device-info__device--active': deviceState.address === d.address,
          'device-info__device--expanded': expandedAddress === d.address
        }"
        @click="toggleDetail(d.address)">
        <div class="device-info__device-row">
          <div class="device-info__device-main">
            <span class="device-info__device-name">{{ getDeviceName(d) }}</span>
            <span v-if="getDeviceType(d)" class="device-info__device-type-tag">{{ getDeviceType(d) }}</span>
          </div>
          <div class="device-info__device-meta">
            <span class="device-info__device-ip">{{ friendlyAddress(d.address) }}</span>
            <button class="device-info__connect-btn" :disabled="connecting" @click.stop="connectDevice(d.address)">
              {{ connecting && deviceState.address === d.address ? '连接中...' : '连接' }}
            </button>
          </div>
        </div>
        <div v-if="expandedAddress === d.address" class="device-info__device-detail" @click.stop>
          <div v-if="deviceInfoMap[d.address]?.loading" class="device-info__detail-section">
            <span class="device-info__detail-label">信息</span>
            <span class="device-info__detail-value" style="color: #7a8490;">加载中...</span>
          </div>
          <div v-else-if="deviceInfoMap[d.address]?.data" class="device-info__detail-section">
            <span class="device-info__detail-label">信息</span>
            <div class="device-info__detail-grid">
              <div v-if="deviceInfoMap[d.address]!.data!.Manufacturer" class="device-info__detail-grid-item">
                <span class="device-info__detail-grid-label">厂家</span>
                <span class="device-info__detail-value">{{ deviceInfoMap[d.address]!.data!.Manufacturer }}</span>
              </div>
              <div v-if="deviceInfoMap[d.address]!.data!.Model" class="device-info__detail-grid-item">
                <span class="device-info__detail-grid-label">型号</span>
                <span class="device-info__detail-value">{{ deviceInfoMap[d.address]!.data!.Model }}</span>
              </div>
              <div v-if="deviceInfoMap[d.address]!.data!.FirmwareVersion" class="device-info__detail-grid-item">
                <span class="device-info__detail-grid-label">固件</span>
                <span class="device-info__detail-value device-info__detail-value--mono">{{ deviceInfoMap[d.address]!.data!.FirmwareVersion }}</span>
              </div>
              <div v-if="deviceInfoMap[d.address]!.data!.SerialNumber" class="device-info__detail-grid-item">
                <span class="device-info__detail-grid-label">序列号</span>
                <span class="device-info__detail-value device-info__detail-value--mono">{{ deviceInfoMap[d.address]!.data!.SerialNumber }}</span>
              </div>
              <div v-if="deviceInfoMap[d.address]!.data!.HardwareID" class="device-info__detail-grid-item">
                <span class="device-info__detail-grid-label">硬件 ID</span>
                <span class="device-info__detail-value device-info__detail-value--mono">{{ deviceInfoMap[d.address]!.data!.HardwareID }}</span>
              </div>
            </div>
          </div>
          <div v-else-if="deviceInfoMap[d.address]?.error" class="device-info__detail-section">
            <span class="device-info__detail-label">信息</span>
            <span class="device-info__detail-value" style="color: #ff3d57;">{{ deviceInfoMap[d.address]!.error }}</span>
          </div>
          <div class="device-info__detail-section">
            <span class="device-info__detail-label">认证</span>
            <div class="device-info__cred-fields">
              <input v-model="credOf(d.address).username" placeholder="用户名(无认证留空)"
                @change="persistCred(d.address)" @click.stop />
              <input v-model="credOf(d.address).password" type="password" placeholder="密码"
                @change="persistCred(d.address)" @click.stop />
            </div>
          </div>
          <div class="device-info__detail-section">
            <span class="device-info__detail-label">地址</span>
            <span class="device-info__detail-value device-info__detail-value--mono">{{ d.address }}</span>
          </div>
          <div v-if="scopeValue(d.scopes, 'hardware')" class="device-info__detail-section">
            <span class="device-info__detail-label">硬件</span>
            <span class="device-info__detail-value">{{ scopeValue(d.scopes, 'hardware') }}</span>
          </div>
          <div v-if="scopeValue(d.scopes, 'location')" class="device-info__detail-section">
            <span class="device-info__detail-label">位置</span>
            <span class="device-info__detail-value">{{ scopeValue(d.scopes, 'location') }}</span>
          </div>
          <div v-if="scopeTags(d.scopes).length" class="device-info__detail-section">
            <span class="device-info__detail-label">Scopes</span>
            <div class="device-info__detail-tags">
              <span v-for="s in scopeTags(d.scopes)" :key="s" class="device-info__scope-tag">{{ s }}</span>
            </div>
          </div>
          <div v-if="d.xaddrs?.length" class="device-info__detail-section">
            <span class="device-info__detail-label">XAddrs</span>
            <div class="device-info__detail-list">
              <span v-for="x in d.xaddrs" :key="x" class="device-info__detail-value device-info__detail-value--mono">{{ x }}</span>
            </div>
          </div>
          <div v-if="d.types?.length" class="device-info__detail-section">
            <span class="device-info__detail-label">类型</span>
            <div class="device-info__detail-tags">
              <span v-for="t in d.types" :key="t" class="device-info__scope-tag device-info__scope-tag--type">{{ t }}</span>
            </div>
          </div>
          <div class="device-info__detail-section">
            <span class="device-info__detail-label">元数据版本</span>
            <span class="device-info__detail-value">{{ d.metadata_version }}</span>
          </div>
        </div>
      </div>
    </div>
    <div v-else-if="!discovering && !discoverError" class="device-info__hint">点击「搜索设备」发现局域网 ONVIF 摄像头</div>

    <!-- 快照降级时的观看设置（通用功能，不藏在 AI 配置里） -->
    <div v-if="deviceState.snapshot_mode" class="device-info__llm-field">
      <label>快照帧率 ({{ snapshotFps }} FPS)</label>
      <input type="range" v-model.number="snapshotFps" min="1" max="10" @change="saveSettings" />
    </div>

    <button class="device-info__settings-toggle" @click="showLLMSettings = !showLLMSettings">
      ⚙️ AI 模型配置 {{ showLLMSettings ? '▲' : '▼' }}
    </button>

    <div v-if="showLLMSettings" class="device-info__llm-panel">
      <div class="device-info__llm-field">
        <label>API 地址</label>
        <input v-model="llmBaseURL" placeholder="https://api.openai.com" />
      </div>
      <div class="device-info__llm-field">
        <label>API Key</label>
        <input v-model="llmApiKey" type="password" placeholder="sk-..." />
      </div>
      <div class="device-info__llm-field">
        <label>模型</label>
        <input v-model="llmModel" placeholder="gpt-3.5-turbo" />
      </div>
      <button class="device-info__llm-save" @click="saveLLMConfig" :disabled="llmSaving">
        {{ llmSaving ? '保存中...' : '保存' }}
      </button>
    </div>

    <button class="device-info__settings-toggle" @click="toggleForwardSettings">
      📡 转发设置 {{ showForwardSettings ? '▲' : '▼' }}
    </button>

    <div v-if="showForwardSettings" class="device-info__llm-panel">
      <label class="device-info__fwd-check">
        <input type="checkbox" v-model="fwdEnabled" />
        <span>启用转发（把摄像头流推送到流媒体服务器）</span>
      </label>
      <div class="device-info__llm-field">
        <label>流媒体服务器地址</label>
        <input v-model="fwdTargetURL" placeholder="rtsp://192.168.1.10:8554" />
      </div>
      <div class="device-info__llm-field">
        <label>用户名</label>
        <input v-model="fwdUsername" placeholder="无认证留空" />
      </div>
      <div class="device-info__llm-field">
        <label>密码</label>
        <input v-model="fwdPassword" type="password" placeholder="无认证留空" />
      </div>
      <label class="device-info__fwd-check">
        <input type="checkbox" v-model="fwdUseTLS" />
        <span>加密连接（rtsps）</span>
      </label>
      <label class="device-info__fwd-check">
        <input type="checkbox" v-model="fwdAutoDiscover" />
        <span>自动发现设备并转发</span>
      </label>
      <div v-if="fwdEnabled" class="device-info__fwd-hint">
        每路画面将发布为流媒体服务器上的 onvif-ai/设备地址/画面名 路径；服务器离线时会自动重试，恢复后继续推送。
      </div>
      <div v-if="fwdStatuses.length > 0" class="device-info__fwd-status">
        <div v-for="s in fwdStatuses" :key="s.path" class="device-info__fwd-status-row">
          <span class="device-info__fwd-status-path">{{ s.path }}</span>
          <span class="device-info__fwd-status-state" :class="`device-info__fwd-status-state--${s.state}`">
            {{ forwardStateText(s) }}
          </span>
          <span v-if="s.last_error" class="device-info__fwd-status-error" :title="s.last_error">{{ s.last_error }}</span>
        </div>
      </div>
      <button class="device-info__llm-save" @click="saveForwardConfig" :disabled="fwdSaving">
        {{ fwdSaving ? '保存中...' : '保存' }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.device-info { background: #131820; border: 1px solid #1e2530; border-radius: 8px; padding: 12px 16px; display: flex; flex-direction: column; gap: 10px; }
.device-info__header { display: flex; align-items: center; justify-content: space-between; }
.device-info__title { font-size: 0.75rem; font-weight: 600; letter-spacing: 0.05em; text-transform: uppercase; color: #7a8490; }
.device-info__status { display: flex; align-items: center; gap: 8px; padding: 6px 10px; border-radius: 6px; font-size: 0.8rem; }
.device-info__status--online { background: rgba(0,229,160,.1); color: #00e5a0; }
.device-info__status--snapshot { background: rgba(255,184,0,.1); color: #ffb800; }
.device-info__status--connected { background: rgba(0,145,255,.1); color: #0091ff; }
.device-info__status--offline { background: rgba(122,132,144,.1); color: #7a8490; }
.device-info__status-dot { width: 8px; height: 8px; border-radius: 50%; background: currentColor; flex-shrink: 0; }
.device-info__status-text { font-weight: 500; }
.device-info__status-addr { margin-left: auto; font-size: 0.7rem; opacity: 0.7; max-width: 180px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.device-info__discover-btn { width: 100%; padding: 8px 12px; background: rgba(0,145,255,.12); border: 1px solid rgba(0,145,255,.25); border-radius: 6px; color: #0091ff; font-size: 0.8rem; cursor: pointer; display: flex; align-items: center; justify-content: center; gap: 6px; transition: all 0.15s; }
.device-info__discover-btn:hover:not(:disabled) { background: rgba(0,145,255,.2); }
.device-info__discover-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.device-info__spinner { width: 14px; height: 14px; border: 2px solid transparent; border-top-color: currentColor; border-radius: 50%; animation: spin .6s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
.device-info__error { color: #ff3d57; font-size: 0.75rem; padding: 6px 8px; background: rgba(255,61,87,.1); border-radius: 4px; }
.device-info__hint { color: #5a6470; font-size: 0.75rem; text-align: center; padding: 8px; }
.device-info__list { display: flex; flex-direction: column; gap: 6px; max-height: 360px; overflow-y: auto; }
.device-info__device { padding: 8px 10px; background: rgba(255,255,255,.03); border: 1px solid rgba(255,255,255,.06); border-radius: 6px; cursor: pointer; transition: all .15s; }
.device-info__device:hover { background: rgba(255,255,255,.06); }
.device-info__device--active { border-color: rgba(0,229,160,.3); background: rgba(0,229,160,.05); }
.device-info__device--expanded { border-color: rgba(0,145,255,.25); background: rgba(0,145,255,.04); }

.device-info__device-row { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.device-info__device-main { display: flex; align-items: center; gap: 6px; min-width: 0; flex: 1; }
.device-info__device-name { font-size: 0.78rem; color: #c8d0d8; font-weight: 500; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.device-info__device-type-tag { font-size: 0.55rem; padding: 1px 5px; background: rgba(122,132,144,.15); color: #7a8490; border-radius: 3px; white-space: nowrap; flex-shrink: 0; }
.device-info__device-meta { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.device-info__device-ip { font-size: 0.65rem; color: #5a6470; font-family: monospace; }

.device-info__device-detail { margin-top: 8px; padding-top: 8px; border-top: 1px solid rgba(255,255,255,.06); display: flex; flex-direction: column; gap: 6px; }
.device-info__detail-section { display: flex; align-items: flex-start; gap: 8px; }
.device-info__detail-label { font-size: 0.62rem; color: #5a6470; text-transform: uppercase; letter-spacing: 0.04em; min-width: 55px; flex-shrink: 0; padding-top: 1px; }
.device-info__detail-value { font-size: 0.68rem; color: #9098a4; word-break: break-all; }
.device-info__detail-value--mono { font-family: monospace; font-size: 0.62rem; }
.device-info__detail-tags { display: flex; flex-wrap: wrap; gap: 3px; }
.device-info__detail-list { display: flex; flex-direction: column; gap: 2px; }

.device-info__detail-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 4px 12px; width: 100%; }
.device-info__detail-grid-item { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
.device-info__detail-grid-label { font-size: 0.58rem; color: #5a6470; text-transform: uppercase; letter-spacing: 0.03em; }

.device-info__scope-tag { font-size: 0.58rem; padding: 1px 6px; background: rgba(0,145,255,.12); color: #0091ff; border-radius: 3px; white-space: nowrap; }
.device-info__scope-tag--type { background: rgba(122,132,144,.12); color: #7a8490; }

.device-info__cred-fields { display: flex; gap: 6px; flex: 1; min-width: 0; }
.device-info__cred-fields input {
  flex: 1; min-width: 0; padding: 4px 8px;
  background: #131820; border: 1px solid #1e2530; border-radius: 4px;
  color: #c8d0d8; font-size: 0.7rem;
}
.device-info__cred-fields input:focus { outline: none; border-color: #0091ff; }

.device-info__connect-btn { font-size: 0.7rem; padding: 3px 10px; background: rgba(0,229,160,.12); border: 1px solid rgba(0,229,160,.25); border-radius: 4px; color: #00e5a0; cursor: pointer; white-space: nowrap; }
.device-info__connect-btn:hover:not(:disabled) { background: rgba(0,229,160,.2); }
.device-info__connect-btn:disabled { opacity: 0.4; cursor: not-allowed; }

.device-info__settings-toggle {
  width: 100%; padding: 6px 10px;
  background: rgba(255,255,255,.03); border: 1px solid rgba(255,255,255,.06);
  border-radius: 6px; color: #7a8490; font-size: 0.72rem; cursor: pointer;
}
.device-info__settings-toggle:hover { background: rgba(255,255,255,.06); }

.device-info__llm-panel {
  display: flex; flex-direction: column; gap: 6px;
  padding: 8px; background: #0a0e14; border-radius: 6px;
  border: 1px solid #1e2530;
}

.device-info__llm-field { display: flex; flex-direction: column; gap: 2px; }
.device-info__llm-field label {
  font-size: 0.6rem; color: #5a6470; letter-spacing: 0.03em;
}
.device-info__llm-field input {
  padding: 4px 8px; background: #131820; border: 1px solid #1e2530;
  border-radius: 4px; color: #c8d0d8; font-size: 0.7rem;
  font-family: monospace;
}
.device-info__llm-field input:focus { outline: none; border-color: #0091ff; }

.device-info__llm-save {
  padding: 5px 12px; background: rgba(0,145,255,.15);
  border: 1px solid rgba(0,145,255,.25); border-radius: 4px;
  color: #0091ff; font-size: 0.72rem; cursor: pointer;
  align-self: flex-end;
}
.device-info__llm-save:disabled { opacity: 0.4; cursor: not-allowed; }

/* 转发设置面板：复用 AI 面板的字段样式，追加开关行与状态列表 */
.device-info__fwd-check {
  display: flex; align-items: center; gap: 6px;
  font-size: 0.72rem; color: #c8d0d8; cursor: pointer;
}
.device-info__fwd-check input { accent-color: #0091ff; }

.device-info__fwd-hint {
  font-size: 0.62rem; color: #5a6470; line-height: 1.5;
}

.device-info__fwd-status {
  display: flex; flex-direction: column; gap: 4px;
  padding: 6px; background: rgba(255,255,255,.02);
  border: 1px solid rgba(255,255,255,.05); border-radius: 4px;
}
.device-info__fwd-status-row {
  display: flex; align-items: center; gap: 8px; min-width: 0;
}
.device-info__fwd-status-path {
  font-family: monospace; font-size: 0.62rem; color: #9098a4;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.device-info__fwd-status-state {
  font-size: 0.6rem; padding: 1px 6px; border-radius: 3px; flex-shrink: 0;
}
.device-info__fwd-status-state--running { background: rgba(0,229,160,.12); color: #00e5a0; }
.device-info__fwd-status-state--retrying { background: rgba(255,184,0,.12); color: #ffb800; }
.device-info__fwd-status-state--error { background: rgba(255,61,87,.12); color: #ff3d57; }
.device-info__fwd-status-error {
  font-size: 0.6rem; color: #ff3d57; opacity: .8;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1;
}
</style>
