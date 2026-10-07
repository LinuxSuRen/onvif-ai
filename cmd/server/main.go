package main

import (
	"context"
	"encoding/base64"
	"errors"
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
	units       []*camUnit
	backchannel *rtsp.Backchannel
	life        *streamLife

	cameraAudioBuf    []byte
	cameraAudioBufMax int
	// cameraAudioRate 是当前音频源的实际采样率（SDP 协商结果），
	// 供 STT 的 WAV 封装使用；0 表示尚未收到音频，按 G.711 常规值兜底。
	cameraAudioRate int
	// audioState 是设备级音频轨状态（第一路画面），随 RTSP 协商与
	// 解码降级/自愈更新，经 device_state 广播给前端。
	audioState *server.AudioState

	// talkbackState 是对讲回传通道状态（随 backchannel 连接结果更新），
	// talkbackReady 是其可用位的快照，受理对讲会话时无需再解引用。
	talkbackState *server.TalkbackState
	talkbackReady bool
	// talkbackActive 表示浏览器对讲会话进行中；ttsAudioActive 表示
	// TTS 回传占用 backchannel。二者互斥（先到先得），避免把两条
	// 音频流交错写进同一条 RTSP 回传轨。
	talkbackActive bool
	ttsAudioActive bool

	history []llm.Message
}

// camUnit 是一路画面（一个 ONVIF media profile）的完整生命周期：
// 独立的 RTSP 重试、快照降级与断流恢复，互不影响。
type camUnit struct {
	token       string // profile token，即该路画面在 WS 消息里的 cam 标识
	name        string
	ptz         bool
	rtspURL     string
	snapshotURL string

	// 运行时状态（受 cm.mu 保护）
	streaming   bool
	snapshotOn  bool
	mjpeg       bool // 该路为 MJPEG（JPEG 帧流）而非 H.264
	stream      *rtsp.Stream
	snapLife    *streamLife // 控制该路快照循环；RTSP 起流后停止
	loopRunning bool        // runUnit 防重入：避免断流重连派生并发循环
	width       int         // 画面分辨率（SDP SPS / 带内 SPS / JPEG SOF 解析）
	height      int
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

// maxCams 限制同时拉取的画面路数：带宽与浏览器解码能力有限。
const maxCams = 6

func (cm *cameraManager) connect(address string) {
	cm.disconnect()

	cm.mu.Lock()
	life := newStreamLife()
	cm.life = life
	cm.mu.Unlock()

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

	if len(profiles) > maxCams {
		log.Printf("Found %d profiles, using first %d", len(profiles), maxCams)
		profiles = profiles[:maxCams]
	}
	log.Printf("Found %d profiles", len(profiles))

	if len(profiles) == 0 {
		cm.hub.BroadcastError("摄像头未返回任何媒体配置（profile），无法取流")
		cm.handler.SetDeviceState(true, false, false, address)
		return
	}

	// 每个 profile 建立一路画面单元；快照/取流地址逐路获取
	units := make([]*camUnit, 0, len(profiles))
	for _, p := range profiles {
		u := &camUnit{token: p.Token, name: p.Name, ptz: p.HasPTZ()}

		if snapURL, snapErr := onvifClient.GetSnapshotURI(ctx, p.Token); snapErr != nil {
			log.Printf("GetSnapshotUri(%s) failed: %v", p.Token, snapErr)
		} else if snapURL != "" {
			u.snapshotURL = snapURL
		}

		if uri, uriErr := onvifClient.GetStreamURI(ctx, p.Token); uriErr != nil {
			log.Printf("GetStreamUri(%s) failed: %v, snapshot only", p.Token, uriErr)
		} else {
			u.rtspURL = uri.URI
		}

		units = append(units, u)
	}

	// 单路设备且未拿到快照地址时尝试设备级候选路径（无法归属到 token）
	if len(units) == 1 && units[0].snapshotURL == "" {
		if fallback := tryFallbackSnapshotURL(address); fallback != "" {
			log.Printf("Using fallback snapshot URL: %s", fallback)
			units[0].snapshotURL = fallback
		}
	}

	cm.mu.Lock()
	cm.units = units
	cm.cameraAudioBufMax = 160000
	cm.mu.Unlock()
	cm.syncCameraStates()

	for _, u := range units {
		go cm.runUnit(u, life, address)
	}
}

// syncCameraStates 把各路画面状态汇总为 device_state 广播给前端。
func (cm *cameraManager) syncCameraStates() {
	cm.mu.Lock()
	cams := make([]server.CameraState, 0, len(cm.units))
	anyStreaming := false
	anySnapshot := false
	for _, u := range cm.units {
		cams = append(cams, server.CameraState{
			Token:        u.token,
			Name:         u.name,
			PTZSupported: u.ptz,
			Streaming:    u.streaming,
			SnapshotMode: u.snapshotOn,
			MJPEG:        u.mjpeg,
			Width:        u.width,
			Height:       u.height,
		})
		anyStreaming = anyStreaming || u.streaming
		anySnapshot = anySnapshot || u.snapshotOn
	}
	cm.mu.Unlock()

	cm.handler.SetCameras(cams)
	cm.handler.SetDeviceState(true, anyStreaming, anySnapshot && !anyStreaming, "")
}

// setUnitMode 更新单路画面的运行状态并重新广播。
func (cm *cameraManager) setUnitMode(u *camUnit, streaming, snapshotOn bool) {
	cm.mu.Lock()
	u.streaming = streaming
	u.snapshotOn = snapshotOn
	cm.mu.Unlock()
	cm.syncCameraStates()
}

// setUnitResolution 首次解析出某路画面的分辨率时更新并广播 device_state。
func (cm *cameraManager) setUnitResolution(u *camUnit, width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	cm.mu.Lock()
	if u.width == width && u.height == height {
		cm.mu.Unlock()
		return
	}
	u.width = width
	u.height = height
	cm.mu.Unlock()
	cm.syncCameraStates()
}

// runUnit 启动一路画面：先起快照降级，再进入 RTSP 重试循环。
// 同一 unit 同时只允许一个循环在跑：断流回调若恰逢循环仍在运行（例如
// 被并发循环关闭旧连接触发），直接跳过，否则会指数级派生 RTSP 连接。
func (cm *cameraManager) runUnit(u *camUnit, life *streamLife, address string) {
	cm.mu.Lock()
	if u.loopRunning {
		cm.mu.Unlock()
		log.Printf("[%s] unit loop already running, skip re-entry", u.token)
		return
	}
	u.loopRunning = true
	u.snapLife = newStreamLife()
	snapStop := u.snapLife.stopCh
	cm.mu.Unlock()
	defer func() {
		cm.mu.Lock()
		u.loopRunning = false
		cm.mu.Unlock()
	}()

	if u.snapshotURL != "" {
		go func() {
			if err := cm.fetchAndShowSnapshot(u, u.snapshotURL); err != nil {
				log.Printf("Initial snapshot(%s) fetch failed: %v", u.token, err)
			}
		}()
		go cm.startSnapshotLoop(u, u.snapshotURL, snapStop)
		cm.setUnitMode(u, false, true)
	}

	cm.runUnitRTSPLoop(u, life, address)
}

// runUnitRTSPLoop 对一路画面持续重试 RTSP；成功后停掉该路快照降级，
// 断流时自动恢复快照并重试。
func (cm *cameraManager) runUnitRTSPLoop(u *camUnit, life *streamLife, address string) {
	if u.rtspURL == "" {
		return // 该路无取流地址，仅快照
	}

	isFirst := cm.firstUnitIs(u)

	const maxAttempts = 12
	const retryWait = 5 * time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !cm.isCurrent(life) {
			return
		}

		stream := rtsp.NewStream(u.rtspURL)
		cam := u.token
		stream.OnVideoNAL(func(nalu []byte) {
			cm.hub.BroadcastVideoNAL(cam, nalu)
		})
		// MJPEG（RFC 2435）帧为完整 JPEG，复用快照通道推给浏览器
		stream.OnVideoJPEG(func(jpeg []byte) {
			cm.hub.BroadcastVideoJPEG(cam, jpeg)
		})
		// 分辨率（H.264 SDP/带内 SPS、MJPEG JPEG SOF）解析出即上报
		stream.OnVideoResolution(func(width, height int) {
			cm.setUnitResolution(u, width, height)
		})
		stream.OnAudioPCM(func(pcm []byte, sampleRate, channels int) {
			// 音频按设备级处理：只取第一路，供浏览器播放与语音识别
			if !cm.firstUnitIs(u) {
				return
			}
			cm.hub.BroadcastAudioPCM(pcm, sampleRate, channels)

			cm.mu.Lock()
			cm.cameraAudioRate = sampleRate
			// 解码器重建后恢复出声：清除降级标记并广播一次
			var recovered *server.AudioState
			if cm.audioState != nil && cm.audioState.Degraded {
				r := *cm.audioState
				r.Degraded = false
				r.Reason = ""
				cm.audioState = &r
				recovered = &r
			}
			if cm.cameraAudioBufMax > 0 {
				cm.cameraAudioBuf = append(cm.cameraAudioBuf, pcm...)
				if len(cm.cameraAudioBuf) > cm.cameraAudioBufMax {
					excess := len(cm.cameraAudioBuf) - cm.cameraAudioBufMax
					cm.cameraAudioBuf = cm.cameraAudioBuf[excess:]
				}
			}
			cm.mu.Unlock()
			if recovered != nil {
				cm.handler.SetAudio(recovered)
			}
		})

		cm.mu.Lock()
		if u.stream != nil {
			u.stream.Close()
		}
		u.stream = stream
		cm.mu.Unlock()

		err := stream.Connect()
		if err == nil {
			log.Printf("[%s] RTSP stream connected", u.token)
			cm.mu.Lock()
			u.snapLife.stop()
			u.mjpeg = stream.IsMJPEG()
			cm.mu.Unlock()
			cm.setUnitMode(u, true, false)

			// 首路画面顺带建立音频回传通道
			if isFirst {
				cm.startBackchannel(u)

				// 设备级音频状态：协商结果即刻可见（无音频也明确告知），
				// 解码降级时更新，后续 PCM 恢复到达则视为自愈
				audioSt := &server.AudioState{Available: false}
				if info := stream.AudioTrack(); info != nil {
					audioSt = &server.AudioState{
						Available:  true,
						Codec:      info.Codec,
						SampleRate: info.SampleRate,
						Channels:   info.Channels,
					}
				}
				cm.setAudioState(audioSt)
				stream.OnAudioDegraded(func(reason string) {
					cm.mu.Lock()
					cur := cm.audioState
					if cur == nil || !cur.Available || cur.Degraded {
						cm.mu.Unlock()
						return
					}
					r := *cur
					r.Degraded = true
					r.Reason = reason
					cm.audioState = &r
					cm.mu.Unlock()
					cm.handler.SetAudio(&r)
				})
			}

			stream.WatchDisconnect(func() {
				if !cm.isCurrent(life) {
					return
				}
				log.Printf("[%s] RTSP stream disconnected — reconnecting", u.token)
				cm.setUnitMode(u, false, u.snapshotURL != "")
				cm.resetAudioState(u)
				time.Sleep(2 * time.Second)
				if cm.isCurrent(life) && cm.ownsUnit(u) && !cm.unitLoopBusy(u) {
					cm.runUnit(u, life, address)
				}
			})
			return
		}

		log.Printf("[%s] RTSP stream attempt %d/%d failed: %v", u.token, attempt, maxAttempts, err)
		if attempt == 1 && u.snapshotURL == "" {
			cm.hub.BroadcastError("RTSP 暂不可用（" + err.Error() + "），自动重试中")
		}
		if attempt == maxAttempts {
			log.Printf("[%s] RTSP stream giving up after %d attempts", u.token, maxAttempts)
			cm.setUnitMode(u, false, u.snapshotURL != "")
			cm.resetAudioState(u)
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

// firstUnitIs 判断 u 是否当前连接的第一路画面（用于设备级音频归属）。
func (cm *cameraManager) firstUnitIs(u *camUnit) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return len(cm.units) > 0 && cm.units[0] == u
}

// setAudioState 更新设备级音频状态并立即广播（device_state 携带）。
func (cm *cameraManager) setAudioState(st *server.AudioState) {
	cm.mu.Lock()
	cm.audioState = st
	cm.mu.Unlock()
	cm.handler.SetAudio(st)
}

// resetAudioState 在承载设备级音频的画面断流/放弃时把音频状态置回未知。
func (cm *cameraManager) resetAudioState(u *camUnit) {
	if !cm.firstUnitIs(u) {
		return
	}
	cm.setAudioState(nil)
}

// startBackchannel 为首路画面建立对讲回传通道：先关掉旧连接（断流重连
// 场景避免泄漏），状态置为“协商中”，连接结果异步更新并广播给前端。
func (cm *cameraManager) startBackchannel(u *camUnit) {
	cm.mu.Lock()
	if cm.backchannel != nil {
		cm.backchannel.Close()
		cm.backchannel = nil
	}
	// 换新通道时旧对讲会话即刻失效，避免写到已关闭的连接上
	cm.talkbackActive = false
	bc := rtsp.NewBackchannel(u.rtspURL)
	cm.backchannel = bc
	cm.mu.Unlock()

	cm.setTalkbackState(&server.TalkbackState{Available: false})

	go func() {
		if err := bc.Connect(); err != nil {
			log.Printf("Audio backchannel unavailable: %v", err)
			reason := "connect_failed"
			if errors.Is(err, rtsp.ErrNoBackchannel) {
				reason = "no_backchannel"
			}
			cm.setTalkbackState(&server.TalkbackState{Available: false, Reason: reason})
			return
		}
		log.Println("Audio backchannel connected")
		cm.setTalkbackState(&server.TalkbackState{Available: true})
	}()
}

// setTalkbackState 更新对讲通道状态并广播（device_state 携带）。
func (cm *cameraManager) setTalkbackState(st *server.TalkbackState) {
	cm.mu.Lock()
	cm.talkbackState = st
	cm.talkbackReady = st != nil && st.Available
	cm.mu.Unlock()
	cm.handler.SetTalkback(st)
}

// beginTalkback 受理浏览器对讲会话（talkback_start）。
// 先到先得：通道不可用、TTS 播报占用或已有对讲会话时拒绝并返回稳定拒绝码。
func (cm *cameraManager) beginTalkback() (bool, string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if !cm.talkbackReady {
		return false, ws.TalkbackRejectNoBackchannel
	}
	if cm.ttsAudioActive {
		return false, ws.TalkbackRejectBusy
	}
	if cm.talkbackActive {
		return false, ws.TalkbackRejectInUse
	}
	cm.talkbackActive = true
	return true, ""
}

// writeTalkbackPCM 把浏览器采集的 16kHz 单声道 PCM16 转发到回传通道。
// 仅会话激活期间转发；写入失败（连接中断等）直接结束会话并告知前端。
func (cm *cameraManager) writeTalkbackPCM(pcm []byte) {
	cm.mu.Lock()
	bc := cm.backchannel
	active := cm.talkbackActive
	cm.mu.Unlock()

	if !active || bc == nil {
		return // 会话已结束，丢弃迟到分片
	}
	if err := bc.WritePCM(pcm); err != nil {
		log.Printf("Talkback write failed: %v", err)
		cm.endTalkback()
		cm.hub.BroadcastError("对讲已中断，请重新按住说话")
	}
}

// endTalkback 结束对讲会话，释放回传通道给 TTS。
func (cm *cameraManager) endTalkback() {
	cm.mu.Lock()
	cm.talkbackActive = false
	cm.mu.Unlock()
}

// speakToBackchannel 用 TTS 语音经回传通道向摄像头端播报。
// 对讲会话占用时跳过（先到先得，浏览器端仍会用 SpeechSynthesis 播放）；
// 播报期间置 ttsAudioActive，新的对讲请求会收到“占用中”。
func (cm *cameraManager) speakToBackchannel(ctx context.Context, ttsClient *tts.Client, text string) {
	cm.mu.Lock()
	if cm.talkbackActive {
		cm.mu.Unlock()
		log.Println("Talkback active, skip TTS backchannel audio")
		return
	}
	cm.ttsAudioActive = true
	bc := cm.backchannel
	cm.mu.Unlock()
	defer func() {
		cm.mu.Lock()
		cm.ttsAudioActive = false
		cm.mu.Unlock()
	}()

	if bc == nil {
		return
	}

	pcmAudio, err := ttsClient.Synthesize(ctx, text)
	if err != nil {
		log.Printf("TTS error (backchannel only, browser uses SpeechSynthesis): %v", err)
		return
	}
	if len(pcmAudio) == 0 {
		return
	}
	log.Printf("TTS audio for backchannel: %d bytes", len(pcmAudio))
	if err := bc.WritePCM(pcmAudio); err != nil {
		log.Printf("Backchannel write error: %v", err)
	}
}

// unitLoopBusy 判断该路的循环是否仍在运行（防重入检查用）。
func (cm *cameraManager) unitLoopBusy(u *camUnit) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return u.loopRunning
}

// ownsUnit 判断 u 是否仍属于当前连接（未被新连接替换）。
func (cm *cameraManager) ownsUnit(u *camUnit) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, x := range cm.units {
		if x == u {
			return true
		}
	}
	return false
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

func (cm *cameraManager) fetchAndShowSnapshot(u *camUnit, snapshotURL string) error {
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

	// 首帧快照即可解析出分辨率（快照模式也能展示分辨率角标）
	if w, h := rtsp.JPEGResolution(jpeg); w > 0 {
		cm.setUnitResolution(u, w, h)
	}

	cm.hub.BroadcastVideoJPEG(u.token, jpeg)
	return nil
}

func (cm *cameraManager) startSnapshotLoop(u *camUnit, snapshotURL string, stopCh chan struct{}) {
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

		if err := cm.fetchAndShowSnapshot(u, snapshotURL); err != nil {
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
				processLLMResponseWithHistory(cm.llmClient, cm.ttsClient, hub, cm, prompt, &cm.history)
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
		func(camera, direction string) {
			cm.handlePTZMove(camera, direction)
		},
		func(mode string) {
			log.Printf("Audio mode: %s", mode)
		},
	)
	// 对讲（浏览器麦克风 → 摄像头扬声器）会话回调
	h.SetTalkbackCallbacks(
		cm.beginTalkback,
		cm.writeTalkbackPCM,
		cm.endTalkback,
	)
	hub.BroadcastStatus(ws.StatusIdle)
}

func (cm *cameraManager) handlePTZMove(camera, direction string) {
	cm.mu.Lock()
	client := cm.onvifClient
	var target *camUnit
	for _, u := range cm.units {
		if camera != "" && u.token == camera {
			target = u
			break
		}
	}
	if target == nil && len(cm.units) > 0 {
		target = cm.units[0] // 未指定时回退到第一路
	}
	cm.mu.Unlock()

	if client == nil || target == nil {
		log.Println("PTZ: no camera connected")
		cm.hub.BroadcastError("云台控制需要先连接摄像头")
		return
	}
	if !target.ptz {
		cm.hub.BroadcastError("该摄像头不支持云台控制")
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

	if err := client.PTZContinuousMove(ctx, target.token, pan, tilt, zoom, 2*time.Second); err != nil {
		log.Printf("PTZ move %s(%s) failed: %v", direction, target.token, err)
		cm.hub.BroadcastError("云台转动失败: " + err.Error())
		return
	}

	log.Printf("PTZ: moved %s (%s)", direction, target.token)
}

func (cm *cameraManager) processCameraAudio() {
	cm.mu.Lock()
	buf := make([]byte, len(cm.cameraAudioBuf))
	copy(buf, cm.cameraAudioBuf)
	cm.cameraAudioBuf = nil
	audioRate := cm.cameraAudioRate
	cm.mu.Unlock()

	if audioRate <= 0 {
		audioRate = audio.G711SampleRate
	}

	if len(buf) == 0 {
		log.Println("No camera audio buffered")
		cm.hub.BroadcastError("摄像头没有缓冲到音频数据")
		return
	}

	ctx := context.Background()
	wavData := audio.PCMToWAV(buf, audioRate)
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

	processLLMResponseWithHistory(cm.llmClient, cm.ttsClient, cm.hub, cm, text, &cm.history)
}

func (cm *cameraManager) disconnect() {
	cm.mu.Lock()
	life := cm.life
	cm.life = nil
	for _, u := range cm.units {
		if u.stream != nil {
			u.stream.Close()
			u.stream = nil
		}
		u.streaming = false
		u.snapshotOn = false
		u.snapLife.stop()
	}
	cm.units = nil
	cm.cameraAudioBuf = nil
	cm.cameraAudioRate = 0
	cm.audioState = nil
	cm.talkbackActive = false
	cm.talkbackState = nil
	cm.talkbackReady = false
	if cm.backchannel != nil {
		cm.backchannel.Close()
		cm.backchannel = nil
	}
	cm.mu.Unlock()

	life.stop()
	cm.handler.SetCameras(nil)
	cm.handler.SetAudio(nil)
	cm.handler.SetTalkback(nil)
	cm.handler.SetDeviceState(false, false, false, "未连接")
	cm.hub.BroadcastStatus(ws.StatusIdle)
}

// processLLMResponseWithHistory 走完整语音问答链路：LLM 流式回复 →
// 浏览器字幕 + SpeechSynthesis 播报 + 回传通道 TTS（对讲互斥，见
// cameraManager.speakToBackchannel）。
func processLLMResponseWithHistory(llmClient *llm.Client, ttsClient *tts.Client, hub *ws.Hub, cm *cameraManager, prompt string, history *[]llm.Message) {
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

	cm.speakToBackchannel(ctx, ttsClient, fullText)

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
