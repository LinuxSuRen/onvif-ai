package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"
	"github.com/onvif-ai/internal/onvif"
	"github.com/onvif-ai/internal/onvif/discovery"
	"github.com/onvif-ai/internal/ws"
)

type DeviceState struct {
	Connected    bool   `json:"connected"`
	Streaming    bool   `json:"streaming"`
	Address      string `json:"address"`
	SnapshotMode bool   `json:"snapshot_mode"`
	PTZSupported bool   `json:"ptz_supported"`
	// Cameras 列出单设备多摄像头（多 media profile）时每一路画面的状态
	Cameras []CameraState `json:"cameras,omitempty"`
	// Audio 是设备级音频轨状态（取第一路画面的音频）。
	// nil 表示尚未协商（未连接/还在建流）；非 nil 且 Available=false
	// 表示已确认无音频轨；Available=true 时携带编码与参数。
	Audio *AudioState `json:"audio,omitempty"`
	// Talkback 是设备级对讲回传通道（RTSP backchannel）状态。
	// nil 表示尚未协商；非 nil 且 Available=false 表示不可对讲，
	// Reason 携带稳定原因码（no_backchannel / connect_failed），
	// 前端据此禁用对讲按钮并展示原因。
	Talkback *TalkbackState `json:"talkback,omitempty"`
}

// TalkbackState 描述对讲回传通道的可见状态（随 device_state 广播）。
type TalkbackState struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // no_backchannel / connect_failed
}

// AudioState 描述设备级音频轨的可见状态。
type AudioState struct {
	Available  bool   `json:"available"`
	Codec      string `json:"codec,omitempty"`       // "G.711" / "AAC-LC"
	SampleRate int    `json:"sample_rate,omitempty"` // Hz
	Channels   int    `json:"channels,omitempty"`
	Degraded   bool   `json:"degraded,omitempty"` // 解码失败已降级（视频不受影响）
	Reason     string `json:"reason,omitempty"`
}

// CameraState 是一路画面（一个 media profile）的运行状态。
type CameraState struct {
	Token        string `json:"token"`
	Name         string `json:"name"`
	PTZSupported bool   `json:"ptz_supported"`
	// PTZPanTilt/PTZZoom 按 GetConfigurationOptions 的速度空间区分云台
	// 与变焦能力；未查询到（老设备/查询失败）时均为 true，保持旧版
	// 全显示行为。
	PTZPanTilt   bool   `json:"ptz_pan_tilt"`
	PTZZoom      bool   `json:"ptz_zoom"`
	Streaming    bool   `json:"streaming"`
	SnapshotMode bool   `json:"snapshot_mode"`
	// MJPEG 表示该路为 JPEG 帧流（RTSP MJPEG），前端按连续图片渲染而非 H.264
	MJPEG bool `json:"mjpeg,omitempty"`
	// Width/Height 是该路画面的像素分辨率（H.264 SPS / JPEG SOF 解析），
	// 0 表示尚未得知，前端不展示角标
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

type Handler struct {
	hub            *ws.Hub
	listener       *discovery.Listener
	upgrader       websocket.Upgrader
	deviceState    DeviceState
	llmConfig      *LLMConfig
	snapshotFPS    int
	onConnect      func(address string)
	onAudioData    func([]byte)
	onAudioStart   func()
	onAudioStop    func()
	onSpeechText   func(string)
	onCameraListen func()
	onClearHistory func()
	onPTZMove      func(camera, direction string, step bool)
	onPTZStop      func(camera string)
	onSwitchMode   func(string)
	onLLMUpdate    func(baseURL, apiKey, model string)
	// 对讲会话回调：onTalkbackStart 受理会话（返回是否接受与拒绝码），
	// onTalkbackData 转发 PCM 音频，onTalkbackStop 结束会话
	onTalkbackStart func() (bool, string)
	onTalkbackData  func([]byte)
	onTalkbackStop  func()
	// talkbackOwner 持有当前对讲会话的客户端；其连接断开时自动释放，
	// 避免会话悬挂导致 TTS 回传被永久阻塞
	talkbackOwner *ws.Client
	mu            sync.RWMutex
}

type LLMConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

func NewHandler(hub *ws.Hub, listener *discovery.Listener) *Handler {
	return &Handler{
		hub:      hub,
		listener: listener,
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  1024 * 64,
			WriteBufferSize: 1024 * 64,
		},
		deviceState: DeviceState{
			Address: "",
		},
		snapshotFPS: 1,
	}
}

func (h *Handler) SetCallbacks(onData func([]byte), onStart func(), onStop func(), onSpeech func(string), onCamera func(), onClear func(), onPTZ func(camera, direction string, step bool), onMode func(string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onAudioData = onData
	h.onAudioStart = onStart
	h.onAudioStop = onStop
	h.onSpeechText = onSpeech
	h.onCameraListen = onCamera
	h.onClearHistory = onClear
	h.onPTZMove = onPTZ
	h.onSwitchMode = onMode
}

func (h *Handler) SetOnConnect(fn func(address string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onConnect = fn
}

// SetPTZStopCallback 注册云台停止回调（收到 ptz_stop 时调用）。
// 单独一个 setter 而非并入 SetCallbacks：SetCallbacks 已有多个同型的
// func(string) 位置参数，再加会极易在调用处接错顺序。
func (h *Handler) SetPTZStopCallback(fn func(camera string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onPTZStop = fn
}

// SetTalkbackCallbacks 注册对讲（浏览器麦克风 → 摄像头扬声器）会话回调。
// onTalkbackStart 在收到 talkback_start 时调用，返回是否受理与稳定拒绝码；
// onTalkbackData 在收到 audio_in 时调用（仅会话属主的消息会被转发）；
// onTalkbackStop 在收到 talkback_stop 或属主连接断开时调用。
func (h *Handler) SetTalkbackCallbacks(onStart func() (bool, string), onData func([]byte), onStop func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onTalkbackStart = onStart
	h.onTalkbackData = onData
	h.onTalkbackStop = onStop
}

// SetDeviceState updates the aggregate device fields; an empty addr keeps the
// current address. Cameras 与 PTZ 汇总字段由 SetCameras 维护，此处保留。
func (h *Handler) SetDeviceState(connected, streaming, snapshot bool, addr string) {
	h.mu.Lock()
	if addr == "" {
		addr = h.deviceState.Address
	}
	h.deviceState.Connected = connected
	h.deviceState.Streaming = streaming
	h.deviceState.SnapshotMode = snapshot
	h.deviceState.Address = addr
	state := h.deviceState
	h.mu.Unlock()
	h.hub.BroadcastDeviceState(state)
}

// SetCameras 更新多摄像头（多 media profile）列表及每路状态，同时刷新
// 汇总的 PTZ 能力字段（取第一路），并重新广播 device_state。
func (h *Handler) SetCameras(cams []CameraState) {
	h.mu.Lock()
	h.deviceState.Cameras = cams
	if len(cams) > 0 {
		h.deviceState.PTZSupported = cams[0].PTZSupported
	} else {
		h.deviceState.PTZSupported = false
	}
	state := h.deviceState
	h.mu.Unlock()
	h.hub.BroadcastDeviceState(state)
}

// SetAudio 更新设备级音频轨状态并重新广播 device_state。
// nil 表示回到“未协商/未知”。新客户端连接时由 handleWebSocket 下发完整
// 状态种子，前端因此无需轮询。
func (h *Handler) SetAudio(st *AudioState) {
	h.mu.Lock()
	h.deviceState.Audio = st
	state := h.deviceState
	h.mu.Unlock()
	h.hub.BroadcastDeviceState(state)
}

// SetTalkback 更新设备级对讲回传通道状态并重新广播 device_state。
// nil 表示回到“未协商/未知”；新客户端连接时由 handleWebSocket 下发完整
// 状态种子，前端对讲按钮据此切换可用态。
func (h *Handler) SetTalkback(st *TalkbackState) {
	h.mu.Lock()
	h.deviceState.Talkback = st
	state := h.deviceState
	h.mu.Unlock()
	h.hub.BroadcastDeviceState(state)
}

func (h *Handler) RegisterRoutes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	}))

	r.Get("/ws", h.handleWebSocket)
	r.Get("/health", h.handleHealth)
	r.Get("/api/camera/discover", h.handleDiscover)
	r.Get("/api/camera/info", h.handleCameraInfo)
	r.Post("/api/camera/connect", h.handleConnect)
	r.Get("/api/camera/device-info", h.handleDeviceInfo)
	r.Get("/api/llm/config", h.handleLLMGetConfig)
	r.Post("/api/llm/config", h.handleLLMSetConfig)
	r.Get("/api/settings", h.handleGetSettings)
	r.Post("/api/settings", h.handleSetSettings)

	fileServer := http.FileServer(http.Dir("web/dist"))
	r.Handle("/*", fileServer)

	return r
}

func (h *Handler) handleDiscover(w http.ResponseWriter, r *http.Request) {
	var allDevices []discovery.Device

	if h.listener != nil {
		allDevices = h.listener.Devices()
	}

	probed, err := discovery.Probe("")
	if err == nil {
		seen := make(map[string]bool)
		for _, d := range allDevices {
			seen[d.Address] = true
		}
		for _, d := range probed {
			if !seen[d.Address] {
				allDevices = append(allDevices, d)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"devices": allDevices,
		"error":   errMsg,
	})
}

func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Address == "" {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	fn := h.onConnect
	h.mu.RUnlock()

	if fn != nil {
		fn(req.Address)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "connecting", "address": req.Address})
}

func (h *Handler) handleDeviceInfo(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if address == "" {
		http.Error(w, `{"error":"missing address"}`, http.StatusBadRequest)
		return
	}

	client := onvif.NewClient(onvif.Config{
		DeviceAddr: address,
		Timeout:    5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	info, err := client.GetDeviceInformation(ctx)
	if err != nil {
		log.Printf("GetDeviceInformation for %s failed: %v", address, err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (h *Handler) handleCameraInfo(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	state := h.deviceState
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

func (h *Handler) GetSnapshotFPS() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.snapshotFPS <= 0 {
		return 1
	}
	return h.snapshotFPS
}

func (h *Handler) SetLLMUpdateCallback(fn func(baseURL, apiKey, model string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onLLMUpdate = fn
}

func (h *Handler) SetLLMConfig(cfg *LLMConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.llmConfig = cfg
}

func (h *Handler) handleLLMGetConfig(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	cfg := h.llmConfig
	h.mu.RUnlock()

	resp := map[string]string{}
	if cfg != nil {
		resp["base_url"] = cfg.BaseURL
		resp["api_key"] = maskKey(cfg.APIKey)
		resp["model"] = cfg.Model
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleLLMSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	if h.llmConfig == nil {
		h.llmConfig = &LLMConfig{}
	}
	if req.BaseURL != "" {
		h.llmConfig.BaseURL = req.BaseURL
	}
	if req.APIKey != "" && req.APIKey != "***" {
		h.llmConfig.APIKey = req.APIKey
	}
	if req.Model != "" {
		h.llmConfig.Model = req.Model
	}
	h.mu.Unlock()

	if h.onLLMUpdate != nil {
		go h.onLLMUpdate(req.BaseURL, req.APIKey, req.Model)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"snapshot_fps": h.snapshotFPS,
	})
}

func (h *Handler) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SnapshotFPS int `json:"snapshot_fps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if req.SnapshotFPS < 0 || req.SnapshotFPS > 30 {
		req.SnapshotFPS = 1
	}
	h.mu.Lock()
	h.snapshotFPS = req.SnapshotFPS
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:5] + "***" + key[len(key)-3:]
}

func mustMarshal(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "ok",
		"client_count": h.hub.ClientCount(),
	})
}

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := h.hub.RegisterClient(conn)

	// Seed the new client with the current device state so a page that loads
	// mid-session immediately knows about streaming/snapshot/PTZ flags.
	h.mu.RLock()
	state := h.deviceState
	h.mu.RUnlock()
	if state.Address != "" {
		payload, err := json.Marshal(state)
		if err == nil {
			client.Send(&ws.Message{Type: ws.MsgTypeDeviceState, Payload: payload})
		}
	}

	go client.WritePump()
	go func() {
		client.ReadPump(func(msg *ws.Message) {
			h.handleClientMessage(client, msg)
		})
		// 连接断开：若该客户端持有对讲会话则立即释放，
		// 否则会话悬挂导致 TTS 回传被“占用中”永久阻塞
		if h.takeTalkbackOwner(client) {
			h.mu.RLock()
			fn := h.onTalkbackStop
			h.mu.RUnlock()
			if fn != nil {
				fn()
			}
		}
	}()
}

// ownsTalkback 判断 client 是否当前对讲会话属主。
func (h *Handler) ownsTalkback(client *ws.Client) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.talkbackOwner == client
}

// takeTalkbackOwner 若 client 是会话属主则清除属主并返回 true；
// 非属主（无会话或他人会话）返回 false，不产生任何副作用。
func (h *Handler) takeTalkbackOwner(client *ws.Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.talkbackOwner == client {
		h.talkbackOwner = nil
		return true
	}
	return false
}

func (h *Handler) handleClientMessage(client *ws.Client, msg *ws.Message) {
	switch msg.Type {
	case ws.MsgTypeAudioStart:
		h.mu.RLock()
		if h.onAudioStart != nil {
			h.onAudioStart()
		}
		h.mu.RUnlock()

	case ws.MsgTypeAudioStop:
		h.mu.RLock()
		if h.onAudioStop != nil {
			h.onAudioStop()
		}
		h.mu.RUnlock()

	case ws.MsgTypeAudioData:
		if msg.Data != "" {
			audio, err := base64.StdEncoding.DecodeString(msg.Data)
			if err == nil {
				h.mu.RLock()
				if h.onAudioData != nil {
					h.onAudioData(audio)
				}
				h.mu.RUnlock()
			}
		}

	case ws.MsgTypeSwitchMode:
		var payload struct{ Mode string }
		if msg.Payload != nil {
			json.Unmarshal(msg.Payload, &payload)
		}
		h.mu.RLock()
		if h.onSwitchMode != nil {
			h.onSwitchMode(payload.Mode)
		}
		h.mu.RUnlock()

	case ws.MsgTypeSpeechText:
		if msg.Text != "" {
			h.mu.RLock()
			if h.onSpeechText != nil {
				h.onSpeechText(msg.Text)
			}
			h.mu.RUnlock()
		}

	case ws.MsgTypeCameraListen:
		h.mu.RLock()
		if h.onCameraListen != nil {
			h.onCameraListen()
		}
		h.mu.RUnlock()

	case ws.MsgTypeClearHistory:
		h.mu.RLock()
		if h.onClearHistory != nil {
			h.onClearHistory()
		}
		h.mu.RUnlock()

	case ws.MsgTypePTZMove:
		var payload struct {
			Camera    string
			Direction string
			// Step 表示轻点步进（变焦按钮短按松开后补发的完整一步）
			Step bool
		}
		if msg.Payload != nil {
			json.Unmarshal(msg.Payload, &payload)
		}
		if payload.Direction != "" {
			h.mu.RLock()
			if h.onPTZMove != nil {
				h.onPTZMove(payload.Camera, payload.Direction, payload.Step)
			}
			h.mu.RUnlock()
		}

	case ws.MsgTypePTZStop:
		var payload struct {
			Camera string
		}
		if msg.Payload != nil {
			json.Unmarshal(msg.Payload, &payload)
		}
		h.mu.RLock()
		if h.onPTZStop != nil {
			h.onPTZStop(payload.Camera)
		}
		h.mu.RUnlock()

	case ws.MsgTypeTalkbackStart:
		h.mu.RLock()
		fn := h.onTalkbackStart
		h.mu.RUnlock()
		if fn == nil {
			return
		}
		ok, reason := fn()
		if ok {
			h.mu.Lock()
			h.talkbackOwner = client
			h.mu.Unlock()
		}
		// 受理结果只回给发起方，不打扰其他客户端
		client.Send(&ws.Message{
			Type:    ws.MsgTypeTalkbackState,
			Payload: mustMarshal(ws.TalkbackSessionPayload{Active: ok, Reason: reason}),
		})

	case ws.MsgTypeAudioIn:
		// 只转发会话属主的音频，避免旁路客户端混入他人会话
		if !h.ownsTalkback(client) {
			return
		}
		if msg.Data != "" {
			pcm, err := base64.StdEncoding.DecodeString(msg.Data)
			if err == nil && len(pcm) > 0 {
				h.mu.RLock()
				fn := h.onTalkbackData
				h.mu.RUnlock()
				if fn != nil {
					fn(pcm)
				}
			}
		}

	case ws.MsgTypeTalkbackStop:
		if h.takeTalkbackOwner(client) {
			h.mu.RLock()
			fn := h.onTalkbackStop
			h.mu.RUnlock()
			if fn != nil {
				fn()
			}
		}

	case ws.MsgTypeClockSync:
		// Echo the client timestamp together with the server clock so
		// the browser can estimate the clock offset (RTT/2 correction)
		// and compute end-to-end video latency from frame timestamps.
		var payload struct {
			T0 int64 `json:"t0"`
		}
		if msg.Payload != nil {
			json.Unmarshal(msg.Payload, &payload)
		}
		client.Send(&ws.Message{
			Type:    ws.MsgTypeClockSync,
			Payload: mustMarshal(map[string]int64{"t0": payload.T0, "t1": time.Now().UnixMilli()}),
		})
	}
}
