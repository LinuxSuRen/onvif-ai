package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/onvif-ai/internal/audio"
	"github.com/onvif-ai/internal/fmp4mux"
	"github.com/onvif-ai/internal/forward"
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

	portFlag := flag.String("port", "", "HTTP 服务端口（优先级高于 PORT 环境变量）")
	flag.Parse()

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

	// 转发（mediamtx 等）：环境变量只是启动默认值，运行时可经 API 覆盖，
	// 覆盖为内存态、重启回落（见 internal/forward/config.go 注释）
	fwdCfg := forward.ConfigFromEnv()
	fwdMgr := forward.NewManager(fwdCfg)
	defer fwdMgr.Close()

	llmClient := llm.NewClient(llmCfg)
	ttsClient := tts.NewClient(ttsCfg)

	h.SetLLMConfig(&server.LLMConfig{
		BaseURL: llmCfg.BaseURL,
		APIKey:  llmCfg.APIKey,
		Model:   llmCfg.Model,
	})

	fwdAPICfg := server.ForwardConfigFrom(fwdCfg)
	h.SetForwardConfig(&fwdAPICfg)
	h.SetForwardUpdateCallback(func(c server.ForwardConfig) {
		fwdMgr.UpdateConfig(c.ToForward())
		log.Printf("转发配置已更新: enabled=%v target=%s tls=%v autodiscover=%v",
			c.Enabled, c.TargetURL, c.UseTLS, c.AutoDiscover)
	})
	h.SetForwardStatusProvider(fwdMgr.Status)

	cm := &cameraManager{
		hub:           hub,
		llmClient:     llmClient,
		ttsClient:     ttsClient,
		handler:       h,
		fwd:           fwdMgr,
		deviceSession: &deviceSession{},
		registry:      newDeviceRegistry(),
	}

	h.SetOnConnect(func(addr, user, pass string) {
		go cm.connect(addr, user, pass)
	})

	// 视频墙：上墙/下墙/快照（凭证由前端每次携带，后端不落盘）
	h.SetWallCallbacks(
		func(addr, user, pass string) { cm.connectWall(addr, user, pass) },
		func(addr string) { cm.disconnectDevice(addr) },
		cm.wallState,
	)

	h.SetLLMUpdateCallback(func(baseURL, apiKey, model string) {
		cm.llmClient.UpdateConfig(baseURL, apiKey, model)
		log.Printf("LLM config updated: %s, model=%s", baseURL, model)
	})

	setupVoiceCallbacks(h, hub, cm)

	router := h.RegisterRoutes()

	port := *portFlag
	if port == "" {
		port = getEnv("PORT", "8080")
	}
	listener, err := listenWithDrift(port)
	if err != nil {
		log.Fatalf("Server failed: %v", err)
	}
	defer listener.Close()

	log.Printf("Server starting on %s", listener.Addr())
	log.Printf("Open http://localhost%s (前端已内嵌，无需单独启动)", listener.Addr())

	go func() {
		if err := http.Serve(listener, router); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	cm.shutdown()
}

type cameraManager struct {
	hub       *ws.Hub
	llmClient *llm.Client
	ttsClient *tts.Client
	handler   *server.Handler
	// fwd 是流媒体转发管理器；摄像头连接的画面自动登记为转发源
	fwd *forward.Manager

	mu sync.Mutex
	// deviceSession 以匿名嵌入承载「当前设备」的全部状态，字段经结构体
	// 提升仍以 cm.xxx 访问。指针恒非 nil：断开时原地替换为空白会话，
	// 保证任何时刻的晋升访问都安全。多设备开放后由 registry 管理多个
	// 会话（#39 第一步重构，单设备行为保持不变）。
	*deviceSession
	// registry 是设备会话登记表：连接登记、断开注销。当前仅承担生命
	// 周期记账，多设备取流入口在后续 PR 基于它开放。
	registry *deviceRegistry
}

// deviceSession 承载一台 ONVIF 设备连接的全部状态：设备地址与凭证、
// 各路画面单元、事件订阅、对讲/TTS 音频引擎与对话历史。所有字段受
// cameraManager.mu 保护（与重构前的 cameraManager 字段一致）。
type deviceSession struct {
	// addr 是当前连接设备的地址（转发源登记/注销的设备键）
	addr string
	// onvifClient 是设备的 SOAP 客户端
	onvifClient *onvif.Client
	// eventSub 是设备的 ONVIF Event 订阅器（PullPoint 模型），
	// 设备不支持 Event 时静默降级（不影响取流）
	eventSub *onvif.EventSubscriber

	// authUser/authPass 是当前连接设备的凭证(受 mu 保护):SOAP 走
	// WS-Security,RTSP/对讲注入 URL userinfo,HTTP 快照走 Basic
	authUser string
	authPass string

	units       []*camUnit
	backchannel *rtsp.Backchannel
	life        *streamLife
	// state 是视频墙展示用的连接阶段：connecting（建连中）/ online
	//（画面单元已建立）。空值按 connecting 处理。
	state string

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

// isCurrentSession 报告 life 是否仍是该会话的活跃连接代际：会话被拆解
//（teardownSession 将 life 置 nil）或被重连替换后，旧代次的循环据此退出。
func (cm *cameraManager) isCurrentSession(sess *deviceSession, life *streamLife) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return sess != nil && sess.life == life
}

// maxCams 限制单台设备同时拉取的画面路数：带宽与浏览器解码能力有限。
const maxCams = 6

// maxWallStreams 限制视频墙总路数（所有设备之和）：默认 9，可经环境
// 变量 MAX_WALL_STREAMS 覆盖。720p 子码流下该值对应约 5Mbps 上行带宽。
var maxWallStreams = func() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MAX_WALL_STREAMS"))); err == nil && v > 0 {
		return v
	}
	return 9
}()

// connect 以聚焦模式连接设备（详情页入口）：取全部 media profile 并把
// 会话设为当前聚焦；旧聚焦设备（含其上的对讲/TTS 资源）随会话拆除。
func (cm *cameraManager) connect(address, username, password string) {
	cm.mu.Lock()
	focused := cm.deviceSession
	cm.mu.Unlock()
	if focused != nil && focused.addr != "" {
		cm.teardownSession(focused)
	}
	cm.startSession(address, username, password, false)
}

// connectWall 把一台设备上墙（视频墙入口）：只取子码流一路（profile
// 列表末位，主码流在前的设备惯例），不动当前聚焦设备；路数超限时拒绝。
func (cm *cameraManager) connectWall(address, username, password string) {
	cm.mu.Lock()
	total := 0
	for _, s := range cm.registry.Sessions() {
		total += len(s.units)
	}
	_, exists := cm.registry.Get(address)
	cm.mu.Unlock()
	if !exists && total >= maxWallStreams {
		cm.hub.BroadcastError(fmt.Sprintf("视频墙路数已达上限（%d），请先下墙部分设备", maxWallStreams))
		return
	}
	cm.startSession(address, username, password, true)
}

// disconnectDevice 下墙一台设备（视频墙入口）。
func (cm *cameraManager) disconnectDevice(address string) {
	if sess, ok := cm.registry.Get(address); ok {
		cm.teardownSession(sess)
	}
}

// shutdown 拆除全部设备会话（进程退出时调用）。
func (cm *cameraManager) shutdown() {
	for _, sess := range cm.registry.Sessions() {
		cm.teardownSession(sess)
	}
	cm.disconnect()
}

// startSession 建立并运行一台设备的会话。wallMode 为 true 是视频墙上墙：
// 只取子码流一路、不替换聚焦；否则为聚焦连接（详情页），取全部 profile
// 并设为当前聚焦会话。
func (cm *cameraManager) startSession(address, username, password string, wallMode bool) {
	cm.mu.Lock()
	life := newStreamLife()
	sess := &deviceSession{
		addr:     address,
		authUser: username,
		authPass: password,
		life:     life,
		state:    "connecting",
	}
	cm.registry.Register(address, sess)
	if !wallMode || cm.deviceSession.addr == "" {
		// 聚焦连接直接接管详情页；首台设备上墙时顺带聚焦，
		// 详情页不至于空屏
		cm.deviceSession = sess
	}
	cm.mu.Unlock()

	log.Printf("Connecting to camera: %s", address)
	cm.handler.SetDeviceState(true, false, false, address)

	onvifClient := onvif.NewClient(onvif.Config{
		DeviceAddr: address,
		Username:   username,
		Password:   password,
		Timeout:    5 * time.Second,
	})

	cm.mu.Lock()
	sess.onvifClient = onvifClient
	cm.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	profiles, err := cm.getProfilesWithRetry(sess, onvifClient, ctx, life, address)
	if err != nil {
		log.Printf("GetProfiles failed: %v", err)
		cm.handler.SetDeviceState(true, false, false, address)
		cm.hub.BroadcastError("连接摄像头失败: " + err.Error())
		/* 认证类失败另发结构化通知：前端据此展开对应设备详情并
		 * 聚焦凭证输入，引导用户补填/改正用户名密码后重连 */
		var authErr *onvif.AuthError
		if errors.As(err, &authErr) {
			code := ws.ConnectErrAuthRequired
			if authErr.CredentialsProvided {
				code = ws.ConnectErrAuthFailed
			}
			cm.hub.BroadcastConnectError(address, code)
		}
		return
	}

	if wallMode {
		// 视频墙只取子码流一路：GetProfiles 不携带分辨率信息，按「主码
		// 流在前」的设备惯例取列表末位作为子码流近似（#39）
		profiles = profiles[len(profiles)-1:]
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
			/* 取流地址注入凭证:userinfo 形态让 gortsplib 在 401 时自动
			 * 完成 Basic/Digest 重试,对讲回传通道复用同一地址 */
			u.rtspURL = onvif.WithCredentials(uri.URI, username, password)
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
	sess.units = units
	sess.cameraAudioBufMax = 160000
	sess.state = "online"
	cm.mu.Unlock()
	if wallMode {
		cm.broadcastWallState()
	} else {
		cm.syncCameraStates()
	}
	cm.syncForwardSources(sess)

	// 启动 Event 订阅（PullPoint）：设备端实时推送 SignalLoss/Motion/Battery 等，
	// 不支持 Event 的设备静默降级。Motion 事件触发 AI 分析管线。
	cm.mu.Lock()
	if client := sess.onvifClient; client != nil {
		logger := slog.Default()
		sess.eventSub = client.StartEventSubscriber(address, logger, cm.onDeviceEvent)
	}
	cm.mu.Unlock()

	for _, u := range units {
		go cm.runUnit(sess, u, life)
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
	cm.broadcastWallState()
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
func (cm *cameraManager) runUnit(sess *deviceSession, u *camUnit, life *streamLife) {
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
			if err := cm.fetchAndShowSnapshot(sess, u, u.snapshotURL); err != nil {
				log.Printf("Initial snapshot(%s) fetch failed: %v", u.token, err)
			}
		}()
		go cm.startSnapshotLoop(sess, u, u.snapshotURL, snapStop)
		cm.setUnitMode(u, false, true)
	}

	cm.runUnitRTSPLoop(sess, u, life)
}

// runUnitRTSPLoop 对一路画面持续重试 RTSP；成功后停掉该路快照降级，
// 断流时自动恢复快照并重试。
func (cm *cameraManager) runUnitRTSPLoop(sess *deviceSession, u *camUnit, life *streamLife) {
	address := sess.addr
	if u.rtspURL == "" {
		return // 该路无取流地址，仅快照
	}

	isFirst := cm.firstUnitIs(u)

	const maxAttempts = 12
	const retryWait = 5 * time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !cm.isCurrentSession(sess, life) {
			return
		}

		stream := rtsp.NewStream(u.rtspURL)
		cam := u.token
		// H.264 → 服务端 fMP4 封装 → MSE 直播（替代 jmuxer 自制封装：
		// 其输出不合规范，硬解拒绝/软解绿条，见 fmp4mux 包注释）。
		// muxer 在首个关键帧到达且 SPS/PPS 就绪时创建/按需重建
		//（分辨率切换会换 SPS）；每个关键帧附带重发 init 段，
		// 浏览器端管线（重）初始化即可用。
		var mux *fmp4mux.Muxer
		stream.OnVideoFrame(func(au [][]byte, durTicks uint32, key bool) {
			if !key || len(stream.SPS()) == 0 || len(stream.PPS()) == 0 {
				if mux == nil {
					return // 首帧必须是关键帧才有参数集
				}
			} else {
				if mux == nil || mux.SPSChanged(stream.SPS()) {
					m, err := fmp4mux.New(stream.SPS(), stream.PPS())
					if err != nil {
						log.Printf("[%s] fmp4 init failed: %v", cam, err)
						return
					}
					mux = m
				}
			}
			if mux == nil {
				return
			}
			// 每个关键帧前重发 init 段（每 GOP 一次，约 700 字节）：
			// 浏览器端管线（重）初始化（codec 变化、WS 重连、解码器重建）
			// 都要等一份 init 才能开始 append，只在连接建立时发一次会造成
			// 前端 awaitingInit 死锁。
			if key {
				cm.hub.BroadcastVideoFMP4(address, cam, "init", mux.InitSegment(), true, mux.CodecString())
			}
			seg, err := mux.Fragment(au, durTicks, key)
			if err != nil {
				log.Printf("[%s] fmp4 fragment failed: %v", cam, err)
				return
			}
			cm.hub.BroadcastVideoFMP4(address, cam, "frag", seg, key, "")
		})
		// MJPEG（RFC 2435）帧为完整 JPEG，复用快照通道推给浏览器
		stream.OnVideoJPEG(func(jpeg []byte) {
			cm.hub.BroadcastVideoJPEG(address, cam, jpeg)
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
				if !cm.isCurrentSession(sess, life) {
					return
				}
				log.Printf("[%s] RTSP stream disconnected — reconnecting", u.token)
				cm.setUnitMode(u, false, u.snapshotURL != "")
				cm.resetAudioState(u)
				time.Sleep(2 * time.Second)
				if cm.isCurrentSession(sess, life) && cm.ownsUnit(u) && !cm.unitLoopBusy(u) {
					cm.runUnit(sess, u, life)
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

// syncForwardSources 把指定会话的各路画面登记到转发管理器：
// rtspURL 已内嵌凭证，转发直接复用；仅快照降级（无 rtspURL）的画面跳过。
func (cm *cameraManager) syncForwardSources(sess *deviceSession) {
	if cm.fwd == nil || sess == nil {
		return
	}
	cm.mu.Lock()
	addr := sess.addr
	srcs := make([]forward.Source, 0, len(sess.units))
	for _, u := range sess.units {
		if u.rtspURL != "" {
			srcs = append(srcs, forward.Source{
				DeviceAddr: addr,
				Token:      u.token,
				Name:       u.name,
				RTSPURL:    u.rtspURL,
			})
		}
	}
	cm.mu.Unlock()
	cm.fwd.SetDeviceSources(addr, srcs)
}

// getProfilesWithRetry keeps polling the ONVIF endpoint: WiFi cameras are
// often briefly unreachable right after a reboot/drop, and the RTSP-level
// retry loop can only kick in once profiles are known. Authentication
// failures are not transient — retrying them only delays the structured
// connect_error that guides the user to the credential form.
func (cm *cameraManager) getProfilesWithRetry(sess *deviceSession, client *onvif.Client, ctx context.Context, life *streamLife, address string) ([]onvif.Profile, error) {
	const maxAttempts = 12
	const retryWait = 5 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if !cm.isCurrentSession(sess, life) {
			return nil, lastErr
		}
		profiles, err := client.GetProfiles(ctx)
		if err == nil {
			return profiles, nil
		}
		lastErr = err
		log.Printf("GetProfiles attempt %d/%d failed: %v", attempt, maxAttempts, err)
		var authErr *onvif.AuthError
		if errors.As(err, &authErr) {
			return nil, err
		}
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

func (cm *cameraManager) fetchAndShowSnapshot(sess *deviceSession, u *camUnit, snapshotURL string) error {
	if snapshotURL == "" {
		return fmt.Errorf("empty snapshot URL")
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", snapshotURL, nil)
	if err != nil {
		return fmt.Errorf("snapshot request: %w", err)
	}
	/* 设备开启认证时快照端点同样要求登录(HTTP Basic,与 RTSP/ONVIF 同账号) */
	cm.mu.Lock()
	user, pass := sess.authUser, sess.authPass
	cm.mu.Unlock()
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := httpClient.Do(req)
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

	cm.hub.BroadcastVideoJPEG(sess.addr, u.token, jpeg)
	return nil
}

func (cm *cameraManager) startSnapshotLoop(sess *deviceSession, u *camUnit, snapshotURL string, stopCh chan struct{}) {
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

		if err := cm.fetchAndShowSnapshot(sess, u, snapshotURL); err != nil {
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

// disconnect 拆除当前聚焦设备的会话（详情页断开/进程退出兜底）。
func (cm *cameraManager) disconnect() {
	cm.mu.Lock()
	focused := cm.deviceSession
	cm.mu.Unlock()
	if focused != nil && focused.addr != "" {
		cm.teardownSession(focused)
	}
}

// teardownSession 拆解一台设备的会话：停止事件订阅、各路画面与快照循
// 环、对讲回传通道，从 registry 注销并移除转发源；被拆的是聚焦设备时
// 同步清空详情页状态并广播 wall_state。幂等（重连替换后旧引用直接放过）。
func (cm *cameraManager) teardownSession(old *deviceSession) {
	if old == nil {
		return
	}
	cm.mu.Lock()
	if cur, ok := cm.registry.Get(old.addr); !ok || cur != old {
		// 已被重连替换或已拆除；仅当它仍挂为聚焦时清空聚焦
		if cm.deviceSession == old {
			cm.deviceSession = &deviceSession{}
		}
		cm.mu.Unlock()
		return
	}
	cm.registry.Unregister(old.addr)
	wasFocused := cm.deviceSession == old
	life := old.life
	old.life = nil
	addr := old.addr
	old.addr = ""
	if old.eventSub != nil {
		old.eventSub.Stop()
		old.eventSub = nil
	}
	for _, u := range old.units {
		if u.stream != nil {
			u.stream.Close()
			u.stream = nil
		}
		u.streaming = false
		u.snapshotOn = false
		u.snapLife.stop()
	}
	old.units = nil
	old.cameraAudioBuf = nil
	old.cameraAudioRate = 0
	old.authUser = ""
	old.authPass = ""
	old.audioState = nil
	old.talkbackActive = false
	old.talkbackState = nil
	old.talkbackReady = false
	if old.backchannel != nil {
		old.backchannel.Close()
		old.backchannel = nil
	}
	old.state = ""
	if wasFocused {
		cm.deviceSession = &deviceSession{}
	}
	cm.mu.Unlock()

	life.stop()
	// 断开连接即注销该设备的全部转发源（无源可转发时等价于停止转发）
	if cm.fwd != nil && addr != "" {
		cm.fwd.RemoveDevice(addr)
	}
	if wasFocused {
		cm.handler.SetCameras(nil)
		cm.handler.SetAudio(nil)
		cm.handler.SetTalkback(nil)
		cm.handler.SetDeviceState(false, false, false, "未连接")
		cm.hub.BroadcastStatus(ws.StatusIdle)
	}
	cm.broadcastWallState()
}

// wallState 汇总视频墙快照：registry 中每台设备的地址、连接状态与
// 各路画面（上墙设备仅子码流一路）。
func (cm *cameraManager) wallState() []ws.WallDevice {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	sessions := cm.registry.Sessions()
	out := make([]ws.WallDevice, 0, len(sessions))
	for _, s := range sessions {
		wd := ws.WallDevice{Address: s.addr, Cameras: []ws.WallCamera{}}
		wd.State = s.state
		if wd.State == "" {
			wd.State = "connecting"
		}
		for _, u := range s.units {
			wd.Cameras = append(wd.Cameras, ws.WallCamera{
				Token:     u.token,
				Name:      u.name,
				Streaming: u.streaming,
				Snapshot:  u.snapshotOn,
				MJPEG:     u.mjpeg,
				Width:     u.width,
				Height:    u.height,
			})
		}
		out = append(out, wd)
	}
	return out
}

// broadcastWallState 在设备增删或画面状态变化后广播视频墙快照。
func (cm *cameraManager) broadcastWallState() {
	cm.hub.BroadcastWallState(cm.wallState())
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

// listenWithDrift 绑定 HTTP 端口：端口被占用（EADDRINUSE）时默认向后漂移，
// 最多尝试 20 个连续端口；其他错误（如权限不足）直接失败，不做漂移。
func listenWithDrift(port string) (net.Listener, error) {
	start, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("invalid port %q: %w", port, err)
	}

	const maxDrift = 20
	for i := 0; i < maxDrift; i++ {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", start+i))
		if err == nil {
			if i > 0 {
				log.Printf("Port %d in use, drifted to %d", start, start+i)
			}
			return l, nil
		}
		if !isAddrInUse(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("ports %d-%d all in use", start, start+maxDrift-1)
}

// wsaEADDRINUSE 是 Windows 上 bind 冲突的 errno（syscall.WSAEADDRINUSE）。
// 该常量仅存在于 Windows 的 syscall 包，为保持跨平台编译此处用数值。
const wsaEADDRINUSE = 10048

// isAddrInUse 判断监听失败是否为端口占用。
// Windows 上 syscall.EADDRINUSE 是与 WSAEADDRINUSE 不同的占位值，
// bind 冲突实际返回 WSAEADDRINUSE，因此两类错误都要识别，
// 否则端口漂移在 Windows 上永远不生效。
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && uintptr(errno) == wsaEADDRINUSE
}

var _ = audio.PCM16kSampleRate
var _ = base64.StdEncoding
var _ = strings.TrimSpace

// ===================== ONVIF Event 路由 =====================

// onDeviceEvent 是 EventSubscriber 的回调（订阅 goroutine 上执行，不阻塞）：
// 全量广播 device_event 到前端 + Motion 事件触发 AI 分析管线。
func (cm *cameraManager) onDeviceEvent(ev onvif.EventMessage) {
	// 1. 全量推送到前端（前端按 topic 渲染健康徽标/告警横幅/事件面板）
	cm.hub.BroadcastMessage(&ws.Message{
		Type: ws.MsgTypeDeviceEvent,
		Payload: mustJSON(ws.DeviceEventPayload{
			Topic:  ev.Topic,
			Source: ev.Source,
			Data:   ev.Data,
			Device: ev.Device,
			Time:   ev.UTCTime,
		}),
	})

	// 2. Motion State=true → AI 分析管线（快照→LLM→TTS）
	if ev.IsMotion() && ev.Data["State"] == "true" {
		go cm.motionAIPipeline(ev)
	}
}

// motionSnapshotMaxBytes 限制送入 LLM 的快照体积：base64 后约膨胀 4/3，
// 超限直接跳过分析（避免撞 LLM 网关的请求体/token 上限）。
const motionSnapshotMaxBytes = 4 << 20 // 4MB

// motionAIPipeline 移动侦测触发的 AI 分析：
// 抓快照 → 发 LLM 分析画面内容 → TTS 播报到设备扬声器（可用时）。
// 全流程可失败降级（LLM 未配置/TTS 不可用/快照失败——只记日志不中断）。
func (cm *cameraManager) motionAIPipeline(ev onvif.EventMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[motion-ai] pipeline panic: %v", r)
		}
	}()

	// 找 Source Camera_N 对应的 camUnit 取快照 URL
	cm.mu.Lock()
	var snapURL string
	for _, u := range cm.units {
		if u.snapshotURL != "" {
			snapURL = u.snapshotURL
			break
		}
	}
	cm.mu.Unlock()
	if snapURL == "" {
		log.Printf("[motion-ai] no snapshot URL available, skip")
		return
	}

	// 抓快照（带 Basic 认证）
	cm.mu.Lock()
	authUser, authPass := cm.authUser, cm.authPass
	cm.mu.Unlock()
	snapURL = onvif.WithCredentials(snapURL, authUser, authPass)
	jpegData, err := httpGetBinary(snapURL, 5*time.Second)
	if err != nil || len(jpegData) < 100 {
		log.Printf("[motion-ai] snapshot failed: %v (%d bytes)", err, len(jpegData))
		return
	}

	// LLM 分析（多模态消息：文本 + image_url 各占一段 content，
	// base64 放进 data URL——嵌在纯文本里模型看不到画面且易撞请求上限）
	cm.mu.Lock()
	llmCl := cm.llmClient
	cm.mu.Unlock()
	if llmCl == nil {
		return
	}
	if len(jpegData) > motionSnapshotMaxBytes {
		log.Printf("[motion-ai] snapshot too large (%d bytes), skip", len(jpegData))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	analysis, err := llmCl.Chat(ctx, []llm.Message{
		{Role: "user", Content: []llm.ImageContent{
			{Type: "text", Text: "[移动侦测触发] 请用一句话描述这个画面中发生了什么（中文，不超过30字）："},
			{Type: "image_url", ImageURL: &llm.ImageURL{
				URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData),
			}},
		}},
	})
	if err != nil {
		log.Printf("[motion-ai] LLM analysis failed: %v", err)
		return
	}
	analysis = strings.TrimSpace(analysis)
	if analysis == "" {
		return
	}

	// 广播 AI 分析结果到前端（transcript 消息，前端聊天面板展示）
	cm.hub.BroadcastTranscript("[AI 移动侦测] " + analysis)
	log.Printf("[motion-ai] %s", analysis)

	// TTS 播报到设备扬声器（backchannel 可用且无对讲/播报占用时）
	cm.mu.Lock()
	ready := cm.talkbackReady && !cm.talkbackActive && !cm.ttsAudioActive
	ttsClient := cm.ttsClient
	cm.mu.Unlock()
	if ready && ttsClient != nil {
		// TTS 独立生命周期：主管线的 ctx 随本函数返回被 defer cancel，
		// 若沿用会把 http 请求立即取消、合成立即失败
		go func() {
			ttsCtx, ttsCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer ttsCancel()
			cm.speakToBackchannel(ttsCtx, ttsClient, analysis)
		}()
	}
}

// mustJSON 序列化（内部使用，序列化失败返回空 JSON——不应发生）。
func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// httpGetBinary 抓取 URL 内容为 []byte（带超时；用于快照）。
func httpGetBinary(url string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}
