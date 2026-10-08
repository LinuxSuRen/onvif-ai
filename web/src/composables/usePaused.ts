import { ref, type Ref } from 'vue'

const STORAGE_KEY = 'onvif-ai-paused'

/**
 * 全局播放/暂停状态（跨组件共享单例，与 useWebSocket 同模式）。
 *
 * 暂停是纯前端行为：丢弃新到的视频帧（画面冻结在最后一帧）并静音
 * 实时音频，后端拉流与 WS 收发不受影响；恢复后由既有追帧逻辑跳回
 * 当前直播沿。用户选择记忆到 localStorage，刷新/重连后保持。
 */
const paused: Ref<boolean> = ref(localStorage.getItem(STORAGE_KEY) === '1')

export function usePaused() {
  function togglePause(): void {
    paused.value = !paused.value
    localStorage.setItem(STORAGE_KEY, paused.value ? '1' : '0')
  }

  return { paused, togglePause }
}
