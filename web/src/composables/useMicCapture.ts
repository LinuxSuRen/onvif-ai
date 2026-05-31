import { ref, type Ref } from 'vue'

type MicStatus = 'idle' | 'capturing' | 'error'

interface MicCaptureOptions {
  onSpeechResult: (text: string, isFinal: boolean) => void
  onSpeechFinal: (text: string) => void
  onStatusChange?: (status: MicStatus, error?: string) => void
}

export function useMicCapture(options: MicCaptureOptions) {
  const status: Ref<MicStatus> = ref('idle')
  const errorMessage = ref('')

  let recognition: any = null
  let finalTranscript = ''
  let stopping = false

  const SpeechRecognitionAPI: any =
    (window as any).SpeechRecognition || (window as any).webkitSpeechRecognition

  function reportStatus(s: MicStatus, err?: string) {
    status.value = s
    if (err !== undefined) errorMessage.value = err
    options.onStatusChange?.(s, err)
  }

  function start(): void {
    if (status.value === 'capturing') return

    if (!SpeechRecognitionAPI) {
      reportStatus('error', '浏览器不支持语音识别（需要 Chrome 或 Edge）')
      return
    }

    try {
      recognition = new SpeechRecognitionAPI()
      recognition.lang = 'zh-CN'
      recognition.interimResults = true
      recognition.continuous = false
      finalTranscript = ''
      stopping = false

      recognition.onresult = (event: any) => {
        let interim = ''
        for (let i = event.resultIndex; i < event.results.length; i++) {
          const result = event.results[i]
          if (result.isFinal) {
            finalTranscript += result[0].transcript
          } else {
            interim += result[0].transcript
          }
        }
        if (finalTranscript) {
          options.onSpeechResult(finalTranscript, true)
        }
        if (interim) {
          options.onSpeechResult(interim, false)
        }
      }

      recognition.onerror = (event: any) => {
        if (event.error === 'no-speech' || event.error === 'aborted') {
          return
        }
        reportStatus('error', `语音识别错误: ${event.error}`)
      }

      recognition.onend = () => {
        reportStatus('idle')
        if (stopping) {
          stopping = false
          const text = finalTranscript
          finalTranscript = ''
          recognition = null
          options.onSpeechFinal(text)
        }
      }

      recognition.start()
      reportStatus('capturing')
    } catch (err) {
      reportStatus('error', err instanceof Error ? err.message : '无法启动语音识别')
    }
  }

  function stop(): void {
    if (recognition) {
      stopping = true
      recognition.stop()
    } else {
      options.onSpeechFinal('')
    }
  }

  function abort(): void {
    if (recognition) {
      stopping = false
      recognition.abort()
      recognition = null
    }
    finalTranscript = ''
    reportStatus('idle')
  }

  return { status, errorMessage, start, stop, abort }
}
