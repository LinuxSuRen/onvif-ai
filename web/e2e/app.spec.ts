import { test, expect } from '@playwright/test'

test.describe('ONVIF AI Voice Q&A App', () => {
	test('page loads with main components', async ({ page }) => {
		await page.goto('/')

		await expect(page.locator('text=ONVIF AI')).toBeVisible({ timeout: 10000 })

		await expect(page.locator('text=设备信息')).toBeVisible()
		await expect(page.locator('text=语音对话')).toBeVisible()
	})

	test('device info shows camera address', async ({ page }) => {
		await page.goto('/')

		await expect(page.locator('text=192.168.1.138')).toBeVisible({ timeout: 10000 })
	})

	test('video player shows connecting state', async ({ page }) => {
		await page.goto('/')

		const videoArea = page.locator('text=等待视频流').or(page.locator('text=Connecting'))
		await expect(videoArea.first()).toBeVisible({ timeout: 10000 })
	})

	test('voice panel has talk button and mode switch', async ({ page }) => {
		await page.goto('/')

		const talkBtn = page.locator('button, [role="button"]').filter({ hasText: /按住.*说话|Talk/i })
		await expect(talkBtn.first()).toBeVisible({ timeout: 10000 })
	})

	test('status shows idle initially', async ({ page }) => {
		await page.goto('/')

		const statusEl = page.locator('text=就绪').or(page.locator('text=idle')).or(page.locator('text=空闲'))
		await expect(statusEl.first()).toBeVisible({ timeout: 10000 })
	})
})
