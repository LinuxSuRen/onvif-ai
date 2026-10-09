<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import VideoPlayer from './components/VideoPlayer.vue'
import IntercomPanel from './components/IntercomPanel.vue'
import VoicePanel from './components/VoicePanel.vue'
import DeviceInfo from './components/DeviceInfo.vue'

const clockText = ref('--:--:--')
let clockTimer: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  const tick = () => {
    clockText.value = new Date().toLocaleTimeString('zh-CN', { hour12: false })
  }
  tick()
  clockTimer = setInterval(tick, 1000)
})

onUnmounted(() => {
  if (clockTimer) clearInterval(clockTimer)
})
</script>

<template>
  <div class="app-shell">
    <header class="app-header">
      <div class="app-header__brand">
        <div class="app-header__logo">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
            <path d="M23 7l-7 5 7 5V7z"/>
            <rect x="1" y="5" width="15" height="14" rx="2" ry="2"/>
          </svg>
        </div>
        <div class="app-header__titles">
          <h1 class="app-header__title">ONVIF AI</h1>
          <span class="app-header__subtitle">ONVIF 摄像头查看器</span>
        </div>
      </div>
      <div class="app-header__meta">
        <a class="app-header__github" href="https://github.com/LinuxSuRen/onvif-ai"
          target="_blank" rel="noopener noreferrer" aria-label="GitHub 开源仓库" title="GitHub 开源仓库">
          <svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
            <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"/>
          </svg>
        </a>
        <span class="app-header__time">{{ clockText }}</span>
      </div>
    </header>

    <main class="app-main">
      <section class="app-main__video">
        <VideoPlayer />
      </section>
      <!-- 核心流程：搜索设备 → 查看画面 → 语音对讲；AI 助手折叠置底 -->
      <aside class="app-main__sidebar">
        <IntercomPanel />
        <DeviceInfo />
        <VoicePanel />
      </aside>
    </main>
  </div>
</template>



<style scoped>
.app-shell {
  display: flex;
  flex-direction: column;
  height: 100vh;
  background: var(--color-bg-base);
}

.app-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-3) var(--space-6);
  background: var(--color-bg-panel);
  border-bottom: 1px solid var(--color-border-subtle);
  flex-shrink: 0;
}

.app-header__brand {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.app-header__logo {
  width: 32px;
  height: 32px;
  color: var(--color-accent-cyan);
  display: flex;
  align-items: center;
  justify-content: center;
}

.app-header__logo svg {
  width: 22px;
  height: 22px;
}

.app-header__titles {
  display: flex;
  flex-direction: column;
}

.app-header__title {
  font-family: var(--font-display);
  font-size: 0.9375rem;
  font-weight: 700;
  letter-spacing: 0.12em;
  color: var(--color-text-bright);
  line-height: 1.2;
  margin: 0;
}

.app-header__subtitle {
  font-family: var(--font-mono);
  font-size: 0.625rem;
  color: var(--color-text-dim);
  letter-spacing: 0.06em;
}

.app-header__meta {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

/* GitHub 仓库入口：与时钟同区的低调图标，悬停提亮 */
.app-header__github {
  display: flex;
  align-items: center;
  padding: var(--space-1);
  border-radius: var(--radius-sm);
  color: var(--color-text-dim);
  transition: color 0.15s;
}

.app-header__github:hover {
  color: var(--color-text-bright);
}

.app-header__github svg {
  width: 18px;
  height: 18px;
}

.app-header__time {
  font-family: var(--font-mono);
  font-size: 0.875rem;
  color: var(--color-text-primary);
  letter-spacing: 0.08em;
  padding: var(--space-1) var(--space-3);
  background: var(--color-bg-base);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-sm);
}

.app-main {
  display: grid;
  grid-template-columns: 1fr 380px;
  gap: var(--space-4);
  padding: var(--space-4);
  flex: 1;
  min-height: 0;
}

.app-main__video {
  min-height: 0;
  display: flex;
}

.app-main__video > * {
  flex: 1;
}

.app-main__sidebar {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  min-height: 0;
  overflow-y: auto;
}

/* ---- 移动端适配：窄屏单栏堆叠，视频优先 ---- */
@media (max-width: 960px) {
  /* 外壳不再锁死一屏高度：内容（视频 + 侧栏）随页面自然滚动 */
  .app-shell {
    height: auto;
    min-height: 100vh;
    min-height: 100dvh;
  }

  .app-main {
    grid-template-columns: 1fr;
    padding: var(--space-3);
    gap: var(--space-3);
  }

  /* grid 项默认 min-width:auto 会被内容撑宽，显式允许收缩 */
  .app-main__video {
    min-width: 0;
  }

  .app-main__sidebar {
    overflow-y: visible; /* 移动端跟随页面滚动，避免嵌套滚动 */
  }

  .app-header {
    padding: var(--space-2) var(--space-3);
  }

  .app-header__subtitle {
    display: none; /* 窄屏隐藏副标题，保留品牌与时间 */
  }
}
</style>
