import { test, expect } from '@playwright/test'

/**
 * 默认界面按「ONVIF 摄像头查看器」定位组织：
 * 核心流程 = 设备搜索 + 画面查看 + 语音对讲；AI 面板默认收起。
 */
test.describe('ONVIF 摄像头查看器', () => {
	test('默认界面呈现核心功能：画面、设备管理、语音对讲', async ({ page }) => {
		await page.goto('/')

		await expect(page.locator('text=ONVIF AI')).toBeVisible({ timeout: 10000 })
		await expect(page.locator('.video-player').first()).toBeVisible()

		await expect(page.locator('text=设备管理')).toBeVisible()
		await expect(page.locator('text=语音对讲')).toBeVisible()

		// 对讲入口显著：常驻大按钮
		await expect(page.locator('.intercom-panel__btn')).toBeVisible()
	})

	test('AI 面板默认收起，点击标题展开', async ({ page }) => {
		await page.goto('/')

		const header = page.locator('.voice-panel__header', { hasText: 'AI 语音助手' })
		await expect(header).toBeVisible({ timeout: 10000 })

		// 收起状态：AI 输入源切换与问答按钮不可见
		await expect(page.locator('text=浏览器麦克风')).toHaveCount(0)
		await expect(page.locator('.voice-panel__talk-btn')).toHaveCount(0)

		await header.click()
		await expect(page.locator('text=浏览器麦克风')).toBeVisible()
		await expect(page.locator('.voice-panel__talk-btn')).toBeVisible()
	})

	test('窄屏（移动端）布局不破坏：对讲与设备搜索可用', async ({ page }) => {
		await page.setViewportSize({ width: 375, height: 812 })
		await page.goto('/')

		await expect(page.locator('.intercom-panel__btn')).toBeVisible({ timeout: 10000 })
		await expect(page.locator('text=搜索设备')).toBeVisible()

		// 单栏堆叠下无横向溢出
		const overflow = await page.evaluate(
			() => document.documentElement.scrollWidth - document.documentElement.clientWidth,
		)
		expect(overflow).toBeLessThanOrEqual(1)
	})
})
