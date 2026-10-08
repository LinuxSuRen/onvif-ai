<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, inject } from 'vue'
import JMuxer from 'jmuxer'

/**
 * CameraTile 承载一路摄像头画面（对应一个 ONVIF media profile）：
 * 独立的 jmuxer/MSE 解码、快照降级显示、端到端延迟测量与云台控制。
 */
const props = defineProps<{
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

let jmuxer: JMuxer | null = null
let transitEma: number | null = null
let presentedMediaTime = 0
let rVfcHandle = 0
let latencyTimer: ReturnType<typeof setInterval> | null = null
let jmuxerErrTimes: number[] = []
let lastCatchUpAt = 0

// 向父组件注册本 tile 的数据入口（生命周期内自管理，避免 ref 顺序竞态）
const registry = inject<{
  register: (token: string, api: { feedNal: typeof feedNal; feedJpeg: typeof feedJpeg }) => void
  unregister: (token: string, api: { feedNal: typeof feedNal; feedJpeg: typeof feedJpeg }) => void
} | null>('cameraTileRegistry', null)

function destroyJMuxer() {
  if (jmuxer) {
    try {
      jmuxer.destroy()
    } catch {
      /* destroy 在 MSE 已关闭时可能抛错，忽略 */
    }
    jmuxer = null
  }
}

function createJMuxer() {
  if (!videoRef.value || jmuxer) return
  jmuxer = new JMuxer({
    node: videoRef.value,
    mode: 'video',
    videoCodec: 'H264',
    flushingTime: 100,
    debug: false,
    onError: handleJMuxerError,
  })
  videoRef.value.play()?.catch(() => {})
  hasVideo.value = false // 等待新管线首帧
  presentedMediaTime = 0
}

// jmuxer 错误分三类：QuotaExceeded（内部自动清理）、InvalidStateError
// （内部自动 reset）、其余（endMSE，管线永久失效）。前两类之外的错误，
// 或 60s 内反复出错时直接重建管线 —— 后端在每个 IDR 前重发 SPS/PPS，
// 一个 GOP 内即可自动恢复画面。
function handleJMuxerError(error: unknown) {
  console.warn('[CameraTile] jmuxer error:', error)
  const data = error as { name?: string } | null | undefined
  const now = Date.now()
  jmuxerErrTimes = jmuxerErrTimes.filter((t) => now - t < 60_000)
  jmuxerErrTimes.push(now)
  const fatal = data && data.name !== 'QuotaExceeded' && data.name !== 'InvalidStateError'
  if (fatal || jmuxerErrTimes.length >= 3) {
    jmuxerErrTimes = []
    destroyJMuxer()
    createJMuxer()
  }
}

// 重建整条展示管线（WS 重连 / 显式重置时调用）
function resetPipeline() {
  destroyJMuxer()
  transitEma = null
  latencyMs.value = 0
  createJMuxer()
}

onMounted(() => {
  createJMuxer()
  const video = videoRef.value as any
  if (video && typeof video.requestVideoFrameCallback === 'function') {
    const onFrame = (_now: number, metadata: { mediaTime: number }) => {
      presentedMediaTime = metadata.mediaTime
      rVfcHandle = video.requestVideoFrameCallback(onFrame)
    }
    rVfcHandle = video.requestVideoFrameCallback(onFrame)
  }
  latencyTimer = setInterval(measurePresentationLag, 500)
  registry?.register(props.cam.token, { feedNal, feedJpeg })
})

onUnmounted(() => {
  registry?.unregister(props.cam.token, { feedNal, feedJpeg })
  if (latencyTimer) clearInterval(latencyTimer)
  const video = videoRef.value as any
  if (video && rVfcHandle && typeof video.cancelVideoFrameCallback === 'function') {
    video.cancelVideoFrameCallback(rVfcHandle)
  }
  rVfcHandle = 0
  if (jmuxer) {
    jmuxer.destroy()
    jmuxer = null
  }
})

function feedNal(serverTs: number | undefined, base64Data: string) {
  if (!jmuxer) createJMuxer()
  if (!jmuxer) return

  // 传输延迟：服务器收帧时刻 → 本端收到（时钟偏移由父组件校准）
  if (serverTs && props.clockOffset !== null) {
    const raw = Date.now() + props.clockOffset - serverTs
    if (raw >= 0 && raw < 5000) {
      transitEma = transitEma === null ? raw : transitEma * 0.85 + raw * 0.15
    }
  }

  const binary = atob(base64Data)
  // jmuxer 的 H.264 解析器按 Annex-B 起始码（00 00 00 01）切分 NAL，
  // 裸 NAL 无法被提取，需逐个加起始码后再喂入
  const bytes = new Uint8Array(4 + binary.length)
  bytes[0] = 0
  bytes[1] = 0
  bytes[2] = 0
  bytes[3] = 1
  for (let i = 0; i < binary.length; i++) {
    bytes[i + 4] = binary.charCodeAt(i)
  }

  jmuxer.feed({ video: bytes })
  // RTSP 恢复直播后退出快照视图（video 元素重新可见）
  if (inSnapshot.value && !props.cam.mjpeg) inSnapshot.value = false
  hasVideo.value = true
}

function feedJpeg(base64Data: string) {
  if (!imgRef.value) return
  imgRef.value.src = `data:image/jpeg;base64,${base64Data}`
  hasVideo.value = true
  inSnapshot.value = true
}

function measurePresentationLag() {
  const video = videoRef.value
  if (!video || !hasVideo.value || inSnapshot.value) return
  const presented = presentedMediaTime > 0 ? presentedMediaTime : video.currentTime
  if (presented <= 0) return
  const buffered = video.buffered
  if (buffered.length === 0) return
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

// WS 重连后递增 resetKey：旧管线的 MSE 时间轴已作废，重建
watch(
  () => props.resetKey,
  (key) => {
    if (key > 0) resetPipeline()
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

defineExpose({ feedNal, feedJpeg })
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
      <button class="cam-tile__ptz-btn cam-tile__ptz-btn--up" @pointerdown.prevent="ptzMove('up')">▲</button>
      <button class="cam-tile__ptz-btn cam-tile__ptz-btn--left" @pointerdown.prevent="ptzMove('left')">◀</button>
      <button class="cam-tile__ptz-btn cam-tile__ptz-btn--right" @pointerdown.prevent="ptzMove('right')">▶</button>
      <button class="cam-tile__ptz-btn cam-tile__ptz-btn--down" @pointerdown.prevent="ptzMove('down')">▼</button>
    </div>

    <div v-if="!hasVideo" class="cam-tile__placeholder">暂无画面</div>
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
  display: none;
}

.cam-tile__snapshot--visible {
  display: block;
}

.cam-tile__label {
  position: absolute;
  top: 6px;
  left: 6px;
  z-index: 4;
  max-width: 60%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  padding: 2px 8px;
  border-radius: 4px;
  background: rgba(0, 0, 0, 0.55);
  color: rgba(255, 255, 255, 0.9);
  font-size: 0.6875rem;
  letter-spacing: 0.04em;
  pointer-events: none;
}

/* 左下角角标行：快照提示与分辨率同排，互不遮挡 */
.cam-tile__badges {
  position: absolute;
  bottom: 6px;
  left: 6px;
  z-index: 4;
  display: flex;
  align-items: center;
  gap: 4px;
  max-width: calc(100% - 12px);
  pointer-events: none;
}

.cam-tile__badge {
  padding: 2px 8px;
  border-radius: 4px;
  background: rgba(255, 184, 0, 0.15);
  color: rgba(255, 184, 0, 0.95);
  font-size: 0.65rem;
  white-space: nowrap;
}

.cam-tile__res {
  padding: 2px 8px;
  border-radius: 4px;
  background: rgba(0, 0, 0, 0.55);
  color: rgba(255, 255, 255, 0.85);
  font-family: var(--font-mono);
  font-size: 0.65rem;
  letter-spacing: 0.04em;
  white-space: nowrap;
}

.cam-tile__latency {
  position: absolute;
  top: 6px;
  right: 6px;
  z-index: 4;
  padding: 2px 8px;
  border-radius: 4px;
  background: rgba(0, 0, 0, 0.55);
  color: rgba(0, 229, 160, 0.95);
  font-family: var(--font-mono);
  font-size: 0.65rem;
  pointer-events: none;
}

.cam-tile__ptz {
  position: absolute;
  inset: 0;
  pointer-events: none;
  z-index: 5;
}

.cam-tile__ptz-btn {
  position: absolute;
  width: 36px;
  height: 36px;
  border: 1px solid rgba(0, 229, 160, 0.3);
  background: rgba(0, 0, 0, 0.5);
  color: rgba(0, 229, 160, 0.7);
  font-size: 0.9rem;
  cursor: pointer;
  pointer-events: auto;
  border-radius: 4px;
  display: flex;
  align-items: center;
  justify-content: center;
}

.cam-tile__ptz-btn:hover {
  background: rgba(0, 229, 160, 0.15);
  color: #00e5a0;
}

/* 触屏：触摸目标不小于 44px，避免误触 */
@media (pointer: coarse) {
  .cam-tile__ptz-btn {
    width: 44px;
    height: 44px;
    font-size: 1.1rem;
  }
}

.cam-tile__ptz-btn--up { top: 4px; left: 50%; transform: translateX(-50%); }
.cam-tile__ptz-btn--down { bottom: 4px; left: 50%; transform: translateX(-50%); }
.cam-tile__ptz-btn--left { left: 4px; top: 50%; transform: translateY(-50%); }
.cam-tile__ptz-btn--right { right: 4px; top: 50%; transform: translateY(-50%); }

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
</style>
