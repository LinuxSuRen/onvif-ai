package server

import (
	"encoding/base64"
	"encoding/json"
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
func sendViewControlWithBarrier(t *testing.T, conn *websocket.Conn, action string) {
	t.Helper()
	wsSend(t, conn, &ws.Message{
		Type:    ws.MsgTypeViewControl,
		Payload: mustMarshal(ws.ViewControlPayload{Action: action}),
	})
	wsSend(t, conn, &ws.Message{
		Type:    ws.MsgTypeClockSync,
		Payload: mustMarshal(map[string]int64{"t0": 1}),
	})
	if m := wsRecv(t, conn); m.Type != ws.MsgTypeClockSync {
		t.Fatalf("expected clock_sync barrier echo, got %s", m.Type)
	}
}

// TestViewControlRouting 端到端校验 view_control 的路由语义（issue #26）：
// start 后视频帧只送达该连接、非视频消息仍全员广播、
// stop 后停止推送、新连接默认不推视频。
func TestViewControlRouting(t *testing.T) {
	hub := ws.NewHub()
	go hub.Run()
	h := NewHandler(hub, nil)

	viewer := dialTestWS(t, h)
	defer viewer.Close()

	nalu := []byte{0x00, 0x00, 0x00, 0x01, 0x65}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	// start 后：NAL 与 JPEG 都送达，且字段无损
	sendViewControlWithBarrier(t, viewer, ws.ViewActionStart)
	hub.BroadcastVideoNAL("cam1", nalu)
	m := wsRecv(t, viewer)
	if m.Type != ws.MsgTypeVideoNAL || m.Cam != "cam1" {
		t.Fatalf("expected video_nal of cam1, got %+v", m)
	}
	if data, err := base64.StdEncoding.DecodeString(m.Data); err != nil || string(data) != string(nalu) {
		t.Fatalf("video_nal payload mismatch: %v (%x)", err, data)
	}
	hub.BroadcastVideoJPEG("cam1", jpeg)
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
	hub.BroadcastVideoNAL("cam1", nalu)
	if m := wsRecv(t, viewer); m.Type != ws.MsgTypeVideoNAL {
		t.Fatalf("viewer expected video_nal, got %s", m.Type)
	}
	wsExpectSilence(t, bystander, "bystander without start")

	// stop 后：不再收视频
	sendViewControlWithBarrier(t, viewer, ws.ViewActionStop)
	hub.BroadcastVideoJPEG("cam1", jpeg)
	wsExpectSilence(t, viewer, "viewer after stop")
}
