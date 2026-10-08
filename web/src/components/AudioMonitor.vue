<script setup lang="ts">
import { ref, computed } from 'vue'
import { useWebSocket } from '../composables/useWebSocket'
import { usePaused } from '../composables/usePaused'
import { useAudioPlayer } from '../composables/useAudioPlayer'

/**
 * 设备级音频监控（音频归属第一路画面）：
 *  - 状态展示：未知（未协商）/ 无音频 / 接收中（编码+采样率+声道）/ 解码降级
 *    数据来自 device_state.audio（连接时有状态种子，之后事件驱动，无轮询）
 *  - 播放控制：喇叭开关兼作自动播放策略的"启用声音"引导
 *    （浏览器要求用户交互后才允许出声，默认静音），附音量滑杆
 *  - 实时音频播放（audio_out PCM）在本组件完成，VoicePanel 不再参与
 */
interface AudioInfo {
  available: boolean
  codec?: string
  sample_rate?: number
  channels?: number
  degraded?: boolean
  reason?: string
}

const { subscribe } = useWebSocket('/ws')
const { paused } = usePaused()

const audioInfo = ref<AudioInfo | null>(null)

const statusKind = computed<'unknown' | 'none' | 'active' | 'degraded'>(() => {
  const a = audioInfo.value
  if (!a) return 'unknown'
  if (!a.available) return 'none'
  return a.degraded ? 'degraded' : 'active'
})

function formatRate(hz?: number): string {
  if (!hz || hz <= 0) return ''
  return hz % 1000 === 0 ? `${hz / 1000}kHz` : `${(hz / 1000).toFixed(1)}kHz`
}

function channelLabel(ch?: number): string {
  if (ch === 1) return '单声道'
  if (ch === 2) return '立体声'
  return ch && ch > 0 ? `${ch} 声道` : ''
}

const statusLabel = computed(() => {
  const a = audioInfo.value
  switch (statusKind.value) {
    case 'active': {
      const bits = [a?.codec, formatRate(a?.sample_rate), channelLabel(a?.channels)].filter(Boolean)
      return bits.length ? bits.join(' ') : '音频接收中'
    }
    case 'degraded':
      return '音频解码失败'
    case 'none':
      return '无音频'
    default:
      return '音频未就绪'
  }
})

// ---- 播放控制 ----
// 默认静音并记忆选择：既满足自动播放策略（用户点按后才出声），
// 也避免刷新页面后突然外放。
const MUTE_STORAGE_KEY = 'onvif-ai-audio-muted'
const muted = ref(localStorage.getItem(MUTE_STORAGE_KEY) !== '0')
const volume = ref(1)
const { playChunk, setVolume, resume } = useAudioPlayer()

function toggleMute() {
  muted.value = !muted.value
  localStorage.setItem(MUTE_STORAGE_KEY, muted.value ? '1' : '0')
  if (!muted.value) {
    resume() // 必须在用户手势调用栈内解锁 AudioContext
  }
}

function onVolume(e: Event) {
  volume.value = Number((e.target as HTMLInputElement).value)
  setVolume(volume.value)
}

subscribe(['device_state'], (msg) => {
  const a = (msg.payload as { audio?: unknown } | null | undefined)?.audio as AudioInfo | undefined
  if (a && typeof a === 'object' && typeof a.available === 'boolean') {
    audioInfo.value = { ...a }
  } else {
    audioInfo.value = null
  }
})

subscribe(['audio_out'], (msg) => {
  // 暂停时同步静音：帧照收照弃（与视频画面冻结一致）
  if (!msg.data || muted.value || paused.value) return
  const meta = msg.payload as { rate?: unknown; channels?: unknown } | undefined
  const rate = typeof meta?.rate === 'number' && meta.rate > 0 ? meta.rate : undefined
  const channels =
    typeof meta?.channels === 'number' && meta.channels > 0 ? meta.channels : undefined
  playChunk(msg.data, rate, channels)
})
</script>

<template>
  <div class="audio-monitor" :class="`audio-monitor--${statusKind}`" role="group" aria-label="音频监控">
    <button
      class="audio-monitor__toggle"
      :class="{ 'audio-monitor__toggle--prompt': statusKind === 'active' && muted }"
      :title="muted ? '启用声音' : '静音'"
      :aria-label="muted ? '启用声音' : '静音'"
      :aria-pressed="!muted"
      @click="toggleMute"
    >
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
        <path d="M11 5 6 9H2v6h4l5 4V5z" />
        <template v-if="!muted">
          <path d="M15.5 8.5a5 5 0 0 1 0 7" />
          <path d="M18.5 5.5a9 9 0 0 1 0 13" />
        </template>
        <template v-else>
          <line x1="16" y1="9" x2="22" y2="15" />
          <line x1="22" y1="9" x2="16" y2="15" />
        </template>
      </svg>
    </button>
    <input
      v-show="!muted"
      class="audio-monitor__volume"
      type="range"
      min="0"
      max="1"
      step="0.01"
      :value="volume"
      title="音量"
      aria-label="音量"
      @input="onVolume"
    />
    <span class="audio-monitor__status" :title="audioInfo?.reason || statusLabel">
      <span class="audio-monitor__dot"></span>
      <span class="audio-monitor__label">{{ statusLabel }}</span>
      <span v-if="statusKind === 'active' && muted" class="audio-monitor__hint">点喇叭开声</span>
    </span>
  </div>
</template>

<style scoped>
.audio-monitor {
  position: absolute;
  right: 8px;
  bottom: 8px;
  z-index: 20;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 3px 10px 3px 3px;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.7);
  backdrop-filter: blur(2px);
  max-width: calc(100% - 16px);
}

.audio-monitor__toggle {
  width: 32px;
  height: 32px;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: rgba(255, 255, 255, 0.65);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  flex-shrink: 0;
  transition: all 0.15s;
}

.audio-monitor__toggle svg {
  width: 18px;
  height: 18px;
}

.audio-monitor__toggle:hover {
  color: rgba(255, 255, 255, 0.95);
}

/* 接收中且静音：喇叭按钮高亮脉冲，引导点击启用声音（自动播放策略） */
.audio-monitor__toggle--prompt {
  background: rgba(0, 229, 160, 0.22);
  color: #00e5a0;
  animation: audio-prompt 1.6s ease-in-out infinite;
}

@keyframes audio-prompt {
  0%,
  100% {
    box-shadow: 0 0 4px rgba(0, 229, 160, 0.35);
  }
  50% {
    box-shadow: 0 0 14px rgba(0, 229, 160, 0.7);
  }
}

.audio-monitor__volume {
  width: 60px;
  accent-color: #00e5a0;
  cursor: pointer;
}

.audio-monitor__status {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.audio-monitor__dot {
  width: 7px;
  height: 7px;
  border-radius: var(--radius-full, 9999px);
  background: var(--color-text-muted, #3a5068);
  flex-shrink: 0;
}

.audio-monitor--active .audio-monitor__dot {
  background: var(--color-accent-green, #00e5a0);
  animation: audio-pulse 2s ease-in-out infinite;
}

.audio-monitor--degraded .audio-monitor__dot {
  background: var(--color-accent-amber, #ffb800);
}

@keyframes audio-pulse {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.35;
  }
}

.audio-monitor__label {
  font-family: var(--font-mono);
  font-size: 0.65rem;
  letter-spacing: 0.03em;
  color: rgba(255, 255, 255, 0.85);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.audio-monitor--degraded .audio-monitor__label {
  color: var(--color-accent-amber, #ffb800);
}

.audio-monitor__hint {
  font-size: 0.6rem;
  color: #00e5a0;
  white-space: nowrap;
  flex-shrink: 0;
}

/* 触屏：触摸目标不小于 40px，好按 */
@media (pointer: coarse) {
  .audio-monitor__toggle {
    width: 40px;
    height: 40px;
  }

  .audio-monitor__volume {
    width: 72px;
  }
}

/* 窄屏：限制文字宽度，防止把控制条撑出画面 */
@media (max-width: 640px) {
  .audio-monitor__label {
    max-width: 128px;
  }

  .audio-monitor__hint {
    display: none; /* 窄屏靠按钮脉冲引导，避免文字挤压 */
  }
}
</style>
