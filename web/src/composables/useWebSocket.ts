import { ref, onUnmounted, type Ref } from 'vue'

/**
 * WebSocket message from the ONVIF AI backend.
 */
export interface WsMessage {
  type: 'video_nal' | 'video_jpeg' | 'audio_out' | 'transcript' | 'status' | 'error' | 'audio_start' | 'audio_data' | 'audio_stop' | 'speech_text' | 'switch_mode' | 'device_state'
  data?: string
  text?: string
  payload?: {
    state?: string
    [key: string]: unknown
  }
}

function parseMessage(raw: string): WsMessage | null {
  try {
    return JSON.parse(raw) as WsMessage
  } catch {
    return null
  }
}

export function useWebSocket(url: string) {
  const messages: Ref<WsMessage[]> = ref([])
  const isConnected = ref(false)
  const isConnecting = ref(false)
  let processedCount = 0

  let ws: WebSocket | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let shouldReconnect = true

  function connect() {
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
      return
    }

    isConnecting.value = true
    ws = new WebSocket(url)
    console.log('[WS] Connecting to:', url)

    ws.onopen = () => {
      console.log('[WS] Connected')
      isConnected.value = true
      isConnecting.value = false
    }

    ws.onclose = () => {
      isConnected.value = false
      isConnecting.value = false
      ws = null
      if (shouldReconnect) {
        reconnectTimer = setTimeout(connect, 2000)
      }
    }

    ws.onerror = () => {}

    ws.onmessage = (event: MessageEvent) => {
      const msg = parseMessage(event.data)
      if (msg) {
        messages.value = [...messages.value, msg]
      }
    }
  }

  function disconnect() {
    shouldReconnect = false
    if (reconnectTimer) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
    if (ws) {
      ws.close()
      ws = null
    }
    isConnected.value = false
    isConnecting.value = false
  }

  function send(data: unknown): void {
    if (ws && ws.readyState === WebSocket.OPEN) {
      const json = JSON.stringify(data)
      console.log('[WS] Sending:', json.substring(0, 200))
      ws.send(json)
    } else {
      console.warn('[WS] Cannot send - socket state:', ws?.readyState)
    }
  }

  function popNewMessages(): WsMessage[] {
    const all = messages.value
    if (processedCount >= all.length) return []
    const newMsgs = all.slice(processedCount)
    processedCount = all.length
    return newMsgs
  }
  onUnmounted(() => {
    disconnect()
  })

  connect()

  return {
    messages,
    isConnected,
    isConnecting,
    send,
    disconnect,
    connect,
    popNewMessages,
  }
}
