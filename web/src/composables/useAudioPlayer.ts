import { ref, type Ref } from 'vue'

type PlayerStatus = 'idle' | 'playing'

/**
 * 兼容旧后端：audio_out 消息未携带采样率元数据时的默认播放采样率。
 */
const DEFAULT_SAMPLE_RATE = 16000

export function useAudioPlayer() {
  const status: Ref<PlayerStatus> = ref('idle')

  let audioContext: AudioContext | null = null
  let gainNode: GainNode | null = null
  let contextRate = 0
  let nextPlayTime = 0
  let volume = 1

  function ensureContext(sampleRate: number): AudioContext {
    // AudioContext 的采样率在创建时固定；源切换导致采样率变化时必须重建
    if (audioContext && (audioContext.state === 'closed' || contextRate !== sampleRate)) {
      audioContext.close().catch(() => {})
      audioContext = null
      gainNode = null
    }
    if (!audioContext) {
      audioContext = new AudioContext({ sampleRate })
      contextRate = sampleRate
      gainNode = audioContext.createGain()
      gainNode.gain.value = volume
      gainNode.connect(audioContext.destination)
      nextPlayTime = audioContext.currentTime
    }
    if (audioContext.state === 'suspended') {
      audioContext.resume()
    }
    return audioContext
  }

  /**
   * 播放一段线性 PCM（16 位小端，base64 编码）。
   * sampleRate / channels 来自后端 audio_out 消息携带的源轨道参数
   * （如摄像头的 G.711 8kHz 单声道）；缺省时按旧协议默认值处理。
   */
  function playChunk(base64Pcm: string, sampleRate?: number, channels?: number): void {
    const rate = sampleRate && sampleRate > 0 ? sampleRate : DEFAULT_SAMPLE_RATE
    const ch = channels && channels > 0 ? channels : 1
    const ctx = ensureContext(rate)
    const pcmBuffer = base64ToInt16Array(base64Pcm)

    if (pcmBuffer.length === 0) {
      console.warn('[AudioPlayer] Empty PCM buffer')
      return
    }

    const frames = Math.floor(pcmBuffer.length / ch)
    if (frames === 0) {
      return
    }

    const audioBuffer = ctx.createBuffer(ch, frames, rate)
    for (let c = 0; c < ch; c++) {
      const channelData = audioBuffer.getChannelData(c)
      for (let i = 0; i < frames; i++) {
        channelData[i] = pcmBuffer[i * ch + c] / 32768
      }
    }

    const sourceNode = ctx.createBufferSource()
    sourceNode.buffer = audioBuffer
    sourceNode.connect(gainNode ?? ctx.destination)

    const now = ctx.currentTime
    if (nextPlayTime < now) {
      nextPlayTime = now
    }

    sourceNode.start(nextPlayTime)
    nextPlayTime += audioBuffer.duration

    status.value = 'playing'

    sourceNode.onended = () => {
      if (ctx.currentTime >= nextPlayTime - 0.05) {
        status.value = 'idle'
      }
    }
  }

  /** 设置音量（0..1），静音与否由调用方决定是否继续喂入音频。 */
  function setVolume(v: number): void {
    volume = Math.min(1, Math.max(0, v))
    if (gainNode) {
      gainNode.gain.value = volume
    }
  }

  /**
   * 在用户手势事件处理器内调用以解锁 AudioContext（浏览器自动播放
   * 策略）：创建/恢复必须发生在手势调用栈中才可靠。
   */
  function resume(): void {
    if (audioContext && audioContext.state === 'suspended') {
      audioContext.resume()
    }
  }

  function stop(): void {
    if (audioContext && audioContext.state !== 'closed') {
      audioContext.close().catch(() => {})
      audioContext = null
    }
    gainNode = null
    contextRate = 0
    nextPlayTime = 0
    status.value = 'idle'
  }

  return {
    status,
    playChunk,
    setVolume,
    resume,
    stop,
  }
}

function base64ToInt16Array(base64: string): Int16Array {
  const binary = atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i)
  }
  return new Int16Array(bytes.buffer)
}
