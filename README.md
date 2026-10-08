# ONVIF AI — 摄像头查看器

基于 ONVIF 协议的 IP 摄像头查看与语音对讲工具：自动发现局域网摄像头，浏览器实时观看画面与收听现场音频，按住按钮即可通过麦克风向摄像头扬声器喊话对讲。另提供可选的 AI 语音助手（问答 + 播报），默认收起不影响核心使用。

## 核心功能

- 🔍 **ONVIF 自动发现**（WS-Discovery 组播 + Hello 监听，一键搜索并连接）
- 📹 **实时视频流**（RTSP H.264 / 快照降级 1FPS，多画面预览）
- 🔊 **实时音频监听**（RTSP 流内含 G.711 或 AAC-LC 音频轨时自动在浏览器播放，采样率随源动态适配；无音频轨不受影响。HE-AAC 等其他编码暂不支持，后端会记录告警）
- 📣 **语音对讲**（按住说话，浏览器麦克风音频经 RTSP backchannel 实时推到摄像头扬声器；设备 SDP 需提供 G.711 回传轨，不支持的设备会明确提示并禁用入口）
- 📡 **WebSocket 实时通信**（视频帧、音频、状态全走 WS）

## AI 语音助手（可选）

AI 相关功能默认收起在侧栏「AI 语音助手」折叠面板中，按需展开：

- 🎙️ **浏览器语音识别**（Chrome SpeechRecognition，无需 API Key）
- 📷 **摄像头麦克风收音**（G.711 → Whisper STT）
- 🤖 **大模型对话**（OpenAI 兼容接口，DeepSeek/SiliconFlow 等）
- 🔊 **TTS 语音播报**（Edge TTS 免费 / 自定义接口；经对讲回传通道下发，与语音对讲互斥——对讲优先，占用中会提示稍后）

使用语音识别需要 Chrome 或 Edge 浏览器；需在「设备管理 → AI 模型配置」中填入 LLM API Key。

## 快速开始

### 环境要求

- Go 1.21+
- Node.js 18+
- ONVIF 摄像头（可选，Demo 模式不需要）

### 配置

```bash
cp .env.example .env
# 仅使用查看与对讲无需配置；AI 语音助手才需要填 LLM API Key
```

### 启动

```bash
make run    # 后端 :8080
make web-dev # 前端 :5173 (新终端)
```

打开 `http://localhost:5173`

### 移动端

支持手机/平板浏览器访问（响应式布局，窄屏自动单栏堆叠）：

- PTZ 云台按钮使用 Pointer Events，触屏可按住操作
- iOS 需系统 17.1+（iPhone 的 MSE 支持）；更早版本仅可观看快照降级画面
- 建议通过局域网访问开发机：`http://<开发机IP>:5173`

### Docker

```bash
docker build -t onvif-ai .
# WS-Discovery 依赖 UDP 3702 组播，Linux 下建议 --network host
docker run --network host --env-file .env onvif-ai
```

镜像同时发布到 ghcr.io：`ghcr.io/linuxsuren/onvif-ai:<version>`

### 发布

推送 `v*` tag 触发 [Release workflow](.github/workflows/release.yml)：

```bash
make release VERSION=0.0.1
```

自动完成：多平台二进制（linux amd64/arm64/armv6/armv7、darwin amd64/arm64、windows amd64，
含前端静态资源）→ GitHub Release 附件；多架构 Docker 镜像（linux amd64/arm64）→ ghcr.io。

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ONVIF_ADDR` | `192.168.1.138:8089/onvif/device_service` | ONVIF 设备地址 |
| `LLM_BASE_URL` | `https://api.openai.com` | LLM API 地址（AI 助手） |
| `LLM_API_KEY` | — | API Key（AI 助手） |
| `LLM_MODEL` | `gpt-3.5-turbo` | 模型名称（AI 助手） |
| `TTS_PROVIDER` | `edge-tts` | TTS 引擎 (`edge-tts` / `http`)（AI 助手） |
| `TTS_VOICE` | `zh-CN-XiaoxiaoNeural` | 语音名称（AI 助手） |
| `PORT` | `8080` | HTTP 端口 |

命令行参数 `--port` 可指定端口，优先级高于 `PORT` 环境变量：

```bash
./server --port 3000    # 监听 3000，忽略 PORT 环境变量
```

端口被占用时启动不失败，自动向后漂移到下一个可用端口（最多尝试 20 个连续端口），漂移会在日志中说明；权限不足等其他错误不漂移、直接失败。

## 架构

```
浏览器 ←─WebSocket（视频帧 / 音频 / 对讲 PCM）─→ Go 后端 ←─RTSP/ONVIF─→ IP 摄像头
                                                     │
                                                     ├─ 对讲：麦克风 PCM → RTSP backchannel（RTP）→ 摄像头扬声器
                                                     └─ AI 助手（可选）：LLM API + TTS Engine
```

## 项目结构

```
cmd/server/main.go     # 入口
internal/
  audio/               # G.711 编解码、PCM 工具
  llm/                 # LLM 客户端 (Chat + Whisper STT)
  tts/                 # TTS 客户端 (Edge TTS)
  onvif/               # ONVIF SOAP 客户端 + WS-Discovery
  rtsp/                # RTSP 流 + 音频回传
  ws/                  # WebSocket Hub + 协议
  server/              # HTTP 路由 + API
web/                   # Vue 3 前端
```

## License

MIT
