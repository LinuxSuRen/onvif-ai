<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import VideoPlayer from './components/VideoPlayer.vue'
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
          <span class="app-header__subtitle">智能摄像头语音控制台</span>
        </div>
      </div>
      <div class="app-header__meta">
        <span class="app-header__time">{{ clockText }}</span>
      </div>
    </header>

    <main class="app-main">
      <section class="app-main__video">
        <VideoPlayer />
      </section>
      <aside class="app-main__sidebar">
        <VoicePanel />
        <DeviceInfo />
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
</style>
