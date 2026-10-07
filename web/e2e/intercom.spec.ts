import { test, expect, chromium, type Page } from '@playwright/test'

/**
 * 对讲（浏览器麦克风 → 摄像头扬声器）端到端验证：
 * 用页面内 Mock WebSocket 捕获上行消息并注入下行消息（device_state /
 * talkback_state），麦克风走 Chromium fake media stream，不依赖真实
 * 后端与摄像头即可验证按钮状态机与 WS 协议。
 */

const mockWebSocketScript = `
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

async function openApp(page: Page): Promise<void> {
  await page.addInitScript(mockWebSocketScript)
  await page.goto('/')
  await page.waitForFunction(() => (window as any).__wsDispatch !== undefined)
}

function dispatchTalkbackAvailable(page: Page, available: boolean, reason?: string): Promise<unknown> {
  return page.evaluate(
    ({ available, reason }) =>
      (window as any).__wsDispatch({
        type: 'device_state',
        payload: { talkback: reason ? { available, reason } : { available } },
      }),
    { available, reason },
  )
}

/** 带 fake 麦克风的浏览器上下文：getUserMedia 免交互返回合成音源。 */
async function openAppWithFakeMic(baseURL: string | undefined) {
  const browser = await chromium.launch({
    args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'],
  })
  const context = await browser.newContext({ permissions: ['microphone'] })
  const page = await context.newPage()
  await openApp(page)
  return { browser, page }
}

test.describe('对讲（talkback）', () => {
  test('按钮随回传通道状态切换可用态与原因文案', async ({ page }) => {
    await openApp(page)

    const btn = page.locator('.voice-panel__intercom-btn')
    await expect(btn).toBeVisible({ timeout: 10000 })
    // 尚未收到任何 device_state：未就绪且禁用
    await expect(btn).toBeDisabled()
    await expect(btn).toHaveText('对讲未就绪')

    // 设备无回传轨：明确提示不支持并禁用
    await dispatchTalkbackAvailable(page, false, 'no_backchannel')
    await expect(btn).toBeDisabled()
    await expect(btn).toHaveText('设备不支持对讲')

    // 回传通道连接失败
    await dispatchTalkbackAvailable(page, false, 'connect_failed')
    await expect(btn).toBeDisabled()
    await expect(btn).toHaveText('对讲通道连接失败')

    // 通道可用：启用，等待对讲
    await dispatchTalkbackAvailable(page, true)
    await expect(btn).toBeEnabled()
    await expect(btn).toHaveText('按住对讲')
  })

  test('会话被拒（busy）时停止采集并给出占用提示', async ({ baseURL }) => {
    const { browser, page } = await openAppWithFakeMic(baseURL)
    await dispatchTalkbackAvailable(page, true)

    const btn = page.locator('.voice-panel__intercom-btn')
    await expect(btn).toBeEnabled()

    await btn.dispatchEvent('mousedown')
    // 服务器回执拒绝：TTS 播报占用中
    await page.evaluate(() =>
      (window as any).__wsDispatch({
        type: 'talkback_state',
        payload: { active: false, reason: 'busy' },
      }),
    )

    await expect(btn).toHaveText('按住对讲')
    await expect(page.locator('.voice-panel__error').first()).toContainText('语音播报占用中')

    // 采集已被中止：不应出现任何音频分片
    await page.waitForTimeout(800)
    const types = await page.evaluate(() => (window as any).__wsSent.map((m: any) => m.type))
    expect(types).not.toContain('audio_in')
    // 会话被拒后无需（也不应）再发 talkback_stop
    expect(types).not.toContain('talkback_stop')

    await browser.close()
  })

  test('按住对讲：talkback_start → audio_in 分片 → talkback_stop', async ({ baseURL }) => {
    const { browser, page } = await openAppWithFakeMic(baseURL)
    await dispatchTalkbackAvailable(page, true)

    const btn = page.locator('.voice-panel__intercom-btn')
    await expect(btn).toBeEnabled()

    await btn.dispatchEvent('mousedown')
    await expect
      .poll(async () =>
        (await page.evaluate(() => (window as any).__wsSent.filter((m: any) => m.type === 'talkback_start'))).length,
      )
      .toBe(1)

    // 受理回执后按钮进入对讲中状态
    await page.evaluate(() =>
      (window as any).__wsDispatch({ type: 'talkback_state', payload: { active: true } }),
    )
    await expect(btn).toHaveText('对讲中 · 松开结束')

    // fake 麦克风持续出声：约每 100ms 一片 base64 PCM16 分片
    await expect
      .poll(
        async () => (await page.evaluate(() => (window as any).__wsSent.filter((m: any) => m.type === 'audio_in'))).length,
        { timeout: 8000 },
      )
      .toBeGreaterThan(2)

    // 分片可解码为偶数字节的 16k 单声道 PCM16
    const chunkOk = await page.evaluate(() => {
      const chunk = (window as any).__wsSent.find((m: any) => m.type === 'audio_in' && typeof m.data === 'string')
      if (!chunk) return false
      const bin = atob(chunk.data)
      return bin.length >= 1600 && bin.length % 2 === 0
    })
    expect(chunkOk).toBe(true)

    // 松开：本地先停采集，随后释放服务端会话
    await btn.dispatchEvent('mouseup')
    await expect
      .poll(async () =>
        (await page.evaluate(() => (window as any).__wsSent.filter((m: any) => m.type === 'talkback_stop'))).length,
      )
      .toBe(1)
    await expect(btn).toHaveText('按住对讲')

    await browser.close()
  })
})
