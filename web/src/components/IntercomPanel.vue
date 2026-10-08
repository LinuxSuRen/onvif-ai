<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useTalkbackCapture } from '../composables/useTalkback'
import { useWebSocket } from '../composables/useWebSocket'

/**
 * 语音对讲（核心功能）：按住按钮把浏览器麦克风音频实时推到摄像头端
 * 扬声器。通道可用态来自 device_state.talkback（backchannel 连接结果），
 * 会话受理结果经 talkback_state 回执；与 TTS 播报互斥（占用中禁用）。
 */
interface TalkbackInfo {
  available: boolean
  reason?: string
}

// 服务器返回的稳定原因码 → 用户可读文案；未知码走兜底
const TALKBACK_REASONS: Record<string, string> = {
  no_backchannel: '该设备不支持对讲',
  busy: '语音播报占用中，请稍后再试',
  talkback_in_use: '对讲通道占用中，请稍后再试',
}

function talkbackReasonText(reason?: string): string {
  if (!reason) return '对讲不可用'
  return TALKBACK_REASONS[reason] || `对讲不可用（${reason}）`
}

const { isConnected, send, subscribe } = useWebSocket('/ws')

const talkbackInfo = ref<TalkbackInfo | null>(null)
const talkbackPressed = ref(false)
const talkbackActive = ref(false)
const intercomError = ref('')
const ttsSpeaking = ref(false)

const talkback = useTalkbackCapture({
  onChunk: (base64Pcm) => send({ type: 'audio_in', data: base64Pcm }),
})

const intercomEnabled = computed(
  () =>
    isConnected.value &&
    talkbackInfo.value?.available === true &&
    !talkbackPressed.value &&
    !ttsSpeaking.value,
)

const intercomLabel = computed(() => {
  if (talkbackPressed.value) return '对讲中 · 松开结束'
  if (!isConnected.value) return '服务未连接'
  const info = talkbackInfo.value
  if (!info) return '连接摄像头后可对讲'
  if (!info.available) {
    if (info.reason === 'no_backchannel') return '设备不支持对讲'
    if (info.reason === 'connect_failed') return '对讲通道连接失败'
    return '对讲连接中'
  }
  if (ttsSpeaking.value) return '播报占用中'
  return '按住对讲'
})

async function startIntercom() {
  if (!intercomEnabled.value) return
  talkbackPressed.value = true
  intercomError.value = ''
  send({ type: 'talkback_start' })
  // getUserMedia 在按钮按下（用户手势）调用栈内触发，满足权限交互要求
  const err = await talkback.start()
  if (err) {
    talkbackPressed.value = false
    send({ type: 'talkback_stop' }) // 麦克风失败同样要释放服务端会话
    intercomError.value = err
  }
}

function stopIntercom() {
  if (!talkbackPressed.value) return
  talkbackPressed.value = false
  talkbackActive.value = false
  talkback.stop() // 先停本地采集，再释放服务端会话
  send({ type: 'talkback_stop' })
}

subscribe(['device_state'], (msg) => {
  const tb = (msg.payload as { talkback?: unknown } | null | undefined)?.talkback as
    | TalkbackInfo
    | undefined
  if (tb && typeof tb === 'object' && typeof tb.available === 'boolean') {
    talkbackInfo.value = { ...tb }
  } else {
    talkbackInfo.value = null
  }
})

subscribe(['talkback_state'], (msg) => {
  const p = msg.payload as { active?: boolean; reason?: string } | undefined
  if (p?.active) {
    talkbackActive.value = true
    return
  }
  talkbackActive.value = false
  // 会话被拒（占用/不可用）：立即停止本地采集并提示
  if (talkbackPressed.value) {
    talkbackPressed.value = false
    talkback.abort()
    intercomError.value = talkbackReasonText(p?.reason)
  }
})

// TTS 播报占用回传通道期间禁用对讲（后端同样互斥，这里是即时的前端反馈）
subscribe(['status'], (msg) => {
  const state = (msg.payload as { state?: string } | undefined)?.state
  ttsSpeaking.value = state === 'speaking'
})

// 本面板常驻可见，顺带承接全局错误提示（对讲中断、取流失败等）
subscribe(['error'], (msg) => {
  if (msg.text) intercomError.value = msg.text
})

watch(isConnected, (connected) => {
  if (!connected && talkbackPressed.value) {
    // 断连后服务端会按属主断开自动释放会话，这里只需收尾本地采集
    talkbackPressed.value = false
    talkbackActive.value = false
    talkback.abort()
    intercomError.value = '连接已断开，对讲结束'
  }
})
</script>

<template>
  <div class="intercom-panel">
    <div class="intercom-panel__header">
      <span class="intercom-panel__title">语音对讲</span>
      <span class="intercom-panel__hint">喊话直达摄像头扬声器</span>
    </div>

    <button
      class="intercom-panel__btn"
      :class="{ 'intercom-panel__btn--active': talkbackPressed }"
      :disabled="!intercomEnabled"
      @mousedown.prevent="startIntercom"
      @mouseup.prevent="stopIntercom"
      @mouseleave.prevent="stopIntercom"
      @touchstart.prevent="startIntercom"
      @touchend.prevent="stopIntercom"
    >
      <svg class="intercom-panel__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
        <rect x="9" y="2" width="6" height="12" rx="3" />
        <path d="M5 10a7 7 0 0 0 14 0" />
        <line x1="12" y1="17" x2="12" y2="21" />
      </svg>
      <span class="intercom-panel__label">{{ intercomLabel }}</span>
    </button>

    <div v-if="intercomError" class="intercom-panel__error">{{ intercomError }}</div>
  </div>
</template>

<style scoped>
.intercom-panel {
  background: var(--color-bg-panel, #131820);
  border: 1px solid var(--color-border-subtle, #1e2530);
  border-radius: 8px;
  padding: 12px 16px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.intercom-panel__header {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.intercom-panel__title {
  font-size: 0.75rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: #7a8490;
}

.intercom-panel__hint {
  font-size: 0.62rem;
  color: #5a6470;
}

/* 对讲是核心操作：全宽大按钮，触屏目标充足 */
.intercom-panel__btn {
  width: 100%;
  min-height: 64px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid #1e2530;
  background: #0a0e14;
  color: #7a8490;
  font-size: 0.85rem;
  letter-spacing: 0.04em;
  cursor: pointer;
  transition: all 0.2s;
  user-select: none;
  -webkit-user-select: none;
  touch-action: none; /* 按住对讲时避免触发页面滚动 */
}

.intercom-panel__btn:not(:disabled):hover {
  border-color: #0091ff;
  color: #9fb3c8;
}

.intercom-panel__btn:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.intercom-panel__btn--active {
  border-color: #00e5a0;
  color: #00e5a0;
  background: rgba(0, 229, 160, 0.08);
  box-shadow: 0 0 24px rgba(0, 229, 160, 0.25);
}

.intercom-panel__icon {
  width: 20px;
  height: 20px;
  flex-shrink: 0;
}

.intercom-panel__error {
  font-size: 0.7rem;
  color: #ff3d57;
  background: rgba(255, 61, 87, 0.08);
  padding: 6px 10px;
  border-radius: 4px;
  width: 100%;
  text-align: center;
}
</style>
