<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { useWebSocket, type DeviceEventData } from '../composables/useWebSocket'

/** 事件徽标状态 */
interface Badge {
  key: string
  label: string
  level: 'ok' | 'warn' | 'error'
  detail: string
  ts: number
}

/** 历史记录条目 */
interface HistoryItem {
  topic: string
  label: string
  detail: string
  level: 'info' | 'warn' | 'error' | 'motion'
  time: string
}

const badges = ref<Map<string, Badge>>(new Map())
const history = ref<HistoryItem[]>([])
const HISTORY_MAX = 30

/** topic → 人可读标签 */
function topicLabel(topic: string): string {
  if (topic.includes('SignalLoss')) return '信号'
  if (topic.includes('CellMotion/Motion')) return '移动'
  if (topic.includes('AuthFailure')) return '安全'
  if (topic.includes('Battery')) return '电池'
  if (topic.includes('Lifecycle')) return '进程'
  if (topic.includes('Config/Changed')) return '配置'
  return topic.split('/').pop() || topic
}

/** topic → 严重级别 */
function topicLevel(topic: string, data?: Record<string, string>): 'ok' | 'warn' | 'error' | 'info' | 'motion' {
  if (topic.includes('SignalLoss')) return data?.State === 'true' ? 'error' : 'ok'
  if (topic.includes('AuthFailure')) return 'error'
  if (topic.includes('Battery')) {
    const level = parseInt(data?.Level || '100')
    if (level <= 10) return 'error'
    if (level <= 20) return 'warn'
    return 'ok'
  }
  if (topic.includes('CellMotion/Motion')) return 'motion'
  return 'info'
}

function formatTime(_utc?: string): string {
  const now = new Date()
  return now.toLocaleTimeString('zh-CN', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

function handleEvent(ev: DeviceEventData) {
  const label = topicLabel(ev.topic)
  const level = topicLevel(ev.topic, ev.data)
  const detail = Object.entries(ev.data || {})
    .map(([k, v]) => `${k}=${v}`)
    .join(' ')

  // 更新徽标（SignalLoss/Battery/AuthFailure 有持续状态语义）
  if (ev.topic.includes('SignalLoss') || ev.topic.includes('Battery') || ev.topic.includes('AuthFailure')) {
    badges.value.set(label, {
      key: label,
      label,
      level: level === 'ok' ? 'ok' : level === 'error' ? 'error' : 'warn',
      detail,
      ts: Date.now(),
    })
    // 信号恢复时清除错误徽标
    if (ev.topic.includes('SignalLoss') && ev.data?.State === 'false') {
      badges.value.delete(label)
    }
  }

  // 添加历史记录
  history.value.unshift({
    topic: ev.topic,
    label,
    detail,
    level: level === 'ok' ? 'info' : level,
    time: formatTime(ev.time),
  })
  if (history.value.length > HISTORY_MAX) {
    history.value = history.value.slice(0, HISTORY_MAX)
  }
}

const activeBadges = computed(() => Array.from(badges.value.values()))
const hasEvents = computed(() => history.value.length > 0)

let unsub: (() => void) | null = null

onMounted(() => {
  const { subscribe } = useWebSocket(`/ws`)
  unsub = subscribe(['device_event'], (msg) => {
    if (msg.payload) {
      handleEvent(msg.payload as unknown as DeviceEventData)
    }
  })
})

onUnmounted(() => {
  if (unsub) unsub()
})
</script>

<template>
  <div class="event-panel">
    <div class="event-panel__header">
      <svg class="event-panel__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <path d="M22 12h-4l-3 9L9 3l-3 9H2"/>
      </svg>
      <span class="event-panel__title">设备事件</span>
      <span v-if="activeBadges.length > 0" class="event-panel__badge-count">{{ activeBadges.length }}</span>
    </div>

    <!-- 活动告警徽标 -->
    <div v-if="activeBadges.length > 0" class="event-panel__badges">
      <div
        v-for="b in activeBadges"
        :key="b.key"
        class="event-badge"
        :class="`event-badge--${b.level}`"
        :title="b.detail"
      >
        <span class="event-badge__label">{{ b.label }}</span>
        <span class="event-badge__detail">{{ b.detail.substring(0, 20) }}</span>
      </div>
    </div>

    <!-- 事件历史 -->
    <div class="event-panel__history" v-if="hasEvents">
      <TransitionGroup name="event-fade" tag="div" class="event-list">
        <div
          v-for="(item, i) in history"
          :key="`${i}-${item.time}`"
          class="event-item"
          :class="`event-item--${item.level}`"
        >
          <span class="event-item__time">{{ item.time }}</span>
          <span class="event-item__label">{{ item.label }}</span>
          <span class="event-item__detail">{{ item.detail.substring(0, 40) }}</span>
        </div>
      </TransitionGroup>
    </div>
    <div v-else class="event-panel__empty">
      等待设备事件…
    </div>
  </div>
</template>

<style scoped>
.event-panel {
  background: var(--color-bg-panel);
  border-radius: var(--radius-md);
  border: 1px solid var(--color-border-subtle);
  padding: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  max-height: 280px;
}

.event-panel__header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text-secondary);
}

.event-panel__icon {
  width: 16px;
  height: 16px;
}

.event-panel__badge-count {
  background: var(--color-accent);
  color: white;
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 8px;
}

.event-panel__badges {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-1);
}

.event-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px;
  border-radius: var(--radius-sm);
  font-size: 11px;
  font-weight: 500;
}

.event-badge--error {
  background: rgba(239, 68, 68, 0.15);
  color: #ef4444;
  border: 1px solid rgba(239, 68, 68, 0.3);
}

.event-badge--warn {
  background: rgba(245, 158, 11, 0.15);
  color: #f59e0b;
  border: 1px solid rgba(245, 158, 11, 0.3);
}

.event-badge--ok {
  background: rgba(34, 197, 94, 0.15);
  color: #22c55e;
  border: 1px solid rgba(34, 197, 94, 0.3);
}

.event-badge__detail {
  font-size: 10px;
  opacity: 0.7;
}

.event-panel__history {
  overflow-y: auto;
  flex: 1;
  min-height: 0;
}

.event-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.event-item {
  display: flex;
  align-items: baseline;
  gap: 6px;
  font-size: 11px;
  padding: 3px 6px;
  border-radius: var(--radius-xs);
}

.event-item--error { color: #ef4444; background: rgba(239, 68, 68, 0.05); }
.event-item--warn { color: #f59e0b; background: rgba(245, 158, 11, 0.05); }
.event-item--motion { color: #8b5cf6; background: rgba(139, 92, 246, 0.05); }
.event-item--info { color: var(--color-text-secondary); }

.event-item__time {
  font-family: monospace;
  font-size: 10px;
  opacity: 0.5;
  flex-shrink: 0;
}

.event-item__label {
  font-weight: 600;
  flex-shrink: 0;
}

.event-item__detail {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  opacity: 0.7;
}

.event-panel__empty {
  text-align: center;
  font-size: 11px;
  color: var(--color-text-tertiary);
  padding: var(--space-3);
}

.event-fade-enter-active {
  transition: opacity 0.3s, transform 0.3s;
}

.event-fade-enter-from {
  opacity: 0;
  transform: translateY(-6px);
}
</style>
