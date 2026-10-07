<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useMicCapture } from '../composables/useMicCapture'
import { useTalkbackCapture } from '../composables/useTalkback'
import { useWebSocket } from '../composables/useWebSocket'

type TalkStatus = 'idle' | 'listening' | 'thinking' | 'speaking'
type AudioMode = 'browser_mic' | 'camera_mic'

const talkStatus = ref<TalkStatus>('idle')
const transcript = ref('')
const interimText = ref('')
const errorMsg = ref('')
const audioMode = ref<AudioMode>('browser_mic')
let fullResponseText = ''
const availableVoices = ref<SpeechSynthesisVoice[]>([])
const selectedVoice = ref('')

const { isConnected, send, subscribe } = useWebSocket('/ws')

// ---- 对讲（浏览器麦克风 → 摄像头扬声器）----
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

const talkbackInfo = ref<TalkbackInfo | null>(null)
const talkbackPressed = ref(false)
const talkbackActive = ref(false)
const intercomError = ref('')

const talkback = useTalkbackCapture({
  onChunk: (base64Pcm) => send({ type: 'audio_in', data: base64Pcm }),
})

const intercomEnabled = computed(
  () =>
    isConnected.value &&
    talkbackInfo.value?.available === true &&
    !talkbackPressed.value &&
    talkStatus.value !== 'speaking',
)

const intercomLabel = computed(() => {
  if (talkbackPressed.value) return '对讲中 · 松开结束'
  if (!isConnected.value) return '服务未连接'
  const info = talkbackInfo.value
  if (!info) return '对讲未就绪'
  if (!info.available) {
    if (info.reason === 'no_backchannel') return '设备不支持对讲'
    if (info.reason === 'connect_failed') return '对讲通道连接失败'
    return '对讲连接中'
  }
  if (talkStatus.value === 'speaking') return '播报占用中'
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

watch(isConnected, (connected) => {
  console.log('[VoicePanel] WebSocket connected:', connected)
  if (!connected && talkbackPressed.value) {
    // 断连后服务端会按属主断开自动释放会话，这里只需收尾本地采集
    talkbackPressed.value = false
    talkbackActive.value = false
    talkback.abort()
    intercomError.value = '连接已断开，对讲结束'
  }
})

const handleSpeechResult = (text: string, isFinal: boolean) => {
  console.log('[VoicePanel] Speech result:', { text, isFinal })
  if (isFinal) {
    transcript.value = text
  } else {
    interimText.value = text
  }
}

const { start: micStart, stop: micStop } = useMicCapture({
  onSpeechResult: handleSpeechResult,
  onSpeechFinal: (text: string) => {
    console.log('[VoicePanel] Speech final:', text)
    if (text) {
      talkStatus.value = 'thinking'
      const msg = { type: 'speech_text', text }
      console.log('[VoicePanel] Sending speech_text:', msg)
      send(msg)
    } else {
      talkStatus.value = 'idle'
      console.log('[VoicePanel] No text recognized')
    }
  },
})

// 摄像头实时音频的播放已移至 AudioMonitor（视频画面右下角），
// 本面板只负责语音对话；浏览器侧 TTS 播报用 SpeechSynthesis。

const statusLabel = computed(() => {
  const labels: Record<TalkStatus, string> = {
    idle: '按住说话',
    listening: '正在聆听...',
    thinking: 'AI 思考中...',
    speaking: '播放回复中...',
  }
  return labels[talkStatus.value]
})

const isPressed = ref(false)

function speakResponse() {
  if (!fullResponseText) return
  console.log('[VoicePanel] Speaking via SpeechSynthesis:', fullResponseText.substring(0, 50))

  const utterance = new SpeechSynthesisUtterance(fullResponseText)
  utterance.lang = 'zh-CN'
  utterance.rate = 1.0
  utterance.pitch = 1.0

  loadVoices()
  if (selectedVoice.value) {
    const v = availableVoices.value.find(vo => vo.name === selectedVoice.value)
    if (v) utterance.voice = v
  } else {
    const zhCN = availableVoices.value.find(v => v.lang === 'zh-CN')
    const zh = availableVoices.value.find(v => v.lang.startsWith('zh-CN'))
    utterance.voice = zhCN || zh || null
  }

  speechSynthesis.speak(utterance)
}

function loadVoices() {
  const voices = speechSynthesis.getVoices()
  if (voices.length > 0) {
    availableVoices.value = voices.filter(v => v.lang.startsWith('zh'))
  }
}

function startTalk() {
  if (!isConnected.value) return
  isPressed.value = true
  errorMsg.value = ''

  if (audioMode.value === 'camera_mic') {
    talkStatus.value = 'listening'
    send({ type: 'camera_listen' })
    return
  }

  talkStatus.value = 'listening'
  transcript.value = ''
  interimText.value = ''
  micStart()
}

function stopTalk() {
  if (!isPressed.value) return
  isPressed.value = false

  if (audioMode.value === 'camera_mic') {
    console.log('[VoicePanel] Camera listen stop')
    return
  }

  console.log('[VoicePanel] Stopping mic capture...')
  micStop()
}

subscribe(['transcript', 'status', 'error'], (msg) => {
  {
    if (msg.type === 'transcript' && msg.text) {
      if (msg.text === '\n\n') {
        speakResponse()
      } else {
        transcript.value += msg.text
        fullResponseText += msg.text
      }
    }
    if (msg.type === 'status' && msg.payload?.state) {
      const state = msg.payload.state
      console.log('[VoicePanel] Status change:', state)
      if (state === 'thinking') { transcript.value = ''; fullResponseText = ''; talkStatus.value = 'thinking' }
      if (state === 'speaking') talkStatus.value = 'speaking'
      if (state === 'idle' && !isPressed.value) {
        talkStatus.value = 'idle'
      }
    }
    if (msg.type === 'error' && msg.text) {
      console.error('[VoicePanel] Error:', msg.text)
      errorMsg.value = msg.text
      talkStatus.value = 'idle'
    }
  }
})
</script>

<template>
  <div class="voice-panel">
    <div class="voice-panel__header">
      <span class="voice-panel__title">语音对话</span>
      <span class="voice-panel__connection" :class="{ 'voice-panel__connection--online': isConnected }">
        {{ isConnected ? '已连接' : '未连接' }}
      </span>
    </div>

    <div class="voice-panel__body">
      <div class="voice-panel__mode-toggle">
        <button
          class="voice-panel__mode-option"
          :class="{ 'voice-panel__mode-option--active': audioMode === 'browser_mic' }"
          @click="audioMode = 'browser_mic'; send({ type: 'switch_mode', payload: { mode: 'browser_mic' } })"
        >
          浏览器麦克风
        </button>
        <button
          class="voice-panel__mode-option"
          :class="{ 'voice-panel__mode-option--active': audioMode === 'camera_mic' }"
          @click="audioMode = 'camera_mic'; send({ type: 'switch_mode', payload: { mode: 'camera_mic' } })"
        >
          摄像头麦克风
        </button>
      </div>

      <button
        class="voice-panel__talk-btn"
        :class="`voice-panel__talk-btn--${talkStatus}`"
        @mousedown.prevent="startTalk"
        @mouseup.prevent="stopTalk"
        @mouseleave.prevent="stopTalk"
        @touchstart.prevent="startTalk"
        @touchend.prevent="stopTalk"
      >
        <span class="voice-panel__talk-label">{{ statusLabel }}</span>
      </button>

      <!-- 对讲：按住说话，音频实时推到摄像头端扬声器 -->
      <button
        class="voice-panel__intercom-btn"
        :class="{ 'voice-panel__intercom-btn--active': talkbackPressed }"
        :disabled="!intercomEnabled"
        @mousedown.prevent="startIntercom"
        @mouseup.prevent="stopIntercom"
        @mouseleave.prevent="stopIntercom"
        @touchstart.prevent="startIntercom"
        @touchend.prevent="stopIntercom"
      >
        {{ intercomLabel }}
      </button>

      <div v-if="intercomError" class="voice-panel__error">{{ intercomError }}</div>

      <div v-if="errorMsg" class="voice-panel__error">{{ errorMsg }}</div>

      <div class="voice-panel__voice-select">
        <select v-model="selectedVoice" @focus="loadVoices" class="voice-panel__voice-dropdown">
          <option value="">自动选择（普通话）</option>
          <option v-for="v in availableVoices" :key="v.name" :value="v.name">
            {{ v.name }} ({{ v.lang }})
          </option>
        </select>
      </div>

      <div v-if="transcript" class="voice-panel__transcript">
        <div class="voice-panel__transcript-label">对话记录</div>
        <div class="voice-panel__transcript-text">{{ transcript }}</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.voice-panel {
  background: var(--color-bg-panel, #131820);
  border: 1px solid var(--color-border-subtle, #1e2530);
  border-radius: 8px;
  padding: 12px 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.voice-panel__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.voice-panel__title {
  font-size: 0.75rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: #7a8490;
}

.voice-panel__connection {
  font-size: 0.65rem;
  color: #ff3d57;
}

.voice-panel__connection--online {
  color: #00e5a0;
}

.voice-panel__body {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
}

.voice-panel__mode-toggle {
  display: flex;
  width: 100%;
  background: #0a0e14;
  border-radius: 6px;
  padding: 2px;
  border: 1px solid #1e2530;
}

.voice-panel__mode-option {
  flex: 1;
  padding: 5px 10px;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: #5a6470;
  font-size: 0.7rem;
  cursor: pointer;
  transition: all 0.15s;
  letter-spacing: 0.03em;
}

.voice-panel__mode-option:hover {
  color: #7a8490;
}

.voice-panel__mode-option--active {
  background: #1e2530;
  color: #00e5a0;
}

.voice-panel__talk-btn {
  width: 96px;
  height: 96px;
  border-radius: 50%;
  border: 2px solid #1e2530;
  background: #0a0e14;
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  transition: all 0.2s;
  user-select: none;
}

.voice-panel__talk-btn:hover {
  border-color: #0091ff;
  box-shadow: 0 0 20px rgba(0, 145, 255, 0.15);
}

.voice-panel__talk-btn--listening {
  border-color: #00e5a0;
  background: rgba(0, 229, 160, 0.08);
  box-shadow: 0 0 28px rgba(0, 229, 160, 0.25);
}

.voice-panel__talk-btn--thinking {
  border-color: #ffb800;
  background: rgba(255, 184, 0, 0.08);
  animation: pulse 1.2s infinite;
}

.voice-panel__talk-btn--speaking {
  border-color: #0091ff;
  background: rgba(0, 145, 255, 0.08);
}

@keyframes pulse {
  0%, 100% { box-shadow: 0 0 16px rgba(255, 184, 0, 0.15); }
  50% { box-shadow: 0 0 32px rgba(255, 184, 0, 0.35); }
}

.voice-panel__talk-label {
  font-size: 0.7rem;
  color: #7a8490;
  text-align: center;
  padding: 8px;
  line-height: 1.3;
}

/* ---- 对讲按钮：全宽条形，与语音问答按钮区分 ---- */
.voice-panel__intercom-btn {
  width: 100%;
  min-height: 44px; /* 触屏可按目标 */
  padding: 8px 12px;
  border-radius: 6px;
  border: 1px solid #1e2530;
  background: #0a0e14;
  color: #7a8490;
  font-size: 0.75rem;
  letter-spacing: 0.03em;
  cursor: pointer;
  transition: all 0.2s;
  user-select: none;
  -webkit-user-select: none;
  touch-action: none; /* 按住对讲时避免触发页面滚动 */
}

.voice-panel__intercom-btn:not(:disabled):hover {
  border-color: #0091ff;
  color: #9fb3c8;
}

.voice-panel__intercom-btn:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.voice-panel__intercom-btn--active {
  border-color: #00e5a0;
  color: #00e5a0;
  background: rgba(0, 229, 160, 0.08);
  box-shadow: 0 0 20px rgba(0, 229, 160, 0.2);
}

.voice-panel__talk-btn--listening .voice-panel__talk-label {
  color: #00e5a0;
}

.voice-panel__talk-btn--thinking .voice-panel__talk-label {
  color: #ffb800;
}

.voice-panel__talk-btn--speaking .voice-panel__talk-label {
  color: #0091ff;
}

.voice-panel__error {
  font-size: 0.7rem; color: #ff3d57;
  background: rgba(255, 61, 87, 0.08); padding: 6px 10px;
  border-radius: 4px; width: 100%; text-align: center;
}

.voice-panel__voice-select {
  width: 100%;
}

.voice-panel__voice-dropdown {
  width: 100%; padding: 4px 8px;
  background: #0a0e14; border: 1px solid #1e2530;
  border-radius: 4px; color: #7a8490; font-size: 0.65rem;
}

.voice-panel__transcript {
  width: 100%;
  max-height: 160px;
  overflow-y: auto;
}

.voice-panel__transcript-label {
  font-size: 0.6rem;
  color: #5a6470;
  margin-bottom: 3px;
}

.voice-panel__transcript-text {
  font-size: 0.75rem;
  color: #c8d0d8;
  line-height: 1.5;
  white-space: pre-wrap;
}
</style>
