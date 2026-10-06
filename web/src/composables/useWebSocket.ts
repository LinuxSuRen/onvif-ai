import { ref } from 'vue'

/**
 * WebSocket message from the ONVIF AI backend.
 */
export interface WsMessage {
  type: 'video_nal' | 'video_jpeg' | 'audio_out' | 'transcript' | 'status' | 'error' | 'audio_start' | 'audio_data' | 'audio_stop' | 'speech_text' | 'switch_mode' | 'device_state' | 'ptz_move' | 'camera_listen' | 'clear_history' | 'clock_sync'
  data?: string
  text?: string
  ts?: number
  cam?: string
  payload?: {
    state?: string
    [key: string]: unknown
  }
}

/**
 * 全局共享单条 WebSocket 连接，按消息类型分发给订阅者。
 *
 * 之前每个组件各开一条连接，每条都在搬运完整视频流（高峰 ~70 条/秒 ×
 * 20KB），三份 JSON.parse 把主线程压垮，消费变慢后会被后端当作慢客户端
 * 断开，导致视频偶发断流。共享连接后每条消息只解析一次，且只分发给
 * 关心该类型的组件。
 */

type Handler = (msg: WsMessage) => void

interface Subscriber {
  types: Set<WsMessage['type']> | null // null = 订阅全部
  handler: Handler
}

const subscribers = new Set<Subscriber>()
const isConnected = ref(false)
const isConnecting = ref(false)

let ws: WebSocket | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let wsUrl = ''

function connect() {
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
    return
  }

  isConnecting.value = true
  ws = new WebSocket(wsUrl)
  console.log('[WS] Connecting to:', wsUrl)

  ws.onopen = () => {
    console.log('[WS] Connected')
    isConnected.value = true
    isConnecting.value = false
  }

  ws.onclose = () => {
    isConnected.value = false
    isConnecting.value = false
    ws = null
    if (subscribers.size > 0 && reconnectTimer == null) {
      reconnectTimer = setTimeout(() => {
        reconnectTimer = null
        connect()
      }, 2000)
    }
  }

  ws.onerror = () => {}

  ws.onmessage = (event: MessageEvent) => {
    let msg: WsMessage | null = null
    try {
      msg = JSON.parse(event.data) as WsMessage
    } catch {
      return
    }
    if (!msg) return
    for (const sub of subscribers) {
      if (sub.types === null || sub.types.has(msg.type)) {
        try {
          sub.handler(msg)
        } catch (err) {
          console.error('[WS] subscriber error:', err)
        }
      }
    }
  }
}

function ensureConnected(url: string) {
  if (!wsUrl) wsUrl = url
  if (!ws && subscribers.size > 0 && reconnectTimer == null) {
    connect()
  }
}

function maybeReconnectSooner() {
  // 订阅者加入时若连接断开且无重连排程，立即尝试
  if (!ws && reconnectTimer == null) {
    connect()
  }
}

export function useWebSocket(url: string) {
  const subscribe = (types: WsMessage['type'][] | null, handler: Handler): (() => void) => {
    const sub: Subscriber = { types: types ? new Set(types) : null, handler }
    subscribers.add(sub)
    ensureConnected(url)
    maybeReconnectSooner()
    return () => {
      subscribers.delete(sub)
    }
  }

  function send(data: unknown): void {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(data))
    } else {
      console.warn('[WS] Cannot send - socket state:', ws?.readyState)
    }
  }

  return {
    isConnected,
    isConnecting,
    send,
    subscribe,
  }
}
