import { ref } from 'vue'

/**
 * 对讲采集（浏览器麦克风 → 摄像头扬声器）：
 * getUserMedia 采麦克风 → AudioWorklet 在音频线程降采样为 16kHz 单声道
 * PCM16（不支持 AudioWorklet 的环境回退 ScriptProcessor）→ 每 100ms 一片
 * base64 后经 audio_in 消息发给后端，后端写入 RTSP backchannel。
 *
 * 会话协议（talkback_start / audio_in / talkback_stop）由调用方
 * （VoicePanel）负责，本组合式函数只管采集与分片。
 */

const TARGET_SAMPLE_RATE = 16000
const CHUNK_SAMPLES = 1600 // 100ms @16k，兼顾延迟与消息条数

/**
 * AudioWorklet 处理器源码（音频线程执行）：
 * 线性插值降采样（跨块保留小数残留，避免相位漂移与周期性杂音），
 * 多声道先混成单声道，攒满 CHUNK_SAMPLES 后以 Int16 PCM 转移式发送。
 */
const WORKLET_SOURCE = `
class PcmDownsamplerProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super()
    const opts = (options && options.processorOptions) || {}
    this.targetRate = opts.targetRate || ${TARGET_SAMPLE_RATE}
    this.chunkSamples = opts.chunkSamples || ${CHUNK_SAMPLES}
    this.buffer = new Float32Array(this.chunkSamples)
    this.filled = 0
    this.remainder = 0
  }
  process(inputs) {
    const channels = inputs[0]
    if (!channels || channels.length === 0) return true
    const src = channels[0]
    if (!src || src.length === 0) return true

    let mono = src
    if (channels.length > 1) {
      mono = new Float32Array(src.length)
      for (let i = 0; i < src.length; i++) {
        let sum = 0
        for (let c = 0; c < channels.length; c++) sum += channels[c][i]
        mono[i] = sum / channels.length
      }
    }

    const ratio = sampleRate / this.targetRate
    const last = mono.length - 1
    let pos = this.remainder
    while (pos < last) {
      const i = Math.floor(pos)
      const frac = pos - i
      this.buffer[this.filled++] = mono[i] * (1 - frac) + mono[i + 1] * frac
      if (this.filled >= this.chunkSamples) this.flush()
      pos += ratio
    }
    this.remainder = pos - last
    return true
  }
  flush() {
    const pcm = new Int16Array(this.filled)
    for (let i = 0; i < this.filled; i++) {
      const s = Math.max(-1, Math.min(1, this.buffer[i]))
      pcm[i] = s < 0 ? s * 0x8000 : s * 0x7fff
    }
    this.port.postMessage(pcm.buffer, [pcm.buffer])
    this.filled = 0
  }
}
registerProcessor('pcm-downsampler', PcmDownsamplerProcessor)
`

function arrayBufferToBase64(buf: ArrayBufferLike): string {
  const bytes = new Uint8Array(buf)
  let binary = ''
  const step = 0x8000 // 分段拼接，避免一次展开超出参数个数上限
  for (let i = 0; i < bytes.length; i += step) {
    binary += String.fromCharCode(...bytes.subarray(i, i + step))
  }
  return btoa(binary)
}

function micErrorMessage(err: unknown): string {
  if (err instanceof DOMException) {
    switch (err.name) {
      case 'NotAllowedError':
        return '麦克风权限被拒绝，请在浏览器设置中允许后重试'
      case 'NotFoundError':
        return '未检测到可用的麦克风设备'
      case 'NotReadableError':
        return '麦克风被其他应用占用，无法采集'
      default:
        return `麦克风采集失败：${err.name}`
    }
  }
  return err instanceof Error ? `麦克风采集失败：${err.message}` : '麦克风采集失败'
}

/** 主线程降采样器：ScriptProcessor 回退路径，算法与 Worklet 一致。 */
class Downsampler {
  private ratio: number
  private remainder = 0
  private buffer = new Float32Array(CHUNK_SAMPLES)
  private filled = 0

  constructor(inputRate: number, targetRate: number) {
    this.ratio = inputRate / targetRate
  }

  push(src: Float32Array, emit: (pcm: Int16Array) => void): void {
    const last = src.length - 1
    let pos = this.remainder
    while (pos < last) {
      const i = Math.floor(pos)
      const frac = pos - i
      this.buffer[this.filled++] = src[i] * (1 - frac) + src[i + 1] * frac
      if (this.filled >= CHUNK_SAMPLES) this.flush(emit)
      pos += this.ratio
    }
    this.remainder = pos - last
  }

  private flush(emit: (pcm: Int16Array) => void): void {
    const pcm = new Int16Array(this.filled)
    for (let i = 0; i < this.filled; i++) {
      const s = Math.max(-1, Math.min(1, this.buffer[i]))
      pcm[i] = s < 0 ? s * 0x8000 : s * 0x7fff
    }
    emit(pcm)
    this.filled = 0
  }
}

export interface TalkbackCaptureOptions {
  /** 每攒满一个 PCM16 分片（约 100ms）回调一次，参数为 base64。 */
  onChunk: (base64Pcm: string) => void
}

export function useTalkbackCapture(options: TalkbackCaptureOptions) {
  const capturing = ref(false)

  let stream: MediaStream | null = null
  let audioCtx: AudioContext | null = null
  let source: MediaStreamAudioSourceNode | null = null
  let sink: GainNode | null = null
  let workletNode: AudioWorkletNode | null = null
  let processorNode: ScriptProcessorNode | null = null
  let fallback: Downsampler | null = null
  let workletURL: string | null = null
  // 代际令牌：start() 的 await 期间若发生 stop/abort（如会话被服务器
  // 拒绝），迟到的 getUserMedia / addModule 结果按过期丢弃，不会重新
  // 拉起一条无人消费的采集链路。
  let generation = 0

  function teardown(): void {
    generation++
    try {
      workletNode?.disconnect()
      processorNode?.disconnect()
      source?.disconnect()
      sink?.disconnect()
    } catch {
      // 节点已随上下文关闭时忽略
    }
    workletNode?.port.close()
    stream?.getTracks().forEach((track) => track.stop())
    if (audioCtx && audioCtx.state !== 'closed') {
      audioCtx.close().catch(() => {})
    }
    if (workletURL) {
      URL.revokeObjectURL(workletURL)
      workletURL = null
    }
    workletNode = null
    processorNode = null
    source = null
    sink = null
    stream = null
    audioCtx = null
    fallback = null
    capturing.value = false
  }

  /**
   * 开始采集。必须在用户手势调用栈内触发（对讲按钮按下天然满足），
   * 否则浏览器会拒绝授予麦克风。返回错误文案，null 表示成功。
   */
  async function start(): Promise<string | null> {
    if (capturing.value) return null
    const gen = ++generation
    if (!navigator.mediaDevices?.getUserMedia) {
      return '当前浏览器不支持麦克风采集'
    }

    let media: MediaStream
    try {
      media = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true, // 摄像头音频在外放时抑制回声
          noiseSuppression: true,
          autoGainControl: true,
        },
        video: false,
      })
    } catch (err) {
      console.error('[Talkback] getUserMedia failed:', err)
      return micErrorMessage(err)
    }
    if (gen !== generation) {
      media.getTracks().forEach((track) => track.stop()) // 等待期间会话已结束
      return null
    }

    const ctx = new AudioContext()
    if (ctx.state === 'suspended') {
      ctx.resume().catch(() => {})
    }

    stream = media
    audioCtx = ctx
    source = ctx.createMediaStreamSource(media)
    sink = ctx.createGain()
    sink.gain.value = 0 // 仅驱动音频图，不向本机扬声器回放

    let usedWorklet = false
    if (ctx.audioWorklet) {
      try {
        workletURL = URL.createObjectURL(new Blob([WORKLET_SOURCE], { type: 'application/javascript' }))
        await ctx.audioWorklet.addModule(workletURL)
        if (gen !== generation) {
          teardown() // 模块加载期间会话已结束
          return null
        }
        workletNode = new AudioWorkletNode(ctx, 'pcm-downsampler', {
          numberOfInputs: 1,
          numberOfOutputs: 1,
          processorOptions: { targetRate: TARGET_SAMPLE_RATE, chunkSamples: CHUNK_SAMPLES },
        })
        workletNode.port.onmessage = (event: MessageEvent<ArrayBuffer>) => {
          if (event.data && event.data.byteLength > 0) {
            options.onChunk(arrayBufferToBase64(event.data))
          }
        }
        source.connect(workletNode)
        workletNode.connect(sink)
        sink.connect(ctx.destination)
        usedWorklet = true
      } catch (err) {
        console.warn('[Talkback] AudioWorklet unavailable, fallback to ScriptProcessor:', err)
        workletNode = null
      }
    }

    if (!usedWorklet) {
      processorNode = ctx.createScriptProcessor(4096, 1, 1)
      fallback = new Downsampler(ctx.sampleRate, TARGET_SAMPLE_RATE)
      processorNode.onaudioprocess = (event: AudioProcessingEvent) => {
        fallback?.push(event.inputBuffer.getChannelData(0), (pcm) =>
          options.onChunk(arrayBufferToBase64(pcm.buffer)),
        )
      }
      source.connect(processorNode)
      processorNode.connect(sink)
      sink.connect(ctx.destination)
    }

    capturing.value = true
    return null
  }

  return { capturing, start, stop: teardown, abort: teardown }
}
