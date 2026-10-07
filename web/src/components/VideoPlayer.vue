<script setup lang="ts">
import { ref, computed, watch, onUnmounted, provide } from 'vue'
import { useWebSocket, type WsMessage } from '../composables/useWebSocket'
import CameraTile from './CameraTile.vue'

/**
 * 视频区域：支持单设备多摄像头（多 media profile）。
 * 左上角提供两种观看模式：
 *  - 多画面：同时预览全部摄像头
 *  - 单画面：单路全幅显示，可在画面间切换
 */

interface CamInfo {
  token: string
  name: string
  ptz: boolean
  streaming: boolean
  snapshot: boolean
  mjpeg: boolean
}

const connectionStatus = ref<'disconnected' | 'connecting' | 'connected'>('connecting')
const cameras = ref<CamInfo[]>([])
const mode = ref<'grid' | 'single'>(
  (localStorage.getItem('onvif-ai-view-mode') as 'grid' | 'single') || 'single',
)
const activeCam = ref('')
const videoError = ref('')

const { isConnected, isConnecting, send, subscribe } = useWebSocket('/ws')

let lastErrorText = ''
function handleMessage(msg: WsMessage) {
  if (msg.type === 'video_nal' && msg.data) {
    lastFrameTime = Date.now()
    const data = msg.data
    routeToTile(msg.cam, (tile) => tile.feedNal(msg.ts, data))
    return
  }
  if (msg.type === 'video_jpeg' && msg.data) {
    lastFrameTime = Date.now()
    const data = msg.data
    routeToTile(msg.cam, (tile) => tile.feedJpeg(data))
    return
  }
  if (msg.type === 'device_state' && msg.payload) {
    applyDeviceState(msg.payload)
    return
  }
  if (msg.type === 'clock_sync' && msg.payload) {
    handleClockSyncReply(msg.payload)
    return
  }
  if (msg.type === 'error' && msg.text) {
    if (msg.text !== lastErrorText) {
      lastErrorText = msg.text
      console.error('[VideoPlayer] Error:', msg.text)
    }
    videoError.value = msg.text
  }
}
subscribe(['video_nal', 'video_jpeg', 'device_state', 'clock_sync', 'error'], handleMessage)

// tile 自注册表：v-for 的函数 ref 在模式切换时挂载/卸载回调顺序不确定，
// 由 CameraTile 在自身生命周期内注册/注销，避免 Map 被旧实例误删。
interface TileAPI {
  feedNal: (ts: number | undefined, data: string) => void
  feedJpeg: (data: string) => void
}
const tiles = new Map<string, TileAPI>()
provide('cameraTileRegistry', {
  register: (token: string, api: TileAPI) => tiles.set(token, api),
  unregister: (token: string, api: TileAPI) => {
    if (tiles.get(token) === api) tiles.delete(token)
  },
})

const hasAnyStream = computed(() => cameras.value.some((c) => c.streaming || c.snapshot))
const multiCam = computed(() => cameras.value.length > 1)
const activeCamera = computed(
  () => cameras.value.find((c) => c.token === activeCam.value) ?? cameras.value[0],
)

function setMode(m: 'grid' | 'single') {
  mode.value = m
  localStorage.setItem('onvif-ai-view-mode', m)
}

function switchCamera(token: string) {
  activeCam.value = token
}

function camDisplayName(cam: CamInfo, index: number) {
  return cam.name || `摄像头 ${index + 1}`
}

function ptzMove(direction: string) {
  const cam = activeCamera.value
  if (!cam) return
  send({ type: 'ptz_move', payload: { camera: cam.token, direction } })
}

// ---- 时钟同步：为各路延迟测量提供统一的时钟偏移 ----
let bestSyncRtt = Number.POSITIVE_INFINITY
const clockOffsetRef = ref<number | null>(null)

function sendClockSyncProbe() {
  send({ type: 'clock_sync', payload: { t0: Date.now() } })
}

function handleClockSyncReply(payload: unknown) {
  const p = payload as { t0?: number; t1?: number }
  const t0 = Number(p?.t0)
  const t1 = Number(p?.t1)
  if (!Number.isFinite(t0) || !Number.isFinite(t1)) return
  const rtt = Date.now() - t0
  if (rtt < bestSyncRtt) {
    bestSyncRtt = rtt
    clockOffsetRef.value = t1 + rtt / 2 - Date.now()
  }
}

let clockSyncTimer: ReturnType<typeof setInterval> | null = null
// 重连代际：tiles 据此重建解码管线（旧 MSE 时间轴已作废）
const resetKey = ref(0)

watch(isConnected, (connected) => {
  if (connected) {
    connectionStatus.value = 'connected'
    bestSyncRtt = Number.POSITIVE_INFINITY
    clockOffsetRef.value = null
    resetKey.value++
    for (let i = 0; i < 3; i++) {
      setTimeout(sendClockSyncProbe, i * 300)
    }
    if (clockSyncTimer) clearInterval(clockSyncTimer)
    clockSyncTimer = setInterval(sendClockSyncProbe, 15000)
  } else {
    connectionStatus.value = 'disconnected'
    if (clockSyncTimer) {
      clearInterval(clockSyncTimer)
      clockSyncTimer = null
    }
  }
})

watch(isConnecting, (connecting) => {
  if (connecting) {
    connectionStatus.value = 'connecting'
  }
})

let lastFrameTime = 0



function routeToTile(cam: string | undefined, feed: (tile: TileAPI) => void) {
  if (!cameras.value.length) return
  const token = cam || cameras.value[0].token
  const tile = tiles.get(token)
  if (tile) {
    feed(tile)
  }
}

function applyDeviceState(state: any) {
  const list: CamInfo[] = (state.cameras || []).map((c: any) => ({
    token: c.token,
    name: c.name || c.token,
    ptz: !!c.ptz_supported,
    streaming: !!c.streaming,
    snapshot: !!c.snapshot_mode,
    mjpeg: !!c.mjpeg,
  }))

  // 兼容未携带 cameras 列表的旧后端：退化为单路隐式摄像头
  if (!list.length && (state.streaming || state.snapshot_mode)) {
    list.push({
      token: state.address || 'default',
      name: '摄像头',
      ptz: !!state.ptz_supported,
      streaming: !!state.streaming,
      snapshot: !!state.snapshot_mode,
      mjpeg: false,
    })
  }

  const wasEmpty = cameras.value.length === 0
  cameras.value = list

  if (list.length && (!activeCam.value || !list.some((c) => c.token === activeCam.value))) {
    activeCam.value = list[0].token
  }
  // 首次进入多摄像头且用户从未显式选择过模式时，默认多画面预览
  if (list.length > 1 && wasEmpty && !localStorage.getItem('onvif-ai-view-mode')) {
    mode.value = 'grid'
  }
  if (state.streaming || state.snapshot_mode) {
    videoError.value = ''
  }
}

const interval = setInterval(() => {
  if (isConnected.value && hasAnyStream.value && lastFrameTime > 0 && Date.now() - lastFrameTime > 5000) {
    videoError.value = '视频流中断'
  }
}, 3000)

onUnmounted(() => {
  clearInterval(interval)
  if (clockSyncTimer) clearInterval(clockSyncTimer)
})
</script>

<template>
  <div class="video-player">
    <div class="video-player__header">
      <div class="video-player__title">
        <span class="video-player__indicator" :class="`video-player__indicator--${connectionStatus}`"></span>
        <span class="video-player__label">实时视频流</span>
      </div>
      <span v-if="multiCam" class="video-player__cam-count">{{ cameras.length }} 路画面</span>
      <span class="video-player__status-label" :class="`video-player__status-label--${connectionStatus}`">
        {{ connectionStatus === 'connected' ? '在线' : connectionStatus === 'connecting' ? '连接中...' : '断开' }}
      </span>
    </div>

    <div class="video-player__viewport">
      <!-- 左上角：观看模式切换（多画面 / 单画面） -->
      <div v-if="multiCam" class="video-player__mode-switch" role="tablist">
        <button
          class="video-player__mode-btn"
          :class="{ 'video-player__mode-btn--active': mode === 'grid' }"
          title="同时预览全部摄像头"
          @click="setMode('grid')"
        >多画面</button>
        <button
          class="video-player__mode-btn"
          :class="{ 'video-player__mode-btn--active': mode === 'single' }"
          title="单路全幅显示，可切换摄像头"
          @click="setMode('single')"
        >单画面</button>
      </div>

      <!-- 多画面：网格同时预览全部摄像头 -->
      <div v-if="mode === 'grid'" class="video-player__grid">
        <CameraTile
          v-for="cam in cameras"
          :key="cam.token"
          :cam="cam"
          :active="false"
          :show-label="true"
          :clock-offset="clockOffsetRef"
          :reset-key="resetKey"
        />
      </div>

      <!-- 单画面：当前摄像头全幅显示 -->
      <div v-else class="video-player__single">
        <CameraTile
          v-for="cam in cameras.filter((c) => c.token === activeCam)"
          :key="cam.token"
          :cam="cam"
          :active="true"
          :show-label="false"
          :clock-offset="clockOffsetRef"
          :reset-key="resetKey"
          @ptz="ptzMove"
        />
      </div>

      <!-- 单画面模式下的摄像头切换器 -->
      <div v-if="mode === 'single' && multiCam" class="video-player__cam-switch" role="tablist">
        <button
          v-for="(cam, i) in cameras"
          :key="cam.token"
          class="video-player__cam-btn"
          :class="{ 'video-player__cam-btn--active': cam.token === activeCam }"
          :title="`切换到 ${camDisplayName(cam, i)}`"
          @click="switchCamera(cam.token)"
        >{{ camDisplayName(cam, i) }}</button>
      </div>

      <div v-if="!hasAnyStream" class="video-player__placeholder">
        <span class="video-player__placeholder-icon">📷</span>
        <span class="video-player__placeholder-text">暂无摄像头画面</span>
      </div>
    </div>
    <div v-if="videoError" class="video-player__error">{{ videoError }}</div>
  </div>
</template>

<style scoped>
.video-player {
  --vp-padding: var(--space-4);
  background: var(--color-bg-panel);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
  overflow: hidden;
  display: flex;
  flex-direction: column;
  box-shadow: var(--shadow-panel);
}

.video-player__header {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  background: var(--color-bg-elevated);
  border-bottom: 1px solid var(--color-border-subtle);
}

.video-player__title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.video-player__label {
  font-family: var(--font-display);
  font-size: 0.6875rem;
  font-weight: 500;
  letter-spacing: 0.08em;
  color: var(--color-text-secondary);
  text-transform: uppercase;
}

.video-player__cam-count {
  margin-left: auto;
  font-family: var(--font-mono);
  font-size: 0.6875rem;
  color: var(--color-text-secondary);
  padding: 2px var(--space-2);
  border-radius: var(--radius-sm);
  background: var(--color-bg-hover);
}

.video-player__indicator {
  width: 8px;
  height: 8px;
  border-radius: var(--radius-full);
  background: var(--color-text-muted);
  transition: background var(--transition-base);
}

.video-player__indicator--connected {
  background: var(--color-accent-green);
  box-shadow: 0 0 6px var(--color-accent-green);
  animation: pulse 2s ease-in-out infinite;
}

.video-player__indicator--connecting {
  background: var(--color-accent-amber);
  box-shadow: 0 0 6px var(--color-accent-amber);
  animation: pulse 1s ease-in-out infinite;
}

.video-player__indicator--disconnected {
  background: var(--color-accent-red);
  box-shadow: 0 0 6px var(--color-accent-red);
}

.video-player__status-label {
  font-family: var(--font-mono);
  font-size: 0.6875rem;
  letter-spacing: 0.04em;
  padding: 2px var(--space-2);
  border-radius: var(--radius-sm);
  background: var(--color-bg-hover);
}

.video-player__status-label--connected {
  color: var(--color-accent-green);
}

.video-player__status-label--connecting {
  color: var(--color-accent-amber);
}

.video-player__status-label--disconnected {
  color: var(--color-accent-red);
}

.video-player__viewport {
  position: relative;
  aspect-ratio: 16 / 9;
  background: #020408;
  overflow: hidden;
}

/* 左上角模式切换 */
.video-player__mode-switch {
  position: absolute;
  top: 8px;
  left: 8px;
  z-index: 20;
  display: flex;
  gap: 2px;
  padding: 2px;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.6);
  backdrop-filter: blur(2px);
}

.video-player__mode-btn {
  border: none;
  background: transparent;
  color: rgba(255, 255, 255, 0.6);
  font-size: 0.72rem;
  letter-spacing: 0.04em;
  padding: 4px 12px;
  border-radius: 4px;
  cursor: pointer;
  transition: all 0.15s;
}

.video-player__mode-btn--active {
  background: rgba(0, 229, 160, 0.85);
  color: #04110c;
}

.video-player__mode-btn:not(.video-player__mode-btn--active):hover {
  color: rgba(255, 255, 255, 0.95);
}

/* 多画面网格 */
.video-player__grid {
  position: absolute;
  inset: 0;
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(33%, 1fr));
  gap: 2px;
}

.video-player__single {
  position: absolute;
  inset: 0;
}

/* tile 必须填满定位容器：否则其高度跟随视频流自身的宽高比
   （height:100% 在 auto 高度的父级上失效），画面比例与视口不一致时
   视频盒子会高于视口被裁切，object-fit: contain 无法生效 */
.video-player__single :deep(.cam-tile) {
  height: 100%;
}

/* 单画面模式下的摄像头切换器 */
.video-player__cam-switch {
  position: absolute;
  bottom: 8px;
  left: 50%;
  transform: translateX(-50%);
  z-index: 20;
  display: flex;
  gap: 2px;
  padding: 2px;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.6);
  backdrop-filter: blur(2px);
  max-width: 90%;
  overflow-x: auto;
}

.video-player__cam-btn {
  border: none;
  background: transparent;
  color: rgba(255, 255, 255, 0.6);
  font-size: 0.72rem;
  padding: 4px 12px;
  border-radius: 4px;
  cursor: pointer;
  white-space: nowrap;
  transition: all 0.15s;
}

.video-player__cam-btn--active {
  background: rgba(0, 229, 160, 0.85);
  color: #04110c;
}

.video-player__cam-btn:not(.video-player__cam-btn--active):hover {
  color: rgba(255, 255, 255, 0.95);
}

.video-player__placeholder {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  background: #020408;
  z-index: 3;
}

.video-player__placeholder-icon {
  font-size: 1.5rem;
}

.video-player__placeholder-text {
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  color: var(--color-text-dim);
  letter-spacing: 0.04em;
}

.video-player__error {
  padding: 6px 10px;
  background: rgba(255, 61, 87, 0.1);
  color: #ff3d57;
  font-size: 0.72rem;
  text-align: center;
}

/* ---- 触屏 / 窄屏适配 ---- */
@media (pointer: coarse) {
  .video-player__mode-btn,
  .video-player__cam-btn {
    padding: 8px 16px; /* 触摸目标高度 ≥40px */
    font-size: 0.8rem;
  }
}

@media (max-width: 960px) {
  .video-player__viewport {
    /* 竖屏手机上 16:9 过扁，放宽到 3:2 保证可视面积 */
    aspect-ratio: 3 / 2;
    /* 横屏窄高时 3:2 的画面自身就超过屏高，会把页面撑坏；
       限高后画面按 object-fit: contain 居中-letterbox */
    max-height: 70vh;
    max-height: 70dvh;
  }

  .video-player__mode-switch,
  .video-player__cam-switch {
    background: rgba(0, 0, 0, 0.7);
  }
}
</style>
