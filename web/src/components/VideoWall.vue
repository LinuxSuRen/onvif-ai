<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, provide } from 'vue'
import { useWebSocket, type WsMessage, type WallStateData } from '../composables/useWebSocket'
import {
  refreshDiscovered,
  useDiscoveredDevices,
  type DiscoveredDevice,
} from '../composables/useDiscoveredDevices'
import CameraTile from './CameraTile.vue'

/**
 * 视频墙：多台 ONVIF 相机同开（#39，模式 A——内置 WS 帧转发）。
 *
 * - 设备来源：手动添加（地址可从发现列表选择），凭证存浏览器
 *   localStorage、每次上墙请求携带，后端不落盘
 * - 每台设备上墙只取子码流一路（后端选 profile 末位），tile 即设备
 * - 带宽治理：tile 滚出视口自动 view_control stop（IntersectionObserver），
 *   滚回自动恢复；总路数上限由后端 MAX_WALL_STREAMS 控制
 * - 点击 tile 跳转查看器并以该设备聚焦（凭证从 localStorage 取）
 */

interface WallCam {
  token: string
  name: string
  ptz: boolean
  streaming: boolean
  snapshot: boolean
  mjpeg: boolean
  width: number
  height: number
}
interface WallDeviceView {
  address: string
  state: string
  cameras: WallCam[]
}

const CREDS_KEY = 'onvif-ai-wall-creds'
const LAYOUT_KEY = 'onvif-ai-wall-layout'

const devices = ref<WallDeviceView[]>([])
const layout = ref<number>(
  Number(localStorage.getItem(LAYOUT_KEY)) > 0 ? Number(localStorage.getItem(LAYOUT_KEY)) : 4,
)
const adding = ref(false)
const addForm = ref({ username: '', password: '' })
// 发现设备走共享缓存（sessionStorage）：查看器搜到的设备直接可选
const discoveredRaw = useDiscoveredDevices()
const selectedAddress = ref('')
const loadingDiscovery = ref(false)
const addError = ref('')
const busy = ref(false)

// ---- profile 选择：选定设备后拉取画面列表（纯 SOAP 查询，不建会话）----
interface ProfileItem {
  token: string
  name: string
  ptz: boolean
}
const profiles = ref<ProfileItem[]>([])
const selectedProfile = ref('')
const loadingProfiles = ref(false)
const profileError = ref('')

const discovered = computed(() =>
  discoveredRaw.value.map((d: DiscoveredDevice) => ({
    address: d.address,
    name: nameFromScopes(d.scopes),
    label: hostOf(d.address),
  })),
)

const { isConnected, send, subscribe } = useWebSocket('/ws')

// ---- tile 注册与帧路由（dev+'/'+token 复合键，与 CameraTile 约定一致）----
interface TileAPI {
  feedFMP4: (kind: string, codec: string, data: string, serverTs?: number, key?: boolean) => void
  feedJpeg: (data: string) => void
}
const tiles = new Map<string, TileAPI>()
provide('cameraTileRegistry', {
  register: (routeKey: string, api: TileAPI) => tiles.set(routeKey, api),
  unregister: (routeKey: string, api: TileAPI) => {
    if (tiles.get(routeKey) === api) tiles.delete(routeKey)
  },
})

function handleMessage(msg: WsMessage) {
  if ((msg.type === 'video_fmp4' || msg.type === 'video_jpeg') && msg.data) {
    const tile = tiles.get(`${msg.dev}/${msg.cam}`)
    if (tile) {
      if (msg.type === 'video_fmp4') {
        tile.feedFMP4(msg.kind || 'frag', msg.codec || '', msg.data, msg.ts, !!msg.key)
      } else {
        tile.feedJpeg(msg.data)
      }
    }
    return
  }
  if (msg.type === 'wall_state' && msg.payload) {
    applyWallState(msg.payload as unknown as WallStateData)
    return
  }
  if (msg.type === 'error' && msg.text) {
    addError.value = msg.text
  }
}
subscribe(['video_fmp4', 'video_jpeg', 'wall_state', 'error'], handleMessage)

function applyWallState(data: WallStateData) {
  devices.value = (data.devices || []).map((d) => ({
    address: d.address,
    state: d.state || 'connecting',
    cameras: (d.cameras || []).map((c) => ({
      token: c.token,
      name: c.name || c.token,
      ptz: false,
      streaming: !!c.streaming,
      snapshot: !!c.snapshot,
      mjpeg: !!c.mjpeg,
      width: c.width || 0,
      height: c.height || 0,
    })),
  }))
}

// ---- 凭证管理（浏览器侧，后端不落盘）----
function loadCreds(): Record<string, { username: string; password: string }> {
  try {
    return JSON.parse(localStorage.getItem(CREDS_KEY) || '{}')
  } catch {
    return {}
  }
}
function saveCred(address: string, username: string, password: string) {
  const creds = loadCreds()
  creds[address] = { username, password }
  localStorage.setItem(CREDS_KEY, JSON.stringify(creds))
}

// ---- 添加 / 移除设备 ----
async function loadWall() {
  try {
    const resp = await fetch('/api/wall/devices')
    if (resp.ok) {
      const data = (await resp.json()) as WallStateData
      applyWallState(data)
    }
  } catch {
    /* 初始快照失败时等待 wall_state 广播 */
  }
}

function hostOf(address: string): string {
  return address.replace(/^https?:\/\//, '').split(/[/:]/)[0] || address
}

// 设备名藏在 scopes 里（onvif://www.onvif.org/name/友好名），有则展示
function nameFromScopes(scopes: unknown): string {
  if (!Array.isArray(scopes)) return ''
  for (const s of scopes) {
    if (typeof s === 'string' && s.startsWith('onvif://www.onvif.org/name/')) {
      try {
        return decodeURIComponent(s.slice('onvif://www.onvif.org/name/'.length))
      } catch {
        return s.slice('onvif://www.onvif.org/name/'.length)
      }
    }
  }
  return ''
}

async function refreshDiscovery() {
  loadingDiscovery.value = true
  try {
    await refreshDiscovered()
  } finally {
    loadingDiscovery.value = false
  }
}

function onWall(address: string): boolean {
  const host = hostOf(address)
  return devices.value.some((d) => d.address === address || hostOf(d.address) === host)
}

function pickDevice(address: string) {
  if (onWall(address)) return
  selectedAddress.value = address
  const cred = loadCreds()[address]
  if (cred) {
    addForm.value.username = cred.username
    addForm.value.password = cred.password
  }
  // 选定设备即拉取画面列表（有存储凭证时直接用；认证失败可填好
  // 凭证后点「重新获取」）
  fetchProfiles()
}

async function fetchProfiles() {
  const address = selectedAddress.value
  if (!address) return
  loadingProfiles.value = true
  profileError.value = ''
  profiles.value = []
  selectedProfile.value = ''
  try {
    const q = new URLSearchParams({
      address,
      username: addForm.value.username,
      password: addForm.value.password,
    })
    const resp = await fetch(`/api/camera/profiles?${q}`)
    const data = await resp.json()
    if (!resp.ok) {
      throw new Error(data.error || `获取画面列表失败（HTTP ${resp.status}）`)
    }
    profiles.value = (data.profiles || []).map((p: { token: string; name: string; ptz: boolean }) => ({
      token: p.token,
      name: p.name || p.token,
      ptz: !!p.ptz,
    }))
    if (profiles.value.length === 1) {
      // 单画面设备无需选择
      selectedProfile.value = profiles.value[0].token
    }
  } catch (e) {
    profileError.value = e instanceof Error ? e.message : '无法获取画面列表，请检查凭证后重试'
  } finally {
    loadingProfiles.value = false
  }
}

function selectedLabel(): string {
  const d = discovered.value.find((x) => x.address === selectedAddress.value)
  return d ? (d.name ? `${d.name}（${d.label}）` : d.label) : selectedAddress.value
}

async function openAddDialog() {
  adding.value = true
  addError.value = ''
  addForm.value = { username: '', password: '' }
  selectedAddress.value = ''
  profiles.value = []
  selectedProfile.value = ''
  profileError.value = ''
  refreshDiscovery()
}

async function submitAdd() {
  const address = selectedAddress.value.trim()
  if (!address) {
    addError.value = '请先在列表中选择一台设备'
    return
  }
  if (profiles.value.length > 1 && !selectedProfile.value) {
    addError.value = '请选择要上墙的画面（profile）'
    return
  }
  busy.value = true
  addError.value = ''
  try {
    const resp = await fetch('/api/wall/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        address,
        username: addForm.value.username,
        password: addForm.value.password,
        profile: selectedProfile.value,
      }),
    })
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}))
      throw new Error(data.error || `连接失败（HTTP ${resp.status}）`)
    }
    saveCred(address, addForm.value.username, addForm.value.password)
    adding.value = false
  } catch (e) {
    addError.value = e instanceof Error ? e.message : '无法连接后端服务，请确认服务已启动'
  } finally {
    busy.value = false
  }
}

async function removeDevice(address: string) {
  try {
    await fetch('/api/wall/disconnect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ address }),
    })
  } catch {
    /* 界面随 wall_state 广播自然更新 */
  }
}

const emit = defineEmits<{
  // 点击 tile 主体：切换到查看器并聚焦该设备（凭证从 localStorage 取）
  (e: 'focus', address: string): void
}>()

function focusDevice(address: string) {
  emit('focus', address)
}

// ---- 可见性驱动的按路订阅 ----
const visibleTiles = new Set<string>()
let observer: IntersectionObserver | null = null
const resetKey = ref(0)

function tileRouteKey(d: WallDeviceView): string | null {
  const cam = d.cameras[0]
  return cam ? `${d.address}/${cam.token}` : null
}

function subscribeTile(routeKey: string) {
  const [dev, ...rest] = routeKey.split('/')
  send({ type: 'view_control', payload: { action: 'start', dev, cam: rest.join('/') } })
}
function unsubscribeTile(routeKey: string) {
  const [dev, ...rest] = routeKey.split('/')
  send({ type: 'view_control', payload: { action: 'stop', dev, cam: rest.join('/') } })
}

function observeTiles() {
  if (!observer) {
    observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const routeKey = (entry.target as HTMLElement).dataset.routeKey || ''
          if (!routeKey) continue
          if (entry.isIntersecting) {
            if (!visibleTiles.has(routeKey)) {
              visibleTiles.add(routeKey)
              subscribeTile(routeKey)
            }
          } else if (visibleTiles.has(routeKey)) {
            visibleTiles.delete(routeKey)
            unsubscribeTile(routeKey)
          }
        }
      },
      { threshold: 0.05 },
    )
  }
  for (const el of document.querySelectorAll<HTMLElement>('[data-route-key]')) {
    observer.observe(el)
  }
}

function resubscribeAll() {
  for (const key of Array.from(visibleTiles)) {
    subscribeTile(key)
  }
}

watch(isConnected, (connected) => {
  if (connected) {
    resetKey.value++
    resubscribeAll()
    loadWall()
  }
})

watch(devices, () => {
  // 设备集合变化后重新观察 tile 容器（新 tile 需要进入观察）
  requestAnimationFrame(observeTiles)
})

watch(layout, (l) => localStorage.setItem(LAYOUT_KEY, String(l)))

function setLayout(l: number) {
  layout.value = l
  requestAnimationFrame(observeTiles)
}

onMounted(() => {
  loadWall()
  observeTiles()
})

onUnmounted(() => {
  observer?.disconnect()
  observer = null
  // 退订本页面持有的全部画面
  for (const key of Array.from(visibleTiles)) {
    unsubscribeTile(key)
  }
  visibleTiles.clear()
})

const gridTemplate = computed(() => {
  if (layout.value === 1) return '1fr'
  if (layout.value === 4) return 'repeat(2, 1fr)'
  if (layout.value === 9) return 'repeat(3, 1fr)'
  return 'repeat(4, 1fr)'
})

function deviceLabel(d: WallDeviceView): string {
  const host = d.address.replace(/^https?:\/\//, '').split(/[/:]/)[0]
  return host || d.address
}
</script>

<template>
  <div class="wall">
    <div class="wall__header">
      <div class="wall__title">
        <span class="wall__indicator" :class="devices.length ? 'wall__indicator--on' : ''"></span>
        <span class="wall__label">视频墙</span>
        <span class="wall__count">{{ devices.length }} 台设备</span>
      </div>
      <div class="wall__layouts" role="tablist">
        <button
          v-for="l in [1, 4, 9, 16]"
          :key="l"
          class="wall__layout-btn"
          :class="{ 'wall__layout-btn--active': layout === l }"
          :title="`${l} 宫格`"
          @click="setLayout(l)"
        >{{ l }}</button>
      </div>
      <button class="wall__add" type="button" @click="openAddDialog">＋ 添加设备</button>
    </div>

    <div class="wall__grid" :style="{ gridTemplateColumns: gridTemplate }">
      <div
        v-for="d in devices"
        :key="d.address"
        class="wall__cell"
        :data-route-key="tileRouteKey(d) || undefined"
      >
        <div class="wall__cell-bar">
          <span class="wall__cell-name" :title="d.address">{{ deviceLabel(d) }}</span>
          <span v-if="d.state === 'connecting'" class="wall__cell-state wall__cell-state--connecting">连接中</span>
          <button class="wall__cell-remove" title="下墙" @click.stop="removeDevice(d.address)">×</button>
        </div>
        <div class="wall__cell-body" @click="focusDevice(d.address)">
          <CameraTile
            v-if="d.cameras.length"
            :dev="d.address"
            :cam="d.cameras[0]"
            :active="layout === 1"
            :show-label="false"
            :clock-offset="null"
            :reset-key="resetKey"
          />
          <div v-else class="wall__cell-placeholder">
            <span>{{ d.state === 'connecting' ? '正在连接设备…' : '等待画面…' }}</span>
          </div>
        </div>
      </div>

      <div v-if="!devices.length" class="wall__empty">
        <span class="wall__empty-icon">▦</span>
        <span class="wall__empty-text">尚未添加设备，点击右上角「添加设备」开始</span>
      </div>
    </div>

    <div v-if="adding" class="wall__dialog-mask" @click.self="adding = false">
      <div class="wall__dialog" role="dialog" aria-label="添加设备到视频墙">
        <h2 class="wall__dialog-title">添加设备到视频墙</h2>

        <div class="wall__pick">
          <div class="wall__pick-head">
            <span>发现的设备</span>
            <button
              class="wall__refresh"
              type="button"
              :disabled="loadingDiscovery"
              @click="refreshDiscovery"
            >{{ loadingDiscovery ? '探测中…' : '刷新' }}</button>
          </div>
          <div class="wall__device-list">
            <button
              v-for="d in discovered"
              :key="d.address"
              class="wall__device-item"
              :class="{
                'wall__device-item--selected': selectedAddress === d.address,
                'wall__device-item--onwall': onWall(d.address),
              }"
              type="button"
              :disabled="onWall(d.address)"
              :title="d.address"
              @click="pickDevice(d.address)"
            >
              <span class="wall__device-name">{{ d.name || d.label }}</span>
              <span class="wall__device-host">{{ d.label }}</span>
              <span v-if="onWall(d.address)" class="wall__device-badge">已在墙上</span>
            </button>
            <div v-if="!discovered.length && !loadingDiscovery" class="wall__device-empty">
              局域网内未发现 ONVIF 设备，可点击「刷新」重新探测
            </div>
            <div v-else-if="loadingDiscovery && !discovered.length" class="wall__device-empty">
              正在探测局域网设备…
            </div>
          </div>
          <div v-if="selectedAddress" class="wall__picked">已选：{{ selectedLabel() }}</div>
        </div>

        <label class="wall__field">
          <span>用户名</span>
          <input v-model="addForm.username" autocomplete="off" placeholder="匿名设备可留空" />
        </label>
        <label class="wall__field">
          <span>密码</span>
          <input v-model="addForm.password" type="password" autocomplete="off" placeholder="匿名设备可留空" />
        </label>

        <div v-if="selectedAddress" class="wall__pick">
          <div class="wall__pick-head">
            <span>上墙画面</span>
            <button
              class="wall__refresh"
              type="button"
              :disabled="loadingProfiles"
              @click="fetchProfiles"
            >{{ loadingProfiles ? '获取中…' : '重新获取' }}</button>
          </div>
          <div v-if="loadingProfiles" class="wall__device-empty">正在获取画面列表…</div>
          <div v-else-if="profileError" class="wall__device-empty">{{ profileError }}</div>
          <div v-else class="wall__device-list">
            <button
              v-for="p in profiles"
              :key="p.token"
              class="wall__device-item"
              :class="{ 'wall__device-item--selected': selectedProfile === p.token }"
              type="button"
              :title="p.token"
              @click="selectedProfile = p.token"
            >
              <span class="wall__device-name">{{ p.name }}</span>
              <span class="wall__device-host">{{ p.ptz ? '支持云台' : '' }}</span>
            </button>
            <div v-if="!profiles.length" class="wall__device-empty">填写凭证后点击「重新获取」拉取画面列表</div>
          </div>
        </div>
        <p class="wall__dialog-hint">凭证仅保存在本浏览器（localStorage），每次连接时带给后端。</p>
        <div v-if="addError" class="wall__dialog-error">{{ addError }}</div>
        <div class="wall__dialog-actions">
          <button class="wall__btn" type="button" @click="adding = false">取消</button>
          <button class="wall__btn wall__btn--primary" type="button" :disabled="busy" @click="submitAdd">
            {{ busy ? '连接中…' : '上墙' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.wall {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-height: 0;
}

.wall__header {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.wall__title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.wall__indicator {
  width: 8px;
  height: 8px;
  border-radius: var(--radius-full);
  background: var(--color-text-muted);
}

.wall__indicator--on {
  background: var(--color-accent-green);
  box-shadow: 0 0 6px var(--color-accent-green);
}

.wall__label {
  font-family: var(--font-display);
  font-size: 0.75rem;
  font-weight: 500;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--color-text-secondary);
}

.wall__count {
  font-family: var(--font-mono);
  font-size: 0.6875rem;
  color: var(--color-text-secondary);
  padding: 2px var(--space-2);
  border-radius: var(--radius-sm);
  background: var(--color-bg-hover);
}

.wall__layouts {
  display: flex;
  gap: 2px;
  padding: 2px;
  border-radius: 6px;
  background: var(--color-bg-hover);
}

.wall__layout-btn {
  border: none;
  background: transparent;
  color: var(--color-text-secondary);
  font-family: var(--font-mono);
  font-size: 0.72rem;
  min-width: 30px;
  padding: 4px 8px;
  border-radius: 4px;
  cursor: pointer;
  transition: all 0.15s;
}

.wall__layout-btn--active {
  background: rgba(0, 229, 160, 0.85);
  color: #04110c;
}

.wall__add {
  margin-left: auto;
  border: 1px solid var(--color-border-default);
  background: var(--color-bg-hover);
  color: var(--color-text-secondary);
  font-size: 0.78rem;
  padding: 5px 14px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: all 0.15s;
}

.wall__add:hover {
  color: var(--color-text-bright);
  border-color: var(--color-accent-green);
}

.wall__grid {
  display: grid;
  grid-auto-rows: 1fr;
  gap: var(--space-2);
  min-height: 60vh;
}

.wall__cell {
  display: flex;
  flex-direction: column;
  background: #020408;
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-sm);
  overflow: hidden;
  min-height: 120px;
}

.wall__cell-bar {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 3px 8px;
  background: rgba(0, 0, 0, 0.5);
  font-size: 0.7rem;
}

.wall__cell-name {
  font-family: var(--font-mono);
  color: rgba(255, 255, 255, 0.85);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.wall__cell-state {
  font-size: 0.65rem;
  color: var(--color-accent-amber);
}

.wall__cell-remove {
  margin-left: auto;
  border: none;
  background: transparent;
  color: rgba(255, 255, 255, 0.5);
  font-size: 0.9rem;
  line-height: 1;
  cursor: pointer;
  padding: 0 4px;
}

.wall__cell-remove:hover {
  color: var(--color-accent-red);
}

.wall__cell-body {
  flex: 1;
  min-height: 0;
  cursor: pointer;
}

.wall__cell-body :deep(.cam-tile) {
  height: 100%;
}

.wall__cell-placeholder {
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--color-text-dim);
  font-size: 0.75rem;
}

.wall__empty {
  grid-column: 1 / -1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  padding: 10vh 0;
  color: var(--color-text-dim);
}

.wall__empty-icon {
  font-size: 2rem;
}

.wall__empty-text {
  font-size: 0.8125rem;
}

.wall__dialog-mask {
  position: fixed;
  inset: 0;
  z-index: 100;
  background: rgba(2, 4, 8, 0.7);
  display: flex;
  align-items: center;
  justify-content: center;
}

.wall__dialog {
  width: min(420px, calc(100vw - 32px));
  background: var(--color-bg-panel);
  border: 1px solid var(--color-border-default);
  border-radius: var(--radius-lg);
  padding: var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.wall__dialog-title {
  font-size: 0.9375rem;
  font-weight: 600;
  color: var(--color-text-bright);
}

.wall__dialog-discovered {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
}

/* ---- 设备选择列表（发现优先） ---- */
.wall__pick {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.wall__pick-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 0.75rem;
  color: var(--color-text-secondary);
}

.wall__refresh {
  border: 1px solid var(--color-border-subtle);
  background: var(--color-bg-hover);
  color: var(--color-text-secondary);
  font-size: 0.7rem;
  padding: 2px 10px;
  border-radius: var(--radius-sm);
  cursor: pointer;
}

.wall__refresh:disabled {
  opacity: 0.6;
  cursor: wait;
}

.wall__device-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-height: 220px;
  overflow-y: auto;
}

.wall__device-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  border: 1px solid var(--color-border-subtle);
  background: var(--color-bg-elevated);
  padding: 7px 10px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  text-align: left;
  transition: all 0.12s;
}

.wall__device-item:hover:not(:disabled) {
  border-color: var(--color-accent-green);
}

.wall__device-item--selected {
  border-color: var(--color-accent-green);
  background: rgba(0, 229, 160, 0.1);
}

.wall__device-item--onwall {
  opacity: 0.45;
  cursor: not-allowed;
}

.wall__device-name {
  font-size: 0.8125rem;
  color: var(--color-text-bright);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.wall__device-host {
  font-family: var(--font-mono);
  font-size: 0.7rem;
  color: var(--color-text-dim);
  margin-left: auto;
  flex-shrink: 0;
}

.wall__device-badge {
  font-size: 0.65rem;
  color: var(--color-text-dim);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-full);
  padding: 1px 8px;
  flex-shrink: 0;
}

.wall__device-empty {
  padding: 14px 0;
  text-align: center;
  font-size: 0.75rem;
  color: var(--color-text-dim);
}

.wall__picked {
  font-size: 0.75rem;
  color: var(--color-accent-green);
}

.wall__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 0.75rem;
  color: var(--color-text-secondary);
}

.wall__field input {
  background: var(--color-bg-elevated);
  border: 1px solid var(--color-border-default);
  border-radius: var(--radius-sm);
  color: var(--color-text-bright);
  padding: 7px 10px;
  font-size: 0.8125rem;
}

.wall__field input:focus {
  outline: none;
  border-color: var(--color-accent-green);
}

.wall__dialog-hint {
  font-size: 0.7rem;
  color: var(--color-text-dim);
}

.wall__dialog-error {
  font-size: 0.75rem;
  color: var(--color-accent-red);
  background: rgba(255, 61, 87, 0.1);
  border-radius: var(--radius-sm);
  padding: 6px 10px;
}

.wall__dialog-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}

.wall__btn {
  border: 1px solid var(--color-border-default);
  background: var(--color-bg-hover);
  color: var(--color-text-secondary);
  font-size: 0.8rem;
  padding: 6px 16px;
  border-radius: var(--radius-sm);
  cursor: pointer;
}

.wall__btn--primary {
  background: rgba(0, 229, 160, 0.85);
  border-color: transparent;
  color: #04110c;
}

.wall__btn--primary:disabled {
  opacity: 0.6;
  cursor: wait;
}
</style>
