<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useMicCapture } from '../composables/useMicCapture'
import { useAudioPlayer } from '../composables/useAudioPlayer'
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

const { messages, isConnected, send, popNewMessages } = useWebSocket('/ws')

watch(isConnected, (connected) => {
  console.log('[VoicePanel] WebSocket connected:', connected)
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

const { playChunk } = useAudioPlayer()

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

watch(messages, () => {
  const newMsgs = popNewMessages()
  for (const msg of newMsgs) {
    console.log('[VoicePanel] Received WS message:', msg.type, msg.payload || msg.text || '')
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
    if (msg.type === 'audio_out' && msg.data) {
      console.log('[VoicePanel] Playing audio chunk, length:', msg.data.length)
      playChunk(msg.data)
    }
    if (msg.type === 'error' && msg.text) {
      console.error('[VoicePanel] Error:', msg.text)
      errorMsg.value = msg.text
      talkStatus.value = 'idle'
    }
  }
}, { deep: false })
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
