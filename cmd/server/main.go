package main

import (
	"context"
	"encoding/base64"
	"fmt"
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

// version / commit 由构建注入（-ldflags -X），release 二进制与 Docker 镜像携带。
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("onvif-ai %s (commit %s)", version, commit)

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
		hub:       hub,
		llmClient: llmClient,
		ttsClient: ttsClient,
		handler:   h,
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
	life        *streamLife

	cameraAudioBuf    []byte
	cameraAudioBufMax int

	history      []llm.Message
	profileToken string
}

// streamLife owns the stop channel of one connection attempt. The channel is
// closed at most once (close of a closed channel panics and would take the
// whole process down), and its identity lets late goroutines detect that a
// newer connection has superseded them.
type streamLife struct {
	stopCh chan struct{}
	once   sync.Once
}

func newStreamLife() *streamLife {
	return &streamLife{stopCh: make(chan struct{})}
}

func (l *streamLife) stop() {
	if l == nil {
		return
	}
	l.once.Do(func() { close(l.stopCh) })
}

// isCurrent reports whether life is still the active connection of cm.
func (cm *cameraManager) isCurrent(life *streamLife) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.life == life
}

func (cm *cameraManager) connect(address string) {
	cm.disconnect()

	cm.mu.Lock()
	life := newStreamLife()
	cm.life = life
	cm.mu.Unlock()
	stopCh := life.stopCh

	log.Printf("Connecting to camera: %s", address)
	cm.handler.SetDeviceState(true, false, false, address)

	onvifClient := onvif.NewClient(onvif.Config{
		DeviceAddr: address,
		Timeout:    5 * time.Second,
	})

	cm.mu.Lock()
	cm.onvifClient = onvifClient
	cm.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	profiles, err := cm.getProfilesWithRetry(onvifClient, ctx, life, address)
	if err != nil {
		log.Printf("GetProfiles failed: %v", err)
		cm.handler.SetDeviceState(true, false, false, address)
		cm.hub.BroadcastError("连接摄像头失败: " + err.Error())
		return
	}

	log.Printf("Found %d profiles", len(profiles))

	// PTZ capability comes from the profile itself: only show pan/tilt/zoom
	// controls when the camera actually advertises a PTZConfiguration.
	ptzSupported := len(profiles) > 0 && profiles[0].HasPTZ()
	cm.handler.SetPTZSupported(ptzSupported)

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
		go func() {
			if err := cm.fetchAndShowSnapshot(snapshotURL); err != nil {
				log.Printf("Initial snapshot fetch failed: %v", err)
			}
		}()
		go cm.startSnapshotLoop(snapshotURL, stopCh)
		cm.handler.SetDeviceState(true, false, true, address)
	} else {
		log.Println("WARNING: no snapshot endpoint found, relying on RTSP alone")
	}

	if len(profiles) == 0 {
		log.Println("No media profiles, using snapshot only")
		if snapshotURL == "" {
			cm.hub.BroadcastError("摄像头未返回任何媒体配置（profile），无法取流")
		}
		cm.handler.SetDeviceState(true, false, snapshotURL != "", address)
		return
	}

	uri, err := onvifClient.GetStreamURI(ctx, profiles[0].Token)
	if err != nil {
		log.Printf("GetStreamUri failed: %v, using snapshot", err)
		if snapshotURL == "" {
			cm.hub.BroadcastError("获取 RTSP 地址失败: " + err.Error())
		}
		cm.handler.SetDeviceState(true, false, snapshotURL != "", address)
		return
	}

	log.Printf("RTSP URL: %s", uri.URI)

	cm.mu.Lock()
	cm.profileToken = profiles[0].Token
	cm.mu.Unlock()

	cm.cameraAudioBufMax = 160000
	go cm.runRTSPLoop(uri.URI, life, address, snapshotURL != "")
}

// getProfilesWithRetry keeps polling the ONVIF endpoint: WiFi cameras are
// often briefly unreachable right after a reboot/drop, and the RTSP-level
// retry loop can only kick in once profiles are known.
func (cm *cameraManager) getProfilesWithRetry(client *onvif.Client, ctx context.Context, life *streamLife, address string) ([]onvif.Profile, error) {
	const maxAttempts = 12
	const retryWait = 5 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !cm.isCurrent(life) {
			return nil, lastErr
		}
		profiles, err := client.GetProfiles(ctx)
		if err == nil {
			return profiles, nil
		}
		lastErr = err
		log.Printf("GetProfiles attempt %d/%d failed: %v", attempt, maxAttempts, err)
		if attempt == maxAttempts {
			return nil, err
		}
		select {
		case <-life.stopCh:
			return nil, lastErr
		case <-time.After(retryWait):
		}
	}
	return nil, lastErr
}

// runRTSPLoop keeps retrying the RTSP connection for a while: cameras that
// only start publishing after the client connects (e.g. an app-powered sport
// camera answering 503 until its encoder produces frames) would otherwise
// need a manual re-connect.
func (cm *cameraManager) runRTSPLoop(rtspURL string, life *streamLife, address string, hasSnapshot bool) {
	backchannel := rtsp.NewBackchannel(rtspURL)
	cm.mu.Lock()
	cm.backchannel = backchannel
	cm.mu.Unlock()

	go func() {
		if err := backchannel.Connect(); err != nil {
			log.Printf("Audio backchannel unavailable: %v", err)
			return
		}
		log.Println("Audio backchannel connected")
	}()

	const maxAttempts = 12
	const retryWait = 5 * time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !cm.isCurrent(life) {
			return
		}

		stream := rtsp.NewStream(rtspURL)
		stream.OnVideoNAL(func(nalu []byte) {
			cm.hub.BroadcastVideoNAL(nalu)
		})
		stream.OnVideoJPEG(func(jpeg []byte) {
			// MJPEG（RFC 2435）帧为完整 JPEG，复用快照通道推给浏览器
			cm.hub.BroadcastVideoJPEG(jpeg)
		})
		stream.OnAudioPCM(func(pcm []byte) {
			cm.hub.BroadcastAudioPCM(pcm)

			cm.mu.Lock()
			if cm.cameraAudioBufMax > 0 {
				cm.cameraAudioBuf = append(cm.cameraAudioBuf, pcm...)
				if len(cm.cameraAudioBuf) > cm.cameraAudioBufMax {
					excess := len(cm.cameraAudioBuf) - cm.cameraAudioBufMax
					cm.cameraAudioBuf = cm.cameraAudioBuf[excess:]
				}
			}
			cm.mu.Unlock()
		})

		cm.mu.Lock()
		if cm.stream != nil {
			cm.stream.Close()
		}
		cm.stream = stream
		cm.currentURL = rtspURL
		cm.mu.Unlock()

		err := stream.Connect()
		if err == nil {
			log.Println("RTSP stream connected — switching to live video")
			life.stop() // stop the snapshot fallback loop
			if cm.isCurrent(life) {
				cm.handler.SetDeviceState(true, true, false, address)
			}
			// Watch for connection loss (WiFi cameras drop all the time):
			// flip the stale streaming flag and re-run the full connect flow
			// (ONVIF + RTSP retries) after a short backoff.
			stream.WatchDisconnect(func() {
				if !cm.isCurrent(life) {
					return // superseded by a newer connection or closed by us
				}
				log.Println("RTSP stream disconnected — reconnecting")
				cm.handler.SetDeviceState(true, false, hasSnapshot, address)
				time.Sleep(2 * time.Second)
				if cm.isCurrent(life) {
					cm.connect(address)
				}
			})
			return
		}

		log.Printf("RTSP stream attempt %d/%d failed: %v", attempt, maxAttempts, err)
		if attempt == 1 && !hasSnapshot {
			cm.hub.BroadcastError("RTSP 暂不可用（" + err.Error() + "），自动重试中")
		}
		if attempt == maxAttempts {
			log.Printf("RTSP stream giving up after %d attempts", maxAttempts)
			if cm.isCurrent(life) {
				cm.handler.SetDeviceState(true, false, hasSnapshot, address)
			}
			cm.hub.BroadcastError("RTSP 连接失败: " + err.Error())
			return
		}

		select {
		case <-life.stopCh:
			return
		case <-time.After(retryWait):
		}
	}
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

	httpClient := &http.Client{Timeout: 3 * time.Second}
	for _, url := range candidates {
		if probeJPEG(httpClient, url) {
			return url
		}
	}
	return ""
}

// probeJPEG reports whether url really serves a JPEG image. Some devices
// answer HTTP 200 with a SOAP/XML document on every path; trusting the status
// code alone would put a broken image on screen.
func probeJPEG(httpClient *http.Client, url string) bool {
	resp, err := httpClient.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	magic := make([]byte, 2)
	n, err := io.ReadFull(resp.Body, magic)
	return err == nil && n == 2 && magic[0] == 0xFF && magic[1] == 0xD8
}

func (cm *cameraManager) fetchAndShowSnapshot(snapshotURL string) error {
	if snapshotURL == "" {
		return fmt.Errorf("empty snapshot URL")
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(snapshotURL)
	if err != nil {
		return fmt.Errorf("snapshot fetch: %w", err)
	}
	defer resp.Body.Close()

	jpeg, err := io.ReadAll(resp.Body)
	if err != nil || len(jpeg) == 0 {
		return fmt.Errorf("empty snapshot body")
	}
	if jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		return fmt.Errorf("snapshot at %s is not a JPEG (content-type %s)", snapshotURL, resp.Header.Get("Content-Type"))
	}

	cm.hub.BroadcastVideoJPEG(jpeg)
	return nil
}

func (cm *cameraManager) startSnapshotLoop(snapshotURL string, stopCh chan struct{}) {
	if snapshotURL == "" {
		return
	}

	lastTick := time.Now()
	consecutiveFails := 0
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

		if err := cm.fetchAndShowSnapshot(snapshotURL); err != nil {
			consecutiveFails++
			if consecutiveFails == 3 {
				log.Printf("Snapshot loop failing repeatedly: %v", err)
				cm.hub.BroadcastError("快照获取失败: " + err.Error())
			}
			continue
		}
		consecutiveFails = 0
	}
}

func setupVoiceCallbacks(h *server.Handler, hub *ws.Hub, cm *cameraManager) {
	h.SetCallbacks(
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
		func(direction string) {
			cm.handlePTZMove(direction)
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

func (cm *cameraManager) handlePTZMove(direction string) {
	cm.mu.Lock()
	client := cm.onvifClient
	profileToken := cm.profileToken
	cm.mu.Unlock()

	if client == nil || profileToken == "" {
		log.Println("PTZ: no camera connected")
		cm.hub.BroadcastError("云台控制需要先连接摄像头")
		return
	}

	var pan, tilt, zoom float64
	switch direction {
	case "left":
		pan = -1.0
	case "right":
		pan = 1.0
	case "up":
		tilt = 1.0
	case "down":
		tilt = -1.0
	case "zoom_in":
		zoom = 1.0
	case "zoom_out":
		zoom = -1.0
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.PTZContinuousMove(ctx, profileToken, pan, tilt, zoom, 2*time.Second); err != nil {
		log.Printf("PTZ move %s failed: %v", direction, err)
		cm.hub.BroadcastError("云台转动失败: " + err.Error())
		return
	}

	log.Printf("PTZ: moved %s", direction)
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
	life := cm.life
	cm.life = nil
	if cm.stream != nil {
		cm.stream.Close()
		cm.stream = nil
	}
	if cm.backchannel != nil {
		cm.backchannel.Close()
		cm.backchannel = nil
	}
	cm.currentURL = ""
	cm.mu.Unlock()

	life.stop()
	cm.handler.SetDeviceState(false, false, false, "未连接")
	cm.handler.SetPTZSupported(false)
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
