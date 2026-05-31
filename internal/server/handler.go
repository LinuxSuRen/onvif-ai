package server

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"
	"github.com/onvif-ai/internal/onvif/discovery"
	"github.com/onvif-ai/internal/ws"
)

type DeviceState struct {
	Connected    bool   `json:"connected"`
	Streaming    bool   `json:"streaming"`
	Address      string `json:"address"`
	SnapshotMode bool   `json:"snapshot_mode"`
}

type Handler struct {
	hub           *ws.Hub
	listener      *discovery.Listener
	upgrader      websocket.Upgrader
	deviceState   DeviceState
	llmConfig     *LLMConfig
	snapshotFPS   int
	onConnect     func(address string)
	onAudioData   func([]byte)
	onAudioStart  func()
	onAudioStop   func()
	onSpeechText  func(string)
	onCameraListen func()
	onClearHistory func()
	onSwitchMode  func(string)
	onLLMUpdate   func(baseURL, apiKey, model string)
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
			CheckOrigin:    func(r *http.Request) bool { return true },
			ReadBufferSize:  1024 * 64,
			WriteBufferSize: 1024 * 64,
		},
		deviceState: DeviceState{
			Address: os.Getenv("ONVIF_ADDR"),
		},
		snapshotFPS: 1,
	}
}

func (h *Handler) SetAudioCallbacks(onData func([]byte), onStart func(), onStop func(), onSpeech func(string), onCamera func(), onClear func(), onMode func(string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onAudioData = onData
	h.onAudioStart = onStart
	h.onAudioStop = onStop
	h.onSpeechText = onSpeech
	h.onCameraListen = onCamera
	h.onClearHistory = onClear
	h.onSwitchMode = onMode
}

func (h *Handler) SetOnConnect(fn func(address string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onConnect = fn
}

func (h *Handler) SetDeviceState(connected, streaming, snapshot bool, addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deviceState = DeviceState{
		Connected:    connected,
		Streaming:    streaming,
		Address:      addr,
		SnapshotMode: snapshot,
	}
	h.hub.BroadcastDeviceState(h.deviceState)
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

	defaultAddr := h.deviceState.Address
	if defaultAddr == "" || defaultAddr == "未连接" {
		defaultAddr = ""
	} else if !strings.HasPrefix(defaultAddr, "http") {
		defaultAddr = "http://" + defaultAddr
	}

	hasDefault := false
	if defaultAddr != "" {
		for _, d := range allDevices {
			if d.Address == defaultAddr {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			allDevices = append([]discovery.Device{{
				Address: defaultAddr,
				XAddrs:  []string{defaultAddr},
			}}, allDevices...)
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

	go client.WritePump()
	go client.ReadPump(func(msg *ws.Message) {
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
		}
	})
}
