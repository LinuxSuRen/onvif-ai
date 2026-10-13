package ws

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// newDirectClient 直接构造并入册一个客户端（不经 register 通道，
// 适合不需要 Run 循环的同步测试）；send 缓冲远大于测试消息量，
// 不会触发慢客户端移除路径。
func newDirectClient(h *Hub) *Client {
	c := &Client{hub: h, send: make(chan []byte, 16)}
	h.clients[c] = true
	return c
}

// expectReceived 断言通道里恰好有一条消息并返回它（带超时，用于
// 异步投递的正向断言）。
func expectReceived(t *testing.T, ch <-chan []byte) Message {
	t.Helper()
	select {
	case raw := <-ch:
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal message: %v", err)
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("expected a message but got none")
		return Message{}
	}
}

// expectNothing 断言通道为空。settle 等待 Run 循环完成当前投递轮次，
// 之后 default 分支即最终结论（投递不再有新消息进入）。
func expectNothing(t *testing.T, label string, ch <-chan []byte) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	select {
	case raw := <-ch:
		t.Fatalf("%s should not receive anything, got %s", label, raw)
	default:
	}
}

// TestDispatchVideoFiltering 同步校验视频过滤的核心语义：
// 未请求画面的连接不收视频帧、start 后收、stop 后停，
// 且非视频消息始终全员投递不受影响。
func TestDispatchVideoFiltering(t *testing.T) {
	hub := NewHub()
	viewer := newDirectClient(hub)
	bystander := newDirectClient(hub)

	nal, _ := json.Marshal(&Message{Type: MsgTypeVideoNAL, Cam: "cam1"})
	status, _ := json.Marshal(&Message{Type: MsgTypeStatus})

	// 默认（未 start）：视频不投递，两个连接都不收
	hub.dispatch(nal, true)
	for name, c := range map[string]*Client{"viewer": viewer, "bystander": bystander} {
		select {
		case raw := <-c.send:
			t.Fatalf("%s received video before start: %s", name, raw)
		default:
		}
	}

	// 非视频消息不受过滤影响：全员送达
	hub.dispatch(status, false)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeStatus {
		t.Fatalf("viewer expected status, got %s", m.Type)
	}
	if m := expectReceived(t, bystander.send); m.Type != MsgTypeStatus {
		t.Fatalf("bystander expected status, got %s", m.Type)
	}

	// start 后：仅 viewer 收视频
	hub.SetVideoView(viewer, true)
	hub.dispatch(nal, true)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeVideoNAL || m.Cam != "cam1" {
		t.Fatalf("viewer expected video_nal of cam1, got %+v", m)
	}
	select {
	case raw := <-bystander.send:
		t.Fatalf("bystander received video after viewer start: %s", raw)
	default:
	}

	// stop 后：viewer 不再收视频，但非视频消息照常
	hub.SetVideoView(viewer, false)
	hub.dispatch(nal, true)
	select {
	case raw := <-viewer.send:
		t.Fatalf("viewer received video after stop: %s", raw)
	default:
	}
	hub.dispatch(status, false)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeStatus {
		t.Fatalf("viewer expected status after stop, got %s", m.Type)
	}
}

// TestVideoBroadcastRouting 端到端校验 BroadcastVideoNAL/BroadcastVideoJPEG
// 经 Run 循环的实际路由：只送达已 start 的连接，其余连接（含新连接）
// 默认不推送（issue #26）。
func TestVideoBroadcastRouting(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	viewer := newDirectClient(hub)
	bystander := newDirectClient(hub)
	latecomer := newDirectClient(hub)

	nalu := []byte{0x00, 0x00, 0x00, 0x01, 0x65}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	// 只有 viewer 打开画面
	hub.SetVideoView(viewer, true)

	hub.BroadcastVideoNAL("cam1", nalu)
	m := expectReceived(t, viewer.send)
	if m.Type != MsgTypeVideoNAL || m.Cam != "cam1" {
		t.Fatalf("viewer expected video_nal of cam1, got %+v", m)
	}
	if data, err := base64.StdEncoding.DecodeString(m.Data); err != nil || string(data) != string(nalu) {
		t.Fatalf("video_nal payload mismatch: %v (%x)", err, data)
	}
	expectNothing(t, "bystander", bystander.send)
	expectNothing(t, "latecomer", latecomer.send)

	hub.BroadcastVideoJPEG("cam1", jpeg)
	m = expectReceived(t, viewer.send)
	if m.Type != MsgTypeVideoJPEG || m.Cam != "cam1" {
		t.Fatalf("viewer expected video_jpeg of cam1, got %+v", m)
	}
	expectNothing(t, "bystander", bystander.send)
	expectNothing(t, "latecomer", latecomer.send)

	// 后来者 start 后同样收到；先开者 stop 后不再收到
	hub.SetVideoView(latecomer, true)
	hub.SetVideoView(viewer, false)
	hub.BroadcastVideoNAL("cam1", nalu)
	if m := expectReceived(t, latecomer.send); m.Type != MsgTypeVideoNAL {
		t.Fatalf("latecomer expected video_nal after start, got %s", m.Type)
	}
	expectNothing(t, "viewer", viewer.send)
	expectNothing(t, "bystander", bystander.send)
}

// TestViewControlMessageContract 校验 view_control 消息契约：类型常量
// 与负载的 JSON 序列化前后端一致。
func TestViewControlMessageContract(t *testing.T) {
	if string(MsgTypeViewControl) != "view_control" {
		t.Fatalf("unexpected message type: %s", MsgTypeViewControl)
	}

	b, err := json.Marshal(ViewControlPayload{Action: ViewActionStart})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"action":"start"}` {
		t.Fatalf("unexpected start payload: %s", b)
	}

	var p ViewControlPayload
	if err := json.Unmarshal([]byte(`{"action":"stop"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Action != ViewActionStop {
		t.Fatalf("unexpected round-trip: %+v", p)
	}
}
