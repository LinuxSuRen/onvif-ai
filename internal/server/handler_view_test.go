package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/onvif-ai/internal/ws"
)

// wsSend 序列化并发送一条消息。
func wsSend(t *testing.T, conn *websocket.Conn, msg *ws.Message) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write %s: %v", msg.Type, err)
	}
}

// wsRecv 读取下一条消息（带超时）。
func wsRecv(t *testing.T, conn *websocket.Conn) ws.Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	var msg ws.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	return msg
}

// wsExpectSilence 断言连接在短时间内收不到任何消息。gorilla 的读错误
// 是粘性的（超时后连接不可再读），因此只能作为该连接的最后一次断言。
func wsExpectSilence(t *testing.T, conn *websocket.Conn, label string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, data, err := conn.ReadMessage(); err == nil {
		t.Fatalf("%s should not receive anything, got %s", label, data)
	}
}

// sendViewControlWithBarrier 发送 view_control 后紧跟一条 clock_sync 探测，
// 并等待其回显。ReadPump 按序处理同连接消息，回显到达即证明
// view_control 已被服务端处理完毕，消除与后续广播之间的竞态。
func sendViewControlWithBarrier(t *testing.T, conn *websocket.Conn, action, dev, cam string) {
	t.Helper()
	wsSend(t, conn, &ws.Message{
		Type:    ws.MsgTypeViewControl,
		Payload: mustMarshal(ws.ViewControlPayload{Action: action, Dev: dev, Cam: cam}),
	})
	wsSend(t, conn, &ws.Message{
		Type:    ws.MsgTypeClockSync,
		Payload: mustMarshal(map[string]int64{"t0": 1}),
	})
	if m := wsRecv(t, conn); m.Type != ws.MsgTypeClockSync {
		t.Fatalf("expected clock_sync barrier echo, got %s", m.Type)
	}
}

// TestViewControlRouting 端到端校验 view_control 的路由语义（issue #26
// + #39 tile 级订阅）：start(dev+cam) 后对应画面送达该连接、其它画面
// 与未订阅连接不收、非视频消息仍全员广播、stop 后停止推送。
func TestViewControlRouting(t *testing.T) {
	hub := ws.NewHub()
	go hub.Run()
	h := NewHandler(hub, nil)

	viewer := dialTestWS(t, h)
	defer viewer.Close()

	nalu := []byte{0x00, 0x00, 0x00, 0x01, 0x65}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	// 订阅 dev1/cam1 后：该画面送达，字段无损；未订阅的 dev2 不送达
	sendViewControlWithBarrier(t, viewer, ws.ViewActionStart, "dev1", "cam1")
	hub.BroadcastVideoNAL("dev1", "cam1", nalu)
	m := wsRecv(t, viewer)
	if m.Type != ws.MsgTypeVideoNAL || m.Cam != "cam1" || m.Dev != "dev1" {
		t.Fatalf("expected video_nal of dev1/cam1, got %+v", m)
	}
	if data, err := base64.StdEncoding.DecodeString(m.Data); err != nil || string(data) != string(nalu) {
		t.Fatalf("video_nal payload mismatch: %v (%x)", err, data)
	}
	hub.BroadcastVideoJPEG("dev1", "cam1", jpeg)
	if m := wsRecv(t, viewer); m.Type != ws.MsgTypeVideoJPEG || m.Cam != "cam1" {
		t.Fatalf("expected video_jpeg of cam1, got %+v", m)
	}

	// 新连接默认不推视频，但非视频消息照常全员广播
	bystander := dialTestWS(t, h)
	defer bystander.Close()
	hub.BroadcastStatus(ws.StatusIdle)
	if m := wsRecv(t, bystander); m.Type != ws.MsgTypeStatus {
		t.Fatalf("bystander expected status, got %s", m.Type)
	}
	if m := wsRecv(t, viewer); m.Type != ws.MsgTypeStatus {
		t.Fatalf("viewer expected status, got %s", m.Type)
	}
	hub.BroadcastVideoNAL("dev1", "cam1", nalu)
	if m := wsRecv(t, viewer); m.Type != ws.MsgTypeVideoNAL {
		t.Fatalf("viewer expected video_nal, got %s", m.Type)
	}
	wsExpectSilence(t, bystander, "bystander without start")

	// stop 后：不再收视频
	sendViewControlWithBarrier(t, viewer, ws.ViewActionStop, "dev1", "cam1")
	hub.BroadcastVideoJPEG("dev1", "cam1", jpeg)
	wsExpectSilence(t, viewer, "viewer after stop")
}

// TestWallEndpoints 校验视频墙 REST 契约：connect/disconnect 透传回调
// 参数，devices 返回注入的快照；未注册回调时返回 503。
func TestWallEndpoints(t *testing.T) {
	hub := ws.NewHub()
	go hub.Run()
	h := NewHandler(hub, nil)
	router := h.RegisterRoutes()

	// 未注册回调：503
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/wall/connect",
		strings.NewReader(`{"address":"192.168.1.10"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without callbacks expected 503, got %d", rec.Code)
	}

	type connectReq struct{ addr, user, pass, profile string }
	connects := make(chan connectReq, 2)
	disconnects := make(chan string, 2)
	h.SetWallCallbacks(
		func(addr, user, pass, profile string) { connects <- connectReq{addr, user, pass, profile} },
		func(addr string) { disconnects <- addr },
		func() []ws.WallDevice {
			return []ws.WallDevice{{
				Address: "192.168.1.10",
				State:   "online",
				Cameras: []ws.WallCamera{{Token: "profile_2", Name: "sub", Streaming: true}},
			}}
		},
	)

	// connect：参数透传（含 profile），立即返回 connecting
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/wall/connect",
		strings.NewReader(`{"address":"192.168.1.10","username":"admin","password":"pw","profile":"profile_2"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d, body %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-connects:
		if got.addr != "192.168.1.10" || got.user != "admin" || got.pass != "pw" || got.profile != "profile_2" {
			t.Fatalf("connect callback args: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("connect callback not fired")
	}

	// 非法请求：400
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/wall/connect",
		strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid connect expected 400, got %d", rec.Code)
	}

	// disconnect：参数透传
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/wall/disconnect",
		strings.NewReader(`{"address":"192.168.1.10"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect status = %d", rec.Code)
	}
	select {
	case got := <-disconnects:
		if got != "192.168.1.10" {
			t.Fatalf("disconnect callback arg: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect callback not fired")
	}

	// devices：返回注入的快照
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/wall/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("devices status = %d", rec.Code)
	}
	var payload ws.WallStatePayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Devices) != 1 || payload.Devices[0].Address != "192.168.1.10" ||
		payload.Devices[0].State != "online" || len(payload.Devices[0].Cameras) != 1 {
		t.Fatalf("unexpected devices payload: %+v", payload)
	}
}
