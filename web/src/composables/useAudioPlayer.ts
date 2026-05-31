import { ref, type Ref } from 'vue'

type PlayerStatus = 'idle' | 'playing'

export function useAudioPlayer() {
  const status: Ref<PlayerStatus> = ref('idle')

  let audioContext: AudioContext | null = null
  let nextPlayTime = 0
  const SAMPLE_RATE = 16000

  function ensureContext(): AudioContext {
    if (!audioContext || audioContext.state === 'closed') {
      audioContext = new AudioContext({ sampleRate: SAMPLE_RATE })
      nextPlayTime = audioContext.currentTime
    }
    if (audioContext.state === 'suspended') {
      audioContext.resume()
    }
    return audioContext
  }

  function playChunk(base64Pcm: string): void {
    console.log('[AudioPlayer] Playing chunk, base64 length:', base64Pcm.length)
    const ctx = ensureContext()
    const pcmBuffer = base64ToInt16Array(base64Pcm)

    if (pcmBuffer.length === 0) {
      console.warn('[AudioPlayer] Empty PCM buffer')
      return
    }

    console.log('[AudioPlayer] PCM samples:', pcmBuffer.length, 'at', SAMPLE_RATE, 'Hz')

    const audioBuffer = ctx.createBuffer(1, pcmBuffer.length, SAMPLE_RATE)
    const channelData = audioBuffer.getChannelData(0)

    for (let i = 0; i < pcmBuffer.length; i++) {
      channelData[i] = pcmBuffer[i] / 32768
    }

    const sourceNode = ctx.createBufferSource()
    sourceNode.buffer = audioBuffer
    sourceNode.connect(ctx.destination)

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

  function stop(): void {
    if (audioContext && audioContext.state !== 'closed') {
      audioContext.close().catch(() => {})
      audioContext = null
    }
    nextPlayTime = 0
    status.value = 'idle'
  }

  return {
    status,
    playChunk,
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
