package main

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/onvif-ai/internal/audio"
	"github.com/onvif-ai/internal/llm"
	"github.com/onvif-ai/internal/onvif"
	"github.com/onvif-ai/internal/onvif/discovery"
	"github.com/onvif-ai/internal/rtsp"
	"github.com/onvif-ai/internal/server"
	"github.com/onvif-ai/internal/tts"
	"github.com/onvif-ai/internal/ws"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	hub := ws.NewHub()
	go hub.Run()

	discListener := discovery.NewListener()
	if err := discListener.Start(); err != nil {
		log.Printf("Discovery listener failed: %v (probe-only mode)", err)
	} else {
		log.Println("ONVIF discovery listener started — passively detecting Hello messages")
		defer discListener.Stop()
	}

	h := server.NewHandler(hub, discListener)

	llmCfg := llm.Config{
		BaseURL: getEnv("LLM_BASE_URL", "https://api.openai.com"),
		APIKey:  os.Getenv("LLM_API_KEY"),
		Model:   getEnv("LLM_MODEL", "gpt-3.5-turbo"),
	}

	ttsCfg := tts.Config{
		Provider: getEnv("TTS_PROVIDER", "edge-tts"),
		Voice:    getEnv("TTS_VOICE", "zh-CN-XiaoxiaoNeural"),
		Endpoint: os.Getenv("TTS_ENDPOINT"),
	}

	llmClient := llm.NewClient(llmCfg)
	ttsClient := tts.NewClient(ttsCfg)

	h.SetLLMConfig(&server.LLMConfig{
		BaseURL: llmCfg.BaseURL,
		APIKey:  llmCfg.APIKey,
		Model:   llmCfg.Model,
	})

	cm := &cameraManager{
		hub:        hub,
		llmClient:  llmClient,
		ttsClient:  ttsClient,
		handler:    h,
		stopCh:     make(chan struct{}),
	}

	h.SetOnConnect(func(addr string) {
		go cm.connect(addr)
	})

	h.SetLLMUpdateCallback(func(baseURL, apiKey, model string) {
		cm.llmClient.UpdateConfig(baseURL, apiKey, model)
		log.Printf("LLM config updated: %s, model=%s", baseURL, model)
	})

	setupVoiceCallbacks(h, hub, cm)

	router := h.RegisterRoutes()

	port := getEnv("PORT", "8080")
	log.Printf("Server starting on :%s", port)
	log.Println("Open http://localhost:5173 in browser, then click '搜索设备' to discover cameras")

	go func() {
		if err := http.ListenAndServe(":"+port, router); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	cm.disconnect()
}

type cameraManager struct {
	hub         *ws.Hub
	llmClient   *llm.Client
	ttsClient   *tts.Client
	handler     *server.Handler
	onvifClient *onvif.Client

	mu          sync.Mutex
	stream      *rtsp.Stream
	backchannel *rtsp.Backchannel
	currentURL  string
	stopCh      chan struct{}

	cameraAudioBuf    []byte
	cameraAudioBufMax int

	history []llm.Message
}

func (cm *cameraManager) connect(address string) {
	cm.disconnect()

	cm.mu.Lock()
	cm.stopCh = make(chan struct{})
	stopCh := cm.stopCh
	cm.mu.Unlock()

	log.Printf("Connecting to camera: %s", address)
	cm.handler.SetDeviceState(true, false, false, address)

	onvifClient := onvif.NewClient(onvif.Config{
		DeviceAddr: address,
		Timeout:    5 * time.Second,
	})
	cm.onvifClient = onvifClient

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	profiles, err := onvifClient.GetProfiles(ctx)
	if err != nil {
		log.Printf("GetProfiles failed: %v", err)
		cm.handler.SetDeviceState(true, false, false, address)
		return
	}

	log.Printf("Found %d profiles", len(profiles))

	snapshotURL := ""
	if len(profiles) > 0 {
		snapURL, snapErr := onvifClient.GetSnapshotURI(ctx, profiles[0].Token)
		if snapErr != nil {
			log.Printf("GetSnapshotUri failed: %v", snapErr)
		} else if snapURL != "" {
			snapshotURL = snapURL
			log.Printf("Snapshot URL: %s", snapshotURL)
		}
	}

	if snapshotURL == "" {
		snapshotURL = tryFallbackSnapshotURL(address)
		if snapshotURL != "" {
			log.Printf("Using fallback snapshot URL: %s", snapshotURL)
		}
	}

	if snapshotURL != "" {
		go cm.fetchAndShowSnapshot(snapshotURL)
		go cm.startSnapshotLoop(snapshotURL, stopCh)
		cm.handler.SetDeviceState(true, false, true, address)
	} else {
		log.Println("WARNING: No snapshot available at all")
		cm.hub.BroadcastError("摄像头不支持快照功能，且 RTSP 流不可用")
	}

	if len(profiles) == 0 {
		log.Println("No media profiles, using snapshot only")
		cm.handler.SetDeviceState(true, false, true, address)
		return
	}

	uri, err := onvifClient.GetStreamURI(ctx, profiles[0].Token)
	if err != nil {
		log.Printf("GetStreamUri failed: %v, using snapshot", err)
		cm.handler.SetDeviceState(true, false, true, address)
		return
	}

	log.Printf("RTSP URL: %s", uri.URI)

	stream := rtsp.NewStream(uri.URI)
	backchannel := rtsp.NewBackchannel(uri.URI)

	stream.OnVideoNAL(func(nalu []byte) {
		cm.hub.BroadcastVideoNAL(nalu)
	})
	stream.OnAudioPCM(func(pcm []byte) {
		cm.hub.BroadcastAudioPCM(pcm)

		cm.mu.Lock()
		maxBuf := cm.cameraAudioBufMax
		if maxBuf == 0 {
			cm.mu.Unlock()
			return
		}
		cm.cameraAudioBuf = append(cm.cameraAudioBuf, pcm...)
		if len(cm.cameraAudioBuf) > maxBuf {
			excess := len(cm.cameraAudioBuf) - maxBuf
			cm.cameraAudioBuf = cm.cameraAudioBuf[excess:]
		}
		cm.mu.Unlock()
	})

	cm.mu.Lock()
	cm.stream = stream
	cm.backchannel = backchannel
	cm.currentURL = uri.URI
	cm.mu.Unlock()

	cm.cameraAudioBufMax = 160000

	go func() {
		if err := stream.Connect(); err != nil {
			log.Printf("RTSP stream failed: %v, keeping snapshot mode", err)
			cm.handler.SetDeviceState(true, false, true, address)
			return
		}
		log.Println("RTSP stream connected — switching to live video")
		close(stopCh)
		cm.handler.SetDeviceState(true, true, false, address)
	}()

			go func() {
		if err := backchannel.Connect(); err != nil {
			log.Printf("Audio backchannel unavailable: %v", err)
			return
		}
		log.Println("Audio backchannel connected")
	}()
}

func tryFallbackSnapshotURL(deviceAddr string) string {
	if !strings.HasPrefix(deviceAddr, "http") {
		deviceAddr = "http://" + deviceAddr
	}
	idx := strings.Index(deviceAddr, "/onvif/")
	if idx < 0 {
		return ""
	}
	base := deviceAddr[:idx]

	candidates := []string{
		base + "/onvif/snapshot",
		base + "/snapshot.jpg",
		base + "/snapshot",
		base + "/cgi-bin/snapshot.cgi",
		base + "/web/cgi-bin/hi3510/snap.cgi",
	}

	httpClient := &http.Client{Timeout: 2 * time.Second}
	for _, url := range candidates {
		resp, err := httpClient.Head(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return url
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return ""
}

func (cm *cameraManager) fetchAndShowSnapshot(snapshotURL string) {
	if snapshotURL == "" {
		return
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(snapshotURL)
	if err != nil {
		log.Printf("Snapshot fetch failed: %v", err)
		cm.hub.BroadcastError("快照获取失败: " + err.Error())
		return
	}
	defer resp.Body.Close()

	jpeg, err := io.ReadAll(resp.Body)
	if err != nil || len(jpeg) == 0 {
		return
	}

	cm.hub.BroadcastVideoJPEG(jpeg)
}

func (cm *cameraManager) startSnapshotLoop(snapshotURL string, stopCh chan struct{}) {
	if snapshotURL == "" {
		return
	}

	lastTick := time.Now()
	for {
		fps := cm.handler.GetSnapshotFPS()
		interval := time.Second / time.Duration(fps)
		if interval < 33*time.Millisecond {
			interval = 33 * time.Millisecond
		}

		select {
		case <-stopCh:
			return
		case <-time.After(interval):
		}

		now := time.Now()
		if now.Sub(lastTick) < interval-time.Millisecond*10 {
			continue
		}
		lastTick = now
		cm.fetchAndShowSnapshot(snapshotURL)
	}
}

func setupVoiceCallbacks(h *server.Handler, hub *ws.Hub, cm *cameraManager) {
	h.SetAudioCallbacks(
		func(data []byte) {},
		func() {},
		func() {},
		func(text string) {
			hub.BroadcastStatus(ws.StatusThinking)
			go func(prompt string) {
				bc := cm.getBackchannel()
				processLLMResponseWithHistory(cm.llmClient, cm.ttsClient, hub, bc, prompt, &cm.history)
				hub.BroadcastStatus(ws.StatusIdle)
			}(text)
		},
		func() {
			log.Println("Camera listen triggered")
			hub.BroadcastStatus(ws.StatusThinking)
			go func() {
				cm.processCameraAudio()
				hub.BroadcastStatus(ws.StatusIdle)
			}()
		},
		func() {
			cm.mu.Lock()
			cm.history = nil
			cm.mu.Unlock()
			log.Println("Conversation history cleared")
			hub.BroadcastStatus(ws.StatusIdle)
		},
		func(mode string) {
			log.Printf("Audio mode: %s", mode)
		},
	)
	hub.BroadcastStatus(ws.StatusIdle)
}

func (cm *cameraManager) getBackchannel() *rtsp.Backchannel {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.backchannel
}

func (cm *cameraManager) processCameraAudio() {
	cm.mu.Lock()
	buf := make([]byte, len(cm.cameraAudioBuf))
	copy(buf, cm.cameraAudioBuf)
	cm.cameraAudioBuf = nil
	cm.mu.Unlock()

	if len(buf) == 0 {
		log.Println("No camera audio buffered")
		cm.hub.BroadcastError("摄像头没有缓冲到音频数据")
		return
	}

	ctx := context.Background()
	wavData := audio.PCMToWAV(buf, audio.G711SampleRate)
	text, err := cm.llmClient.Transcribe(ctx, wavData, "wav")
	if err != nil {
		log.Printf("Whisper STT failed: %v", err)
		cm.hub.BroadcastError("语音识别失败: " + err.Error())
		return
	}

	log.Printf("Recognized from camera: %s", text)
	if text == "" {
		cm.hub.BroadcastError("未识别到语音内容")
		return
	}

	bc := cm.getBackchannel()
	processLLMResponseWithHistory(cm.llmClient, cm.ttsClient, cm.hub, bc, text, &cm.history)
}

func (cm *cameraManager) disconnect() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.stopCh != nil {
		close(cm.stopCh)
	}
	if cm.stream != nil {
		cm.stream.Close()
		cm.stream = nil
	}
	if cm.backchannel != nil {
		cm.backchannel.Close()
		cm.backchannel = nil
	}
	cm.currentURL = ""
	cm.handler.SetDeviceState(false, false, false, "未连接")
	cm.hub.BroadcastStatus(ws.StatusIdle)
}

func processLLMResponse(llmClient *llm.Client, ttsClient *tts.Client, hub *ws.Hub, backchannel *rtsp.Backchannel, prompt string) {
	processLLMResponseWithHistory(llmClient, ttsClient, hub, backchannel, prompt, nil)
}

func processLLMResponseWithHistory(llmClient *llm.Client, ttsClient *tts.Client, hub *ws.Hub, backchannel *rtsp.Backchannel, prompt string, history *[]llm.Message) {
	ctx := context.Background()

	log.Printf("LLM prompt: %s", prompt)

	messages := []llm.Message{
		{Role: "system", Content: "你是一个友好的语音助手。请用简洁的中文回答用户的问题，回答控制在2-3句话以内，适合语音播放。"},
	}

	if history != nil {
		messages = append(messages, *history...)
	}

	messages = append(messages, llm.Message{Role: "user", Content: prompt})

	fullText, err := llmClient.ChatStream(ctx, messages, func(chunk string) error {
		hub.BroadcastTranscript(chunk)
		return nil
	})
	if err != nil {
		log.Printf("LLM error: %v", err)
		hub.BroadcastError("AI响应失败: " + err.Error())
		return
	}

	log.Printf("LLM response: %s", fullText)

	if history != nil && fullText != "" {
		*history = append(*history,
			llm.Message{Role: "user", Content: prompt},
			llm.Message{Role: "assistant", Content: fullText},
		)
		const maxHistory = 20
		if len(*history) > maxHistory {
			*history = (*history)[len(*history)-maxHistory:]
		}
	}

	hub.BroadcastStatus(ws.StatusSpeaking)

	if backchannel != nil {
		pcmAudio, err := ttsClient.Synthesize(ctx, fullText)
		if err != nil {
			log.Printf("TTS error (backchannel only, browser uses SpeechSynthesis): %v", err)
		} else if len(pcmAudio) > 0 {
			log.Printf("TTS audio for backchannel: %d bytes", len(pcmAudio))
			if err := backchannel.WritePCM(pcmAudio); err != nil {
				log.Printf("Backchannel write error: %v", err)
			}
		}
	}

	hub.BroadcastTranscript("\n\n")
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

var _ = audio.PCM16kSampleRate
var _ = base64.StdEncoding
var _ = strings.TrimSpace
