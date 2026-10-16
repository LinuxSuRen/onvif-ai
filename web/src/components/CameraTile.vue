<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, inject } from 'vue'

/**
 * CameraTile 承载一路摄像头画面（对应一个 ONVIF media profile）：
 * 标准 fMP4 → MSE（服务端封装，SourceBuffer 直接 append）、快照降级显示、
 * 端到端延迟测量与云台控制。
 *
 * 播放管线（替代 jmuxer 自制封装——其输出不合规范：硬件解码拒绝黑屏、
 * 软件解码绿条伪影）：后端把 H.264 AU 封装为标准 fMP4 init/frag 段，
 * 浏览器 appendBuffer 后走原生 MSE 解码路径（与 robot-platform-ui 的
 * RTSPtoWeb 模式一致）。
 */
const props = defineProps<{
  // dev 是该画面所属设备的地址（与 cam 的 token 组成订阅/路由键）
  dev: string
  cam: {
    token: string
    name: string
    ptz: boolean
    streaming: boolean
    snapshot: boolean
    mjpeg: boolean
    width: number
    height: number
  }
  active: boolean
  showLabel: boolean
  clockOffset: number | null
  // 变化时重建解码管线（WS 重连后由父组件递增）
  resetKey: number
}>()

const emit = defineEmits<{
  (e: 'ptz', direction: string): void
}>()

const videoRef = ref<HTMLVideoElement | null>(null)
const imgRef = ref<HTMLImageElement | null>(null)
const hasVideo = ref(false)
const inSnapshot = ref(false)
const latencyMs = ref(0)

// 分辨率角标文案（来自后端 SPS/JPEG 解析，未知为空串不渲染）
const resolution = computed(() =>
  props.cam.width > 0 && props.cam.height > 0 ? `${props.cam.width}x${props.cam.height}` : '',
)

// ---- MSE 管线状态（全部由主线程串行访问，无并发） ----
let mediaSource: MediaSource | null = null
let sourceBuffer: SourceBuffer | null = null
let objectUrl = ''
// codec 由首个 init 段携带（avc1.XXXX，来自 SPS），SourceBuffer 必须用
// 真实 codec 创建——猜错会导致首 init 触发无谓重建并错过整个 GOP。
let codecString = ''
// 最近一次接受的 init 段字节：后端每 GOP 重发同一 init，字节比较检测
// 编码参数变化（见 feedFMP4 的 init 分支）
let lastInitBytes: Uint8Array | null = null
const appendQueue: (ArrayBuffer | Uint8Array)[] = []
// 管线未就绪（等 init / 等 sourceopen）期间缓存最近一个关键帧段：
// 播放必须从关键帧起步，缓存它可保证 init 到位后立即可播。
let pendingKeySeg: Uint8Array | null = null
let awaitingInit = true

let transitEma: number | null = null
let presentedMediaTime = 0
let rVfcHandle = 0
let latencyTimer: ReturnType<typeof setInterval> | null = null
let evictTimer: ReturnType<typeof setInterval> | null = null
let lastCatchUpAt = 0

// 向父组件注册本 tile 的数据入口（生命周期内自管理，避免 ref 顺序竞态）。
// 注册键是 dev+'/'+token 复合键，与 WS 帧路由一致。
const registry = inject<{
  register: (routeKey: string, api: { feedFMP4: typeof feedFMP4; feedJpeg: typeof feedJpeg }) => void
  unregister: (routeKey: string, api: { feedFMP4: typeof feedFMP4; feedJpeg: typeof feedJpeg }) => void
} | null>('cameraTileRegistry', null)

const routeKey = () => `${props.dev}/${props.cam.token}`

function destroyPipeline() {
  appendQueue.length = 0
  pendingKeySeg = null
  if (sourceBuffer) {
    try {
      sourceBuffer.abort()
    } catch {
      /* updating/已移除时可能抛错 */
    }
    sourceBuffer.onupdateend = null
    sourceBuffer.onerror = null
    sourceBuffer = null
  }
  if (mediaSource) {
    try {
      if (mediaSource.readyState === 'open') mediaSource.endOfStream()
    } catch {
      /* ignore */
    }
    mediaSource = null
  }
  if (objectUrl) {
    URL.revokeObjectURL(objectUrl)
    objectUrl = ''
  }
}

function createPipeline() {
  if (!videoRef.value || mediaSource) return
  awaitingInit = true
  const ms = new MediaSource()
  mediaSource = ms
  objectUrl = URL.createObjectURL(ms)
  videoRef.value.src = objectUrl
  videoRef.value.play()?.catch(() => {})
  hasVideo.value = false
  presentedMediaTime = 0
  ms.addEventListener('sourceopen', () => {
    // SourceBuffer 延迟到收到 init 段（携带真实 codec）后创建，
    // 避免 codec 猜错导致的无谓重建。sourceopen 只标记就绪。
    if (ms.readyState !== 'open') return
    tryAttachBuffer()
  })
}

// 用 init 携带的 codec 创建 SourceBuffer（幂等）。
function tryAttachBuffer() {
  if (sourceBuffer || !mediaSource || mediaSource.readyState !== 'open') return
  if (!codecString) return // 等首个 init 段告知 codec
  try {
    const sb = mediaSource.addSourceBuffer(codecString)
    sb.mode = 'segments'
    sb.addEventListener('updateend', flushQueue)
    sb.addEventListener('error', onSourceBufferError)
    sourceBuffer = sb
    flushQueue()
  } catch (e) {
    console.warn('[CameraTile] addSourceBuffer failed:', e, codecString)
    scheduleReset()
  }
}

// 队列串联：SourceBuffer updating 期间不可 append，updateend 驱动下一段。
function flushQueue() {
  if (!sourceBuffer || sourceBuffer.updating || awaitingInit) return
  while (appendQueue.length > 0) {
    const next = appendQueue.shift()!
    try {
      sourceBuffer.appendBuffer(next as BufferSource)
      return // 本次 append 异步，等 updateend 再续
    } catch (e) {
      const name = (e as DOMException).name
      if (name === 'QuotaExceededError') {
        evictBuffer()
        continue // 丢弃本段，清出空间后继续（实时流允许丢段）
      }
      console.warn('[CameraTile] appendBuffer:', e)
      scheduleReset()
      return
    }
  }
}

function onSourceBufferError(e: Event) {
  console.warn('[CameraTile] SourceBuffer error:', e)
  scheduleReset()
}

// initBytesEqual 比较两段 init 的字节内容（同长度时逐字节比对）。
function initBytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false
  }
  return true
}

// 缓冲瘦身：实时流只保留播放点附近的历史，防止 SourceBuffer 无限增长。
function evictBuffer() {
  const v = videoRef.value
  const sb = sourceBuffer
  if (!v || !sb || sb.updating) return
  try {
    const b = sb.buffered
    if (b.length > 0 && b.start(0) < v.currentTime - 5) {
      sb.remove(b.start(0), v.currentTime - 5)
    }
  } catch {
    /* ignore */
  }
}

// fMP4 数据入口：kind=init（携带 codec，建立 SourceBuffer）或 frag（媒体段）。
// key 为后端按 IDR 标记的关键帧标志。
function feedFMP4(kind: string, codec: string, base64Data: string, serverTs?: number, key = false) {
  if (decodeDegraded.value) return
  if (!mediaSource) createPipeline()
  if (!mediaSource) return

  if (serverTs && props.clockOffset !== null) {
    const raw = Date.now() + props.clockOffset - serverTs
    if (raw >= 0 && raw < 5000) {
      transitEma = transitEma === null ? raw : transitEma * 0.85 + raw * 0.15
    }
  }

  const binary = atob(base64Data)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)

  if (kind === 'init') {
    if (!awaitingInit && sourceBuffer) {
      // 就绪后对随 GOP 重发的 init 做字节比较（init 段仅几百字节，代价
      // 可忽略）：相同则忽略；不同说明编码参数变化（SPS 更换、分辨率
      // 切换），旧 SourceBuffer 无法复用，整条管线重建。此前仅比较
      // codec 字符串的分支在稳态下不可达（init 在此之前已被忽略）。
      if (lastInitBytes && initBytesEqual(lastInitBytes, bytes)) return
      lastInitBytes = bytes
      if (codec) codecString = `video/mp4; codecs="${codec}"`
      scheduleReset()
      return
    }
    if (codec) codecString = `video/mp4; codecs="${codec}"`
    lastInitBytes = bytes
    appendQueue.length = 0
    appendQueue.push(bytes)
    if (pendingKeySeg) appendQueue.push(pendingKeySeg) // 缓存的关键帧段紧随 init
    awaitingInit = false
    tryAttachBuffer() // sourceopen 可能早于首个 init，此刻才知 codec
    flushQueue()
    return
  }

  if (kind !== 'frag') return
  if (awaitingInit || !sourceBuffer) {
    // 管线未就绪：只保留最近一个关键帧段（播放必须关键帧起步），
    // 其余丢弃——否则首个 append 的起点会落在缓冲之外（实测卡 rs1）。
    if (key) pendingKeySeg = bytes
    return
  }
  appendQueue.push(bytes)
  if (appendQueue.length > 64) appendQueue.shift() // 防积压：丢最旧段
  flushQueue()
  if (inSnapshot.value && !props.cam.mjpeg) inSnapshot.value = false
  hasVideo.value = true
}

function feedJpeg(base64Data: string) {
  if (!imgRef.value) return
  imgRef.value.src = `data:image/jpeg;base64,${base64Data}`
  hasVideo.value = true
  inSnapshot.value = true
}

// 带节流的管线重建：解码错误可能连续触发，无节流会陷入重建风暴。
// 连续快速重建达阈值后停止自愈并显示引导：典型场景是远程桌面/虚拟显示器
// 环境下浏览器 GPU 硬解整体不可用，重建多少次都无效，唯一解法是关闭
// 浏览器硬件加速——继续重建只会表现为持续黑闪。
let lastResetAt = 0
let resetTimes: number[] = []
const decodeDegraded = ref(false)
function scheduleReset() {
  const now = Date.now()
  if (now - lastResetAt < 1000) return
  resetTimes = resetTimes.filter((t) => now - t < 60_000)
  resetTimes.push(now)
  if (resetTimes.length >= 5) {
    decodeDegraded.value = true
    destroyPipeline() // 停止无效重建；feedFMP4 在降级态直接丢弃
    return
  }
  lastResetAt = now
  resetPipeline()
}

// 用户点击"已关闭硬件加速，重试"或 WS 重连后重新开始自愈
function retryDecode() {
  decodeDegraded.value = false
  resetTimes = []
  lastResetAt = 0
  resetPipeline()
}

// video 元素 error 置位（MEDIA_ERROR_DECODE 等）后 MSE 立即失效，
// 后续 feed 全部作废 —— 必须整条管线重建（新 MediaSource + 解码器重初始化）
function onVideoElementError() {
  scheduleReset()
}

// 重建整条展示管线（WS 重连 / 显式重置时调用）
function resetPipeline() {
  destroyPipeline()
  transitEma = null
  latencyMs.value = 0
  createPipeline()
}

onMounted(() => {
  createPipeline()
  videoRef.value?.addEventListener('error', onVideoElementError)
  const video = videoRef.value as any
  if (video && typeof video.requestVideoFrameCallback === 'function') {
    const onFrame = (_now: number, metadata: { mediaTime: number }) => {
      presentedMediaTime = metadata.mediaTime
      rVfcHandle = video.requestVideoFrameCallback(onFrame)
    }
    rVfcHandle = video.requestVideoFrameCallback(onFrame)
  }
  latencyTimer = setInterval(measurePresentationLag, 500)
  evictTimer = setInterval(evictBuffer, 5000)
  registry?.register(routeKey(), { feedFMP4, feedJpeg })
})

onUnmounted(() => {
  registry?.unregister(routeKey(), { feedFMP4, feedJpeg })
  videoRef.value?.removeEventListener('error', onVideoElementError)
  if (latencyTimer) clearInterval(latencyTimer)
  if (evictTimer) clearInterval(evictTimer)
  const video = videoRef.value as any
  if (video && rVfcHandle && typeof video.cancelVideoFrameCallback === 'function') {
    video.cancelVideoFrameCallback(rVfcHandle)
  }
  rVfcHandle = 0
  destroyPipeline()
})

function measurePresentationLag() {
  const video = videoRef.value
  if (!video || hasVideo.value === false || inSnapshot.value) return
  const buffered = video.buffered
  if (buffered.length === 0) return

  // 缓冲起点超前兜底：播放点落在缓冲区间之外（如关键帧起步丢段/重建后
  // 时间轴错位）时视频会卡在 rs1 永远等数据——直接跳到缓冲起点。
  if (!video.seeking && video.currentTime + 0.3 < buffered.start(0)) {
    try {
      video.currentTime = buffered.start(0)
    } catch {
      /* ignore */
    }
  }

  const presented = presentedMediaTime > 0 ? presentedMediaTime : video.currentTime
  if (presented <= 0) return
  let bufferedEnd = -1
  for (let i = 0; i < buffered.length; i++) {
    if (buffered.start(i) <= presented + 0.25 && buffered.end(i) > bufferedEnd) {
      bufferedEnd = buffered.end(i)
    }
  }
  if (bufferedEnd <= 0) return
  const lagMs = Math.max(0, (bufferedEnd - presented) * 1000)
  const total = (transitEma ?? 0) + lagMs
  if (total > 0 && total < 30000) {
    latencyMs.value = Math.round(total / 10) * 10
  }

  // 追帧：落后超过 3s（后台标签页、解码抖动等）时跳到直播沿，
  // 保留 0.3s 余量避免反复 seek；页面不可见时浏览器 seek 行为不可靠，跳过
  const now = Date.now()
  if (
    lagMs > 3000 &&
    !video.seeking &&
    document.visibilityState === 'visible' &&
    now - lastCatchUpAt > 3000
  ) {
    try {
      video.currentTime = Math.max(0, bufferedEnd - 0.3)
      lastCatchUpAt = now
    } catch {
      /* seek 失败忽略，下个节拍重试 */
    }
  }
}

// WS 重连后递增 resetKey：旧管线的 MSE 时间轴已作废，重建；
// 若处于硬解降级态也一并重试（用户可能已改完浏览器设置后刷新页面）
watch(
  () => props.resetKey,
  (key) => {
    if (key > 0) {
      decodeDegraded.value = false
      resetTimes = []
      resetPipeline()
    }
  },
)

// 摄像头切换到快照降级时显示角标
watch(
  () => props.cam.snapshot,
  (snap) => {
    if (snap) inSnapshot.value = true
  },
)

function ptzMove(direction: string) {
  emit('ptz', direction)
}
void ptzMove

defineExpose({ feedFMP4, feedJpeg })
</script>

<template>
  <div class="cam-tile" :class="{ 'cam-tile--active': active }">
    <video
      ref="videoRef"
      class="cam-tile__video"
      :class="{ 'cam-tile__video--hidden': inSnapshot }"
      autoplay
      muted
      playsinline
    ></video>
    <img
      ref="imgRef"
      class="cam-tile__snapshot"
      :class="{ 'cam-tile__snapshot--visible': inSnapshot }"
    />

    <div v-if="showLabel" class="cam-tile__label" :title="cam.name">{{ cam.name }}</div>
    <!-- 左下角状态角标：快照降级提示 + 分辨率（未知时不显示） -->
    <div v-if="(inSnapshot && !cam.mjpeg) || resolution" class="cam-tile__badges">
      <span v-if="inSnapshot && !cam.mjpeg" class="cam-tile__badge">快照</span>
      <span v-if="resolution" class="cam-tile__res" :title="`画面分辨率 ${resolution}`">{{ resolution }}</span>
    </div>
    <div
      v-if="hasVideo && !inSnapshot && latencyMs > 0"
      class="cam-tile__latency"
      title="从后端收到 RTSP 帧到画面呈现的延迟。不含摄像头采集/编码、以及摄像头到后端的网络传输延迟"
    >
      ⏱ {{ latencyMs }}ms
    </div>

    <div v-if="active && cam.ptz && cam.streaming" class="cam-tile__ptz">
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--up"
        title="上（按住转动）"
        @pointerdown.prevent="emit('ptz', 'hold:up')"
        @pointerup="emit('ptz', 'stop')"
        @pointerleave="emit('ptz', 'stop')"
        @pointercancel="emit('ptz', 'stop')"
      >▲</button>
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--left"
        title="左（按住转动）"
        @pointerdown.prevent="emit('ptz', 'hold:left')"
        @pointerup="emit('ptz', 'stop')"
        @pointerleave="emit('ptz', 'stop')"
        @pointercancel="emit('ptz', 'stop')"
      >◀</button>
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--home"
        title="回到初始位置"
        @pointerdown.prevent="emit('ptz', 'home')"
      >⌂</button>
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--right"
        title="右（按住转动）"
        @pointerdown.prevent="emit('ptz', 'hold:right')"
        @pointerup="emit('ptz', 'stop')"
        @pointerleave="emit('ptz', 'stop')"
        @pointercancel="emit('ptz', 'stop')"
      >▶</button>
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--down"
        title="下（按住转动）"
        @pointerdown.prevent="emit('ptz', 'hold:down')"
        @pointerup="emit('ptz', 'stop')"
        @pointerleave="emit('ptz', 'stop')"
        @pointercancel="emit('ptz', 'stop')"
      >▼</button>
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--zoomin"
        title="变焦+（按住）"
        @pointerdown.prevent="emit('ptz', 'hold:zoom_in')"
        @pointerup="emit('ptz', 'stop')"
        @pointerleave="emit('ptz', 'stop')"
        @pointercancel="emit('ptz', 'stop')"
      >＋</button>
      <button
        class="cam-tile__ptz-btn cam-tile__ptz-btn--zoomout"
        title="变焦-（按住）"
        @pointerdown.prevent="emit('ptz', 'hold:zoom_out')"
        @pointerup="emit('ptz', 'stop')"
        @pointerleave="emit('ptz', 'stop')"
        @pointercancel="emit('ptz', 'stop')"
      >－</button>
    </div>

    <div v-if="!hasVideo && !decodeDegraded" class="cam-tile__placeholder">暂无画面</div>

    <!-- 硬解不可用降级提示：远程桌面/虚拟显示器环境下 GPU 解码整体失效，
         继续重建只会黑闪；引导用户关闭浏览器硬件加速后重试 -->
    <div v-if="decodeDegraded" class="cam-tile__degraded">
      <div class="cam-tile__degraded-title">解码持续失败</div>
      <div class="cam-tile__degraded-text">
        当前环境浏览器硬件解码不可用（远程桌面常见）。请在浏览器设置中关闭
        「硬件加速/图形加速」并重启浏览器后刷新页面。
      </div>
      <button class="cam-tile__degraded-retry" @click="retryDecode">已调整，重试</button>
    </div>
  </div>
</template>

<style scoped>
.cam-tile {
  position: relative;
  background: #020408;
  overflow: hidden;
  min-height: 0;
}

.cam-tile--active {
  outline: 1px solid rgba(0, 229, 160, 0.55);
}

.cam-tile__video,
.cam-tile__snapshot {
  width: 100%;
  height: 100%;
  object-fit: contain;
  display: block;
}

.cam-tile__video--hidden {
  display: none;
}

.cam-tile__snapshot {
  position: absolute;
  inset: 0;
  opacity: 0;
  transition: opacity 0.15s;
  pointer-events: none;
}

.cam-tile__snapshot--visible {
  opacity: 1;
}

.cam-tile__label {
  position: absolute;
  top: 6px;
  left: 8px;
  z-index: 2;
  font-size: 0.68rem;
  color: rgba(255, 255, 255, 0.85);
  background: rgba(0, 0, 0, 0.45);
  padding: 2px 8px;
  border-radius: 3px;
  max-width: calc(100% - 16px);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cam-tile__badges {
  position: absolute;
  bottom: 6px;
  left: 8px;
  z-index: 2;
  display: flex;
  gap: 4px;
}

.cam-tile__badge {
  font-size: 0.65rem;
  color: #ffce4d;
  background: rgba(0, 0, 0, 0.45);
  padding: 1px 6px;
  border-radius: 3px;
}

.cam-tile__res {
  font-family: var(--font-mono, monospace);
  font-size: 0.65rem;
  color: rgba(255, 255, 255, 0.7);
  background: rgba(0, 0, 0, 0.45);
  padding: 1px 6px;
  border-radius: 3px;
}

.cam-tile__latency {
  position: absolute;
  top: 6px;
  right: 8px;
  z-index: 2;
  font-family: var(--font-mono, monospace);
  font-size: 0.65rem;
  color: rgba(255, 255, 255, 0.7);
  background: rgba(0, 0, 0, 0.45);
  padding: 1px 6px;
  border-radius: 3px;
}

.cam-tile__ptz {
  position: absolute;
  bottom: 40px;
  right: 10px;
  z-index: 3;
  display: grid;
  grid-template-columns: repeat(3, 30px);
  grid-template-rows: repeat(3, 30px);
  gap: 2px;
}

.cam-tile__ptz-btn {
  border: 1px solid rgba(255, 255, 255, 0.3);
  background: rgba(0, 0, 0, 0.5);
  color: rgba(255, 255, 255, 0.85);
  border-radius: 4px;
  cursor: pointer;
  font-size: 0.7rem;
  display: flex;
  align-items: center;
  justify-content: center;
}

.cam-tile__ptz-btn:active {
  background: rgba(0, 229, 160, 0.35);
}

.cam-tile__ptz-btn--up {
  grid-column: 2;
  grid-row: 1;
}
.cam-tile__ptz-btn--left {
  grid-column: 1;
  grid-row: 2;
}
.cam-tile__ptz-btn--home {
  grid-column: 2;
  grid-row: 2;
}
.cam-tile__ptz-btn--right {
  grid-column: 3;
  grid-row: 2;
}
.cam-tile__ptz-btn--down {
  grid-column: 2;
  grid-row: 3;
}
.cam-tile__ptz-btn--zoomin {
  grid-column: 1;
  grid-row: 3;
}
.cam-tile__ptz-btn--zoomout {
  grid-column: 3;
  grid-row: 3;
}

.cam-tile__placeholder {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--color-text-dim, #6b7a90);
  font-size: 0.75rem;
  letter-spacing: 0.06em;
  pointer-events: none;
}

/* 硬解降级提示：覆盖整块画面，深色底保证可读 */
.cam-tile__degraded {
  position: absolute;
  inset: 0;
  z-index: 5;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 16px;
  background: rgba(2, 4, 8, 0.92);
  text-align: center;
}
.cam-tile__degraded-title {
  color: #ffce4d;
  font-size: 0.85rem;
  font-weight: 600;
  letter-spacing: 0.04em;
}
.cam-tile__degraded-text {
  color: rgba(255, 255, 255, 0.75);
  font-size: 0.75rem;
  line-height: 1.6;
  max-width: 260px;
}
.cam-tile__degraded-retry {
  border: 1px solid rgba(0, 229, 160, 0.5);
  background: rgba(0, 229, 160, 0.12);
  color: var(--color-accent-green, #00e5a0);
  font-size: 0.78rem;
  padding: 5px 16px;
  border-radius: 4px;
  cursor: pointer;
}
.cam-tile__degraded-retry:hover {
  background: rgba(0, 229, 160, 0.22);
}
</style>
