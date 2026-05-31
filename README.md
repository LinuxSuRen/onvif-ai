# ONVIF AI — 摄像头 AI 语音助手

基于 ONVIF 协议的 IP 摄像头 AI 实时语音问答系统。连接摄像头后，可通过浏览器或摄像头麦克风向 AI 提问，AI 回答通过摄像头喇叭或浏览器语音播报。

## 功能

- 🔍 **ONVIF 自动发现**（WS-Discovery 组播 + Hello 监听）
- 📹 **实时视频流**（RTSP H.264 / 快照降级 1FPS）
- 🎙️ **浏览器语音识别**（Chrome SpeechRecognition，无需 API Key）
- 📷 **摄像头麦克风收音**（G.711 → Whisper STT）
- 🤖 **大模型对话**（OpenAI 兼容接口，DeepSeek/SiliconFlow 等）
- 🔊 **TTS 语音播报**（Edge TTS 免费 / 自定义接口）
- 📡 **WebSocket 实时通信**（视频帧、音频、状态全走 WS）

## 快速开始

### 环境要求

- Go 1.21+
- Node.js 18+
- Chrome 或 Edge 浏览器（语音识别需要）
- ONVIF 摄像头（可选，Demo 模式不需要）

### 配置

```bash
cp .env.example .env
# 编辑 .env，填入 LLM API Key
```

### 启动

```bash
make run    # 后端 :8080
make web-dev # 前端 :5173 (新终端)
```

打开 `http://localhost:5173`

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ONVIF_ADDR` | `192.168.1.138:8089/onvif/device_service` | ONVIF 设备地址 |
| `LLM_BASE_URL` | `https://api.openai.com` | LLM API 地址 |
| `LLM_API_KEY` | — | API Key |
| `LLM_MODEL` | `gpt-3.5-turbo` | 模型名称 |
| `TTS_PROVIDER` | `edge-tts` | TTS 引擎 (`edge-tts` / `http`) |
| `TTS_VOICE` | `zh-CN-XiaoxiaoNeural` | 语音名称 |
| `PORT` | `8080` | HTTP 端口 |

## 架构

```
浏览器 ←─WebSocket─→ Go 后端 ←─RTSP/ONVIF─→ IP 摄像头
                         ↕
                    LLM API (DeepSeek)
                    TTS Engine (Edge)
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
