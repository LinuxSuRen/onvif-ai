package forward

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// startRTSPServer 在随机端口起一个内存 RTSP 服务器。gortsplib 不回报实际
// 监听地址，沿用既有测试的做法：先占一个空闲端口再交给服务器。
func startRTSPServer(t *testing.T, handler gortsplib.ServerHandler) (*gortsplib.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := &gortsplib.Server{Handler: handler, RTSPAddress: addr}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, addr
}

// pickFreePort 取一个当前空闲的端口（构造「目标离线」地址用）。
func pickFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// sourceServer 模拟摄像头：向所有读者提供一个媒体流。转发链路对编码
// 不敏感（RTP 直通），选 G.711 免去 SPS 等参数集构造。
type sourceServer struct {
	stream *gortsplib.ServerStream
}

func (s *sourceServer) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *sourceServer) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *sourceServer) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// publishLoop 以 50pps 持续向源流写裸 RTP 包。转发不解析载荷，
// 任意字节即可满足「有数据在流」的验证需求。
func publishLoop(stop <-chan struct{}, stream *gortsplib.ServerStream, medi *description.Media) {
	var seq uint16
	var ts uint32
	for {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    8,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           0x1234,
			},
			Payload: []byte{0x55, 0x55},
		}
		_ = stream.WritePacketRTP(medi, pkt)
		seq++
		ts += 160
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// targetServer 模拟 mediamtx：接受 ANNOUNCE/RECORD，记录推上来的流路径
// 与收到的 RTP（测试断言用）。
type targetServer struct {
	mu       sync.Mutex
	paths    []string
	gotRTP   chan struct{}
	sessOnce sync.Once
	closed   chan struct{}
}

func newTargetServer() *targetServer {
	return &targetServer{
		gotRTP: make(chan struct{}, 32),
		closed: make(chan struct{}),
	}
}

func (s *targetServer) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	s.mu.Lock()
	s.paths = append(s.paths, ctx.Request.URL.Path)
	s.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *targetServer) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil, nil
}

func (s *targetServer) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	ctx.Session.OnPacketRTPAny(func(_ *description.Media, _ format.Format, _ *rtp.Packet) {
		select {
		case s.gotRTP <- struct{}{}:
		default:
		}
	})
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *targetServer) OnSessionClose(*gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.sessOnce.Do(func() { close(s.closed) })
}

func (s *targetServer) announcedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// waitFor 轮询断言直到条件满足或超时（转发与重试都是异步的）。
func waitFor(t *testing.T, timeout time.Duration, msg string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout waiting: " + msg)
}

// newTestSource 起一台模拟摄像头并持续发包，返回源流地址。
func newTestSource(t *testing.T, path string) string {
	t.Helper()
	g711 := &format.G711{PayloadTyp: 8, SampleRate: 8000, ChannelCount: 1}
	medi := &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{g711}}
	desc := &description.Session{Medias: []*description.Media{medi}}

	src := &sourceServer{}
	srv, addr := startRTSPServer(t, src)

	stream := &gortsplib.ServerStream{Server: srv, Desc: desc}
	if err := stream.Initialize(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stream.Close)
	src.stream = stream

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go publishLoop(stop, stream, medi)

	return "rtsp://" + addr + path
}

// TestForwarderEndToEnd 端到端验证「内存摄像头 → Manager → 内存 mediamtx」：
// 源媒体轨被原样发布到稳定路径并持续送达 RTP；移除设备后推送停止。
func TestForwarderEndToEnd(t *testing.T) {
	sourceURL := newTestSource(t, "/cam")

	tgt := newTargetServer()
	_, tgtAddr := startRTSPServer(t, tgt)

	m := NewManager(Config{
		Enabled:          true,
		TargetURL:        "rtsp://" + tgtAddr,
		DiscoverInterval: time.Hour, // 关闭发现循环干扰
	})
	defer m.Close()

	// 设备地址与 profile 名模拟真实摄像头；路径应为 onvif-ai/<host>/<名>
	m.SetDeviceSources("http://192.168.1.21:80/onvif/device_service", []Source{{
		DeviceAddr: "http://192.168.1.21:80/onvif/device_service",
		Token:      "Profile_1",
		Name:       "Front Door",
		RTSPURL:    sourceURL,
	}})

	waitFor(t, 15*time.Second, "RTP 到达流媒体服务器", func() bool {
		return len(tgt.gotRTP) > 0
	})

	// 目标收到的 ANNOUNCE 路径必须与设计一致（稳定、可读、已清洗）
	waitFor(t, 5*time.Second, "状态进入 running", func() bool {
		for _, st := range m.Status() {
			if st.Path == "onvif-ai/192.168.1.21/front-door" && st.State == StateRunning {
				return true
			}
		}
		return false
	})
	paths := tgt.announcedPaths()
	if len(paths) == 0 || paths[0] != "/onvif-ai/192.168.1.21/front-door" {
		t.Fatalf("announced paths = %v", paths)
	}

	// 移除设备（地址形态与登记时不同，验证键归一化）：推送必须停止
	m.RemoveDevice("192.168.1.21:80/onvif/device_service")
	select {
	case <-tgt.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("移除设备后目标会话未关闭")
	}
	if sts := m.Status(); len(sts) != 0 {
		t.Fatalf("移除设备后仍有转发残留: %+v", sts)
	}
}

// TestForwarderRetryWhenTargetOffline 验证流媒体服务器离线场景：
// 先在 retrying 状态退避重试，目标上线后自动恢复推送。
func TestForwarderRetryWhenTargetOffline(t *testing.T) {
	// 缩短退避，避免测试真实等待 5s+
	oldInit, oldMax := retryInitialBackoff, retryMaxBackoff
	retryInitialBackoff, retryMaxBackoff = 100*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { retryInitialBackoff, retryMaxBackoff = oldInit, oldMax })

	sourceURL := newTestSource(t, "/cam")

	m := NewManager(Config{
		Enabled:          true,
		TargetURL:        "rtsp://" + pickFreePort(t), // 目标离线
		DiscoverInterval: time.Hour,
	})
	defer m.Close()

	m.SetDeviceSources("192.168.1.21", []Source{{
		DeviceAddr: "192.168.1.21",
		Token:      "p1",
		Name:       "cam",
		RTSPURL:    sourceURL,
	}})

	// 离线期间保持重试态并携带最近错误
	waitFor(t, 5*time.Second, "进入 retrying", func() bool {
		for _, st := range m.Status() {
			if st.State == StateRetrying && st.LastError != "" {
				return true
			}
		}
		return false
	})

	// 目标上线，自动恢复
	tgt := newTargetServer()
	_, tgtAddr := startRTSPServer(t, tgt)
	m.UpdateConfig(Config{
		Enabled:          true,
		TargetURL:        "rtsp://" + tgtAddr,
		DiscoverInterval: time.Hour,
	})

	waitFor(t, 10*time.Second, "目标上线后恢复推送", func() bool {
		return len(tgt.gotRTP) > 0
	})
	waitFor(t, 5*time.Second, "恢复后状态 running", func() bool {
		for _, st := range m.Status() {
			if st.State == StateRunning {
				return true
			}
		}
		return false
	})
}
