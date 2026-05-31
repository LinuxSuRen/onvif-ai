<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch, nextTick } from 'vue'
import JMuxer from 'jmuxer'
import { useWebSocket } from '../composables/useWebSocket'

const videoRef = ref<HTMLVideoElement | null>(null)
const imgRef = ref<HTMLImageElement | null>(null)
const connectionStatus = ref<'disconnected' | 'connecting' | 'connected'>('connecting')
const hasStream = ref(false)
const isSnapshotMode = ref(false)
const videoError = ref('')
let frameCount = 0
let lastFrameTime = 0

let jmuxer: JMuxer | null = null

const { messages, isConnected, isConnecting, popNewMessages } = useWebSocket('/ws')

function initJMuxer() {
  if (!videoRef.value) return

  jmuxer = new JMuxer({
    node: videoRef.value,
    mode: 'video',
    videoCodec: 'H264',
    flushingTime: 100,
    debug: false,
    onError: () => {
      // JMuxer error — stream will auto-recover on next keyframe
    },
  })
}

function feedVideoNal(base64Data: string) {
  if (!jmuxer) return

  const binary = atob(base64Data)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i)
  }

  jmuxer.feed({ video: bytes })
  if (!hasStream.value) {
    hasStream.value = true
  }
}

watch(isConnected, (connected) => {
  if (connected) {
    connectionStatus.value = 'connected'
    nextTick(() => {
      if (!jmuxer && videoRef.value) {
        initJMuxer()
      }
    })
  } else {
    connectionStatus.value = 'disconnected'
    hasStream.value = false
  }
})

watch(isConnecting, (connecting) => {
  if (connecting) {
    connectionStatus.value = 'connecting'
  }
})

function feedJPEG(base64Data: string) {
  if (!imgRef.value) return

  imgRef.value.src = `data:image/jpeg;base64,${base64Data}`
  if (!hasStream.value) {
    hasStream.value = true
    isSnapshotMode.value = true
  }
}

watch(messages, () => {
  const newMsgs = popNewMessages()
  for (const msg of newMsgs) {
    if (msg.type === 'video_nal' && msg.data) {
      frameCount++
      lastFrameTime = Date.now()
      if (frameCount === 1) console.log('[VideoPlayer] First H.264 NAL received')
      feedVideoNal(msg.data)
    }
    if (msg.type === 'video_jpeg' && msg.data) {
      frameCount++
      lastFrameTime = Date.now()
      if (frameCount === 1) console.log('[VideoPlayer] First JPEG snapshot received, size:', msg.data.length)
      feedJPEG(msg.data)
    }
    if (msg.type === 'device_state' && msg.payload) {
      const state = msg.payload as any
      console.log('[VideoPlayer] Device state:', state)
      if (state.streaming) {
        videoError.value = ''
      } else if (state.snapshot_mode) {
        videoError.value = ''
      }
    }
    if (msg.type === 'error' && msg.text) {
      console.error('[VideoPlayer] Error:', msg.text)
      videoError.value = msg.text
    }
  }
}, { deep: false })

setInterval(() => {
  if (isConnected.value && !hasStream.value && lastFrameTime === 0) {
    videoError.value = '等待视频流...'
  }
  if (isConnected.value && hasStream.value && Date.now() - lastFrameTime > 5000) {
    videoError.value = '视频流中断'
  }
}, 3000)

onMounted(() => {
  if (videoRef.value) {
    initJMuxer()
  }
})

onUnmounted(() => {
  if (jmuxer) {
    jmuxer.destroy()
    jmuxer = null
  }
})
</script>

<template>
  <div class="video-player">
    <div class="video-player__header">
      <div class="video-player__title">
        <span class="video-player__indicator" :class="`video-player__indicator--${connectionStatus}`"></span>
        <span class="video-player__label">实时视频流</span>
      </div>
      <span class="video-player__status-label" :class="`video-player__status-label--${connectionStatus}`">
        {{ connectionStatus === 'connected' ? '在线' : connectionStatus === 'connecting' ? '连接中...' : '断开' }}
      </span>
    </div>

    <div class="video-player__viewport">
      <video
        ref="videoRef"
        class="video-player__video"
        :class="{ 'video-player__video--hidden': isSnapshotMode }"
        autoplay
        muted
        playsinline
      ></video>
      <img
        ref="imgRef"
        class="video-player__snapshot"
        :class="{ 'video-player__snapshot--visible': isSnapshotMode }"
      />
      <div v-if="!hasStream" class="video-player__placeholder">
        <span class="video-player__placeholder-icon">📷</span>
        <span class="video-player__placeholder-text">等待视频流...</span>
      </div>
    </div>
    <div v-if="isSnapshotMode" class="video-player__snapshot-badge">
      快照模式 (1 FPS)
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
  justify-content: space-between;
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

.video-player__video {
  width: 100%;
  height: 100%;
  object-fit: contain;
  display: block;
}

.video-player__overlay {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgba(6, 10, 16, 0.85);
}

.video-player__overlay-content {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--space-4);
}

.video-player__spinner {
  width: 40px;
  height: 40px;
  border: 2px solid var(--color-border-default);
  border-top-color: var(--color-accent-blue);
  border-radius: var(--radius-full);
  opacity: 0;
  transition: opacity var(--transition-base);
}

.video-player__spinner--active {
  opacity: 1;
  animation: spin 0.8s linear infinite;
}

.video-player__overlay-text {
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  color: var(--color-text-dim);
  letter-spacing: 0.04em;
}

.video-player__video--hidden {
  display: none;
}

.video-player__snapshot {
  display: none;
  width: 100%;
  height: 100%;
  object-fit: contain;
}

.video-player__snapshot--visible {
  display: block;
}

.video-player__snapshot-badge {
  padding: var(--space-1) var(--space-3);
  background: rgba(255, 184, 0, 0.12);
  color: var(--color-warning);
  font-size: 0.6875rem;
  text-align: center;
  letter-spacing: 0.04em;
}

.video-player__error {
  padding: 6px 10px;
  background: rgba(255, 61, 87, 0.1);
  color: #ff3d57;
  font-size: 0.72rem;
  text-align: center;
}
</style>
