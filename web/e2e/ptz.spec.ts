import { test, expect } from '@playwright/test'
import { openApp, dispatchCamera, sentMessages, sentPtzSequence } from './helpers/mock-ws'

/**
 * PTZ 变焦控制端到端验证（协议层）：
 *  - 轻点（<300ms）：ptz_move → ptz_stop → 补发带 step 的完整步进
 *    （后端映射 ContinuousMove 全速 ±1.0 × 0.8s）
 *  - 按住：ptz_move 持续变焦，松开 ptz_stop，不补步进
 *  - 按能力渲染：仅 Zoom（手机）隐藏方向键，仅 PanTilt 隐藏变焦，
 *    旧后端未携带能力字段时回退全显示
 */
test.describe('PTZ 变焦控制', () => {
	test.beforeEach(async ({ page }) => {
		await openApp(page)
		await dispatchCamera(page)
	})

	test('轻点变焦：move → stop → 补发完整步进（step）', async ({ page }) => {
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		await expect(zoomIn).toBeVisible({ timeout: 10000 })

		// 快速按下松开（远小于 300ms 阈值）
		await zoomIn.hover()
		await page.mouse.down()
		await page.mouse.up()

		await expect
			.poll(() => sentPtzSequence(page))
			.toEqual([
				{ type: 'ptz_move', direction: 'zoom_in', step: false, camera: 'cam-back' },
				{ type: 'ptz_stop', direction: undefined, step: false, camera: 'cam-back' },
				{ type: 'ptz_move', direction: 'zoom_in', step: true, camera: 'cam-back' },
			])
	})

	test('按住变焦：move → stop，不补步进', async ({ page }) => {
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		await expect(zoomIn).toBeVisible({ timeout: 10000 })

		await zoomIn.hover()
		await page.mouse.down()
		await expect
			.poll(() => sentMessages(page, 'ptz_move', 'zoom_in'))
			.toHaveLength(1)
		// 按住超过轻点阈值再松开
		await page.waitForTimeout(400)
		await page.mouse.up()

		await expect
			.poll(() => sentPtzSequence(page))
			.toEqual([
				{ type: 'ptz_move', direction: 'zoom_in', step: false, camera: 'cam-back' },
				{ type: 'ptz_stop', direction: undefined, step: false, camera: 'cam-back' },
			])
	})

	test('按住期间移出按钮松开仍发送 ptz_stop（指针捕获）', async ({ page }) => {
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		await expect(zoomIn).toBeVisible({ timeout: 10000 })

		await zoomIn.hover()
		await page.mouse.down()
		await expect
			.poll(() => sentMessages(page, 'ptz_move', 'zoom_in'))
			.toHaveLength(1)

		// 移出按钮并按住超阈值再松开：pointerup 因捕获仍派发到按钮
		await page.mouse.move(40, 40)
		await page.waitForTimeout(400)
		await page.mouse.up()
		await expect
			.poll(() => sentMessages(page, 'ptz_stop'))
			.toHaveLength(1)
	})

	test('缩小方向同样支持轻点步进', async ({ page }) => {
		const zoomOut = page.locator('.cam-tile__ptz-btn--zoom-out')
		await expect(zoomOut).toBeVisible({ timeout: 10000 })

		await zoomOut.hover()
		await page.mouse.down()
		await page.mouse.up()

		await expect
			.poll(() => sentPtzSequence(page))
			.toEqual([
				{ type: 'ptz_move', direction: 'zoom_out', step: false, camera: 'cam-back' },
				{ type: 'ptz_stop', direction: undefined, step: false, camera: 'cam-back' },
				{ type: 'ptz_move', direction: 'zoom_out', step: true, camera: 'cam-back' },
			])
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

	test('无 PTZ 能力的摄像头不显示 PTZ 控制', async ({ page }) => {
		await dispatchCamera(page, { ptz: false })
		await expect(page.locator('.cam-tile__ptz')).toHaveCount(0)
	})

	test('仅 Zoom 能力（手机）：隐藏方向键只留变焦', async ({ page }) => {
		await dispatchCamera(page, { panTilt: false, zoom: true })
		await expect(page.locator('.cam-tile__ptz-btn--up')).toHaveCount(0)
		await expect(page.locator('.cam-tile__ptz-btn--down')).toHaveCount(0)
		await expect(page.locator('.cam-tile__ptz-btn--left')).toHaveCount(0)
		await expect(page.locator('.cam-tile__ptz-btn--right')).toHaveCount(0)
		await expect(page.locator('.cam-tile__ptz-btn--zoom-in')).toBeVisible({ timeout: 10000 })
		await expect(page.locator('.cam-tile__ptz-btn--zoom-out')).toBeVisible()
	})

	test('仅 PanTilt 能力：隐藏变焦只留方向键', async ({ page }) => {
		await dispatchCamera(page, { panTilt: true, zoom: false })
		await expect(page.locator('.cam-tile__ptz-zoom')).toHaveCount(0)
		await expect(page.locator('.cam-tile__ptz-btn--up')).toBeVisible({ timeout: 10000 })
		await expect(page.locator('.cam-tile__ptz-btn--down')).toBeVisible()
	})

	test('旧后端未携带能力字段：回退全显示', async ({ page }) => {
		await dispatchCamera(page, {})
		await expect(page.locator('.cam-tile__ptz-btn--up')).toBeVisible({ timeout: 10000 })
		await expect(page.locator('.cam-tile__ptz-btn--zoom-in')).toBeVisible()
		await expect(page.locator('.cam-tile__ptz-btn--zoom-out')).toBeVisible()
	})
})
