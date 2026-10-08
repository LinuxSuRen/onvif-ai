import { test, expect } from '@playwright/test'
import { openApp, dispatchCamera, sentMessages } from './helpers/mock-ws'

/**
 * PTZ 变焦控制端到端验证（协议层）：
 * 按住 +/− 发送 ptz_move（zoom_in / zoom_out，后端映射 ContinuousMove
 * 的 zoom 轴），松开发送 ptz_stop；无 PTZ 能力时不渲染控制按钮。
 */
test.describe('PTZ 变焦控制', () => {
	test.beforeEach(async ({ page }) => {
		await openApp(page)
		await dispatchCamera(page, true)
	})

	test('按住放大/缩小发送 ptz_move，松开发送 ptz_stop', async ({ page }) => {
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		const zoomOut = page.locator('.cam-tile__ptz-btn--zoom-out')
		await expect(zoomIn).toBeVisible({ timeout: 10000 })
		await expect(zoomOut).toBeVisible()

		// 按住放大：立即发出 zoom_in
		await zoomIn.hover()
		await page.mouse.down()
		await expect
			.poll(() => sentMessages(page, 'ptz_move', 'zoom_in'))
			.toHaveLength(1)
		expect((await sentMessages(page, 'ptz_move', 'zoom_in'))[0].payload).toEqual({
			camera: 'cam-back',
			direction: 'zoom_in',
		})

		// 松开：发出 ptz_stop，携带当前路 token
		await page.mouse.up()
		await expect
			.poll(() => sentMessages(page, 'ptz_stop'))
			.toHaveLength(1)
		expect((await sentMessages(page, 'ptz_stop'))[0].payload).toEqual({ camera: 'cam-back' })

		// 缩小同理，方向为 zoom_out
		await zoomOut.hover()
		await page.mouse.down()
		await expect
			.poll(() => sentMessages(page, 'ptz_move', 'zoom_out'))
			.toHaveLength(1)
		await page.mouse.up()
		await expect
			.poll(() => sentMessages(page, 'ptz_stop'))
			.toHaveLength(2)
	})

	test('按住期间移出按钮松开仍发送 ptz_stop（指针捕获）', async ({ page }) => {
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		await expect(zoomIn).toBeVisible({ timeout: 10000 })

		await zoomIn.hover()
		await page.mouse.down()
		await expect
			.poll(() => sentMessages(page, 'ptz_move', 'zoom_in'))
			.toHaveLength(1)

		// 移出按钮再松开：pointerup 因捕获仍派发到按钮，Stop 必达
		await page.mouse.move(40, 40)
		await page.mouse.up()
		await expect
			.poll(() => sentMessages(page, 'ptz_stop'))
			.toHaveLength(1)
	})

	test('方向键仍为点按触发，松开不发送 ptz_stop', async ({ page }) => {
		const up = page.locator('.cam-tile__ptz-btn--up')
		await expect(up).toBeVisible({ timeout: 10000 })

		await up.hover()
		await page.mouse.down()
		await expect
			.poll(() => sentMessages(page, 'ptz_move', 'up'))
			.toHaveLength(1)
		await page.mouse.up()
		await page.waitForTimeout(200)
		expect(await sentMessages(page, 'ptz_stop')).toHaveLength(0)
	})

	test('无 PTZ 能力的摄像头不显示变焦按钮', async ({ page }) => {
		await dispatchCamera(page, false)
		await expect(page.locator('.cam-tile__ptz')).toHaveCount(0)
	})
})
