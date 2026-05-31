<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useWebSocket } from '../composables/useWebSocket'

interface DiscoveredDevice {
  address: string
  types: string[]
  scopes: string[]
  xaddrs: string[]
  metadata_version: number
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
const llmBaseURL = ref('')
const llmApiKey = ref('')
const llmModel = ref('')
const llmSaving = ref(false)

const { messages, popNewMessages } = useWebSocket('/ws')

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

watch(messages, () => {
  const newMsgs = popNewMessages()
  for (const msg of newMsgs) {
    if (msg.type === 'device_state' && msg.payload) {
      const state = msg.payload as unknown as DeviceState
      if (state && typeof state.connected === 'boolean') {
        deviceState.value = state
      }
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
  try {
    await fetch('/api/camera/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ address: addr }),
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
})

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
        :class="{ 'device-info__device--active': deviceState.address === d.address }">
        <div class="device-info__device-addr">{{ d.address }}</div>
        <div class="device-info__device-scopes">
          <span v-for="s in (d.scopes || []).slice(0, 3)" :key="s" class="device-info__scope-tag">{{ (s || '').split('/').pop() }}</span>
        </div>
        <button class="device-info__connect-btn" :disabled="connecting" @click.stop="connectDevice(d.address)">
          {{ connecting && deviceState.address === d.address ? '连接中...' : '连接' }}
        </button>
      </div>
    </div>
    <div v-else-if="!discovering && !discoverError" class="device-info__hint">点击「搜索设备」发现局域网 ONVIF 摄像头</div>

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
.device-info__list { display: flex; flex-direction: column; gap: 6px; max-height: 260px; overflow-y: auto; }
.device-info__device { padding: 8px 10px; background: rgba(255,255,255,.03); border: 1px solid rgba(255,255,255,.06); border-radius: 6px; cursor: pointer; transition: all .15s; }
.device-info__device:hover { background: rgba(255,255,255,.06); }
.device-info__device--active { border-color: rgba(0,229,160,.3); background: rgba(0,229,160,.05); }
.device-info__device-addr { font-size: 0.75rem; color: #c8d0d8; margin-bottom: 4px; word-break: break-all; }
.device-info__device-scopes { display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 4px; }
.device-info__scope-tag { font-size: 0.6rem; padding: 1px 6px; background: rgba(0,145,255,.12); color: #0091ff; border-radius: 3px; }
.device-info__connect-btn { font-size: 0.7rem; padding: 2px 10px; background: rgba(0,229,160,.12); border: 1px solid rgba(0,229,160,.25); border-radius: 4px; color: #00e5a0; cursor: pointer; }
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
</style>
