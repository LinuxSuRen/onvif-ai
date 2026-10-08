import type { Page } from '@playwright/test'

/**
 * 页面内 Mock WebSocket：捕获上行消息、注入下行消息（device_state 等），
 * 供不依赖真实后端的协议级 e2e 复用（同 intercom.spec.ts 的做法）。
 */
export const mockWebSocketScript = `
(() => {
  const sent = []
  let instance = null
  class MockWebSocket {
    static CONNECTING = 0
    static OPEN = 1
    static CLOSING = 2
    static CLOSED = 3
    constructor(url) {
      this.url = url
      this.readyState = MockWebSocket.CONNECTING
      instance = this
      setTimeout(() => {
        this.readyState = MockWebSocket.OPEN
        if (this.onopen) this.onopen()
      }, 0)
    }
    send(data) {
      sent.push(JSON.parse(data))
    }
    close() {
      if (this.readyState === MockWebSocket.CLOSED) return
      this.readyState = MockWebSocket.CLOSED
      if (this.onclose) this.onclose()
    }
  }
  window.WebSocket = MockWebSocket
  window.__wsSent = sent
  window.__wsDispatch = (msg) => {
    if (instance && instance.onmessage) {
      instance.onmessage({ data: JSON.stringify(msg) })
    }
  }
})()
`

export async function openApp(page: Page): Promise<void> {
	await page.addInitScript(mockWebSocketScript)
	await page.goto('/')
	await page.waitForFunction(() => (window as any).__wsDispatch !== undefined)
}

/** 注入一路摄像头的 device_state。
 *  opts.ptz 控制该路是否支持云台；panTilt/zoom 为 undefined 时字段
 *  省略（模拟旧后端，前端应回退全显示）。 */
export async function dispatchCamera(
	page: Page,
	opts: { ptz?: boolean; panTilt?: boolean; zoom?: boolean } = {},
): Promise<void> {
	const { ptz = true, panTilt, zoom } = opts
	await page.evaluate(
		({ ptz, panTilt, zoom }) =>
			(window as any).__wsDispatch({
				type: 'device_state',
				payload: {
					cameras: [
						Object.assign(
							{ token: 'cam-back', name: '后摄', ptz_supported: ptz, streaming: true },
							panTilt === undefined ? {} : { ptz_pan_tilt: panTilt },
							zoom === undefined ? {} : { ptz_zoom: zoom },
						),
					],
				},
			}),
		{ ptz, panTilt, zoom },
	)
}

/** 取指定类型 + 方向的已发送消息列表。 */
export async function sentMessages(page: Page, type: string, direction?: string): Promise<any[]> {
	return page.evaluate(
		({ type, direction }) =>
			(window as any).__wsSent.filter(
				(m: any) => m.type === type && (direction === undefined || m.payload?.direction === direction),
			),
		{ type, direction },
	)
}

/** 取 PTZ 相关已发送消息的有序摘要（type/direction/step），验证时序用。 */
export async function sentPtzSequence(page: Page): Promise<any[]> {
	return page.evaluate(() =>
		(window as any).__wsSent
			.filter((m: any) => m.type === 'ptz_move' || m.type === 'ptz_stop')
			.map((m: any) => ({
				type: m.type,
				direction: m.payload?.direction,
				step: m.payload?.step === true,
				camera: m.payload?.camera,
			})),
	)
}
