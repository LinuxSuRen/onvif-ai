import { ref } from 'vue'

/**
 * ONVIF 设备发现结果的共享缓存（查看器与视频墙共用）。
 *
 * 主动探测（WS-Discovery 组播）受网络抖动影响可能间歇性返回空，
 * 而查看器里已经发现的设备对视频墙同样可选。这里把发现结果缓存在
 * sessionStorage（同一浏览器会话内跨视图/跨刷新可用）：
 *   - 读取方先拿缓存立即渲染，再后台刷新
 *   - 刷新成功且非空时替换缓存；返回空或失败时保留缓存（本会话内
 *     见过的设备始终可选）
 *   - 查看器（DeviceInfo）发现成功时同样写入，两个视图互为数据源
 */

export interface DiscoveredDevice {
  address: string
  types?: string[]
  scopes?: string[]
  xaddrs?: string[]
  metadata_version?: number
}

const CACHE_KEY = 'onvif-ai-discovered-devices'

// 模块级响应式缓存：同会话内所有订阅者共享同一份数据
const devices = ref<DiscoveredDevice[]>(readCache())

function readCache(): DiscoveredDevice[] {
  try {
    const raw = sessionStorage.getItem(CACHE_KEY)
    if (!raw) return []
    const list = JSON.parse(raw)
    return Array.isArray(list) ? list : []
  } catch {
    return []
  }
}

function writeCache(list: DiscoveredDevice[]) {
  devices.value = list
  try {
    sessionStorage.setItem(CACHE_KEY, JSON.stringify(list))
  } catch {
    /* 存储满/禁用时仅保留内存态 */
  }
}

/**
 * 刷新发现结果。返回最新可用列表（刷新失败或为空时返回缓存）。
 */
export async function refreshDiscovered(): Promise<DiscoveredDevice[]> {
  try {
    const resp = await fetch('/api/camera/discover')
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    const data = await resp.json()
    const list: DiscoveredDevice[] = Array.isArray(data.devices) ? data.devices : []
    if (list.length > 0) {
      writeCache(list)
    }
  } catch {
    /* 探测失败：保留缓存 */
  }
  return devices.value
}

/**
 * 写入发现结果（供查看器等自行探测的入口复用同一份缓存）。
 * 空列表忽略，避免清掉其它视图刚发现的数据。
 */
export function saveDiscovered(list: DiscoveredDevice[]) {
  if (Array.isArray(list) && list.length > 0) {
    writeCache(list)
  }
}

/** 立即取当前缓存（可能为空），需要新鲜数据用 refreshDiscovered */
export function useDiscoveredDevices() {
  return devices
}
