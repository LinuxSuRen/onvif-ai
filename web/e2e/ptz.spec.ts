import { test, expect } from '@playwright/test'
import {
	openApp,
	dispatchCamera,
	sentMessages,
	sentPtzSequence,
	replyZoomStatus,
} from './helpers/mock-ws'

/**
 * PTZ 变焦控制端到端验证（协议层）：
 *  - 轻点（<300ms）：ptz_move → ptz_stop → 补发带 step 的完整步进
 *    （后端映射 ContinuousMove 全速 ±1.0 × 0.8s）
 *  - 按住：ptz_move 持续变焦，松开 ptz_stop，不补步进
 *  - 按能力渲染：仅 Zoom（手机）隐藏方向键，仅 PanTilt 隐藏变焦，
 *    旧后端未携带能力字段时回退全显示
 *  - 倍率显示：初始/松开后各查一次 GetStatus，按倍率或百分比展示
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

	test('变焦可见即查询一次倍率，回执驱动显示（2.5x → 50x）', async ({ page }) => {
		// 控制可见后 tile 立即发起一次 ptz_status 查询
		await expect
			.poll(() => sentMessages(page, 'ptz_status'))
			.toHaveLength(1)
		expect((await sentMessages(page, 'ptz_status'))[0].payload).toEqual({
			camera: 'cam-back',
		})

		// 有自定义倍率空间：真实倍率，<10x 一位小数
		await replyZoomStatus(page, { camera: 'cam-back', position: 0.02, ratio: 2.5 })
		await expect(page.locator('.cam-tile__zoom-ratio')).toHaveText('2.5x')

		// 轻点步进完成后（0.8s）再查一次，≥10x 取整数
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		await zoomIn.hover()
		await page.mouse.down()
		await page.mouse.up()
		await expect
			.poll(() => sentMessages(page, 'ptz_status'), { timeout: 5000 })
			.toHaveLength(2)
		await replyZoomStatus(page, { camera: 'cam-back', position: 0.5, ratio: 50.2 })
		await expect(page.locator('.cam-tile__zoom-ratio')).toHaveText('50x')
	})

	test('无自定义倍率空间时回退显示百分比', async ({ page }) => {
		await expect
			.poll(() => sentMessages(page, 'ptz_status'))
			.toHaveLength(1)
		await replyZoomStatus(page, { camera: 'cam-back', position: 0.0625 })
		await expect(page.locator('.cam-tile__zoom-ratio')).toHaveText('6%')
	})

	test('按住期间 500ms 节流查询，松开后再查一次收尾', async ({ page }) => {
		const zoomIn = page.locator('.cam-tile__ptz-btn--zoom-in')
		await expect(zoomIn).toBeVisible({ timeout: 10000 })
		// 等初始查询稳定，避免与按住节流计数交叠
		await expect
			.poll(() => sentMessages(page, 'ptz_status'))
			.toHaveLength(1)

		await zoomIn.hover()
		await page.mouse.down()
		// 按住 ~1.2s：500ms 节流下应再发起 1~2 次查询（不含初始那次）；
		// 计数必须在 mouse.up 之前取——松开查询在 pointerup 同步发出
		await page.waitForTimeout(1200)
		const duringHold = (await sentMessages(page, 'ptz_status')).length
		await page.mouse.up()
		if (duringHold < 2 || duringHold > 4) {
			throw new Error(`expected 1-2 throttled queries during 1.2s hold (+initial), got ${duringHold}`)
		}
		// 松开后立即补一次收尾查询
		await expect
			.poll(() => sentMessages(page, 'ptz_status'), { timeout: 2000 })
			.toHaveLength(duringHold + 1)
	})

	test('无变焦能力时不显示倍率', async ({ page }) => {
		await dispatchCamera(page, { panTilt: true, zoom: false })
		await expect(page.locator('.cam-tile__zoom-ratio')).toHaveCount(0)
	})
})
