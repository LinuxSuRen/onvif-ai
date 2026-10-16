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

// TestDispatchDropToKey 丢到关键帧策略：send 满仓丢一次视频帧后，
// 该路非关键帧持续被丢弃（画面冻结），关键帧送达并解除丢弃。
func TestDispatchDropToKey(t *testing.T) {
	hub := NewHub()
	viewer := newDirectClient(hub)
	route := VideoRouteKey("dev1", "cam1")
	hub.SetVideoView(viewer, "dev1", "cam1", true)

	frag, _ := json.Marshal(&Message{Type: MsgTypeVideoFMP4, Dev: "dev1", Cam: "cam1", Kind: "frag"})

	// 灌满 send 缓冲后投递非关键帧 → 入队失败进入丢帧模式
	for len(viewer.send) < cap(viewer.send) {
		viewer.send <- []byte("x")
	}
	hub.dispatch(frag, route, false)
	// 消化缓冲
	for len(viewer.send) > 0 {
		<-viewer.send
	}

	// 丢帧期内：非关键帧被跳过
	hub.dispatch(frag, route, false)
	expectNothing(t, "viewer during skip", viewer.send)

	// 关键帧送达并解除丢帧
	hub.dispatch(frag, route, true)
	m := expectReceived(t, viewer.send)
	if m.Type != MsgTypeVideoFMP4 {
		t.Fatalf("expected key frame delivered, got %+v", m)
	}

	// 解除后非关键帧恢复送达
	hub.dispatch(frag, route, false)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeVideoFMP4 {
		t.Fatalf("expected p-frame delivered after recovery, got %+v", m)
	}
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
// 未订阅画面的连接不收视频帧、start 后收、stop 后停，
// 且非视频消息始终全员投递不受影响。
func TestDispatchVideoFiltering(t *testing.T) {
	hub := NewHub()
	viewer := newDirectClient(hub)
	bystander := newDirectClient(hub)
	route := VideoRouteKey("dev1", "cam1")

	nal, _ := json.Marshal(&Message{Type: MsgTypeVideoNAL, Dev: "dev1", Cam: "cam1"})
	status, _ := json.Marshal(&Message{Type: MsgTypeStatus})

	// 默认（未 start）：视频不投递，两个连接都不收
	hub.dispatch(nal, route, true)
	for name, c := range map[string]*Client{"viewer": viewer, "bystander": bystander} {
		select {
		case raw := <-c.send:
			t.Fatalf("%s received video before start: %s", name, raw)
		default:
		}
	}

	// 非视频消息不受过滤影响：全员送达
	hub.dispatch(status, "", false)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeStatus {
		t.Fatalf("viewer expected status, got %s", m.Type)
	}
	if m := expectReceived(t, bystander.send); m.Type != MsgTypeStatus {
		t.Fatalf("bystander expected status, got %s", m.Type)
	}

	// start 后：仅 viewer 收视频
	hub.SetVideoView(viewer, "dev1", "cam1", true)
	hub.dispatch(nal, route, true)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeVideoNAL || m.Cam != "cam1" {
		t.Fatalf("viewer expected video_nal of cam1, got %+v", m)
	}
	select {
	case raw := <-bystander.send:
		t.Fatalf("bystander received video after viewer start: %s", raw)
	default:
	}

	// stop 后：viewer 不再收视频，但非视频消息照常
	hub.SetVideoView(viewer, "dev1", "cam1", false)
	hub.dispatch(nal, route, true)
	select {
	case raw := <-viewer.send:
		t.Fatalf("viewer received video after stop: %s", raw)
	default:
	}
	hub.dispatch(status, "", false)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeStatus {
		t.Fatalf("viewer expected status after stop, got %s", m.Type)
	}
}

// TestVideoBroadcastRouting 端到端校验 BroadcastVideoNAL/BroadcastVideoJPEG
// 经 Run 循环的实际路由：只送达已订阅该画面的连接，其余连接（含新连接）
// 默认不推送（issue #26）。
func TestVideoBroadcastRouting(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	viewer := newDirectClient(hub)
	bystander := newDirectClient(hub)
	latecomer := newDirectClient(hub)

	nalu := []byte{0x00, 0x00, 0x00, 0x01, 0x65}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	// 只有 viewer 订阅 dev1/cam1
	hub.SetVideoView(viewer, "dev1", "cam1", true)

	hub.BroadcastVideoNAL("dev1", "cam1", nalu)
	m := expectReceived(t, viewer.send)
	if m.Type != MsgTypeVideoNAL || m.Cam != "cam1" || m.Dev != "dev1" {
		t.Fatalf("viewer expected video_nal of dev1/cam1, got %+v", m)
	}
	if data, err := base64.StdEncoding.DecodeString(m.Data); err != nil || string(data) != string(nalu) {
		t.Fatalf("video_nal payload mismatch: %v (%x)", err, data)
	}
	expectNothing(t, "bystander", bystander.send)
	expectNothing(t, "latecomer", latecomer.send)

	hub.BroadcastVideoJPEG("dev1", "cam1", jpeg)
	m = expectReceived(t, viewer.send)
	if m.Type != MsgTypeVideoJPEG || m.Cam != "cam1" {
		t.Fatalf("viewer expected video_jpeg of cam1, got %+v", m)
	}
	expectNothing(t, "bystander", bystander.send)
	expectNothing(t, "latecomer", latecomer.send)

	// 后来者订阅后同样收到；先订阅者 stop 后不再收到
	hub.SetVideoView(latecomer, "dev1", "cam1", true)
	hub.SetVideoView(viewer, "dev1", "cam1", false)
	hub.BroadcastVideoNAL("dev1", "cam1", nalu)
	if m := expectReceived(t, latecomer.send); m.Type != MsgTypeVideoNAL {
		t.Fatalf("latecomer expected video_nal after start, got %s", m.Type)
	}
	expectNothing(t, "viewer", viewer.send)
	expectNothing(t, "bystander", bystander.send)
}

// TestTileLevelRouting 视频墙核心语义（#39）：一条连接可以按画面
//（dev+cam 复合键）精确订阅——只订阅 dev1/cam1 的连接收不到
// dev2/cam1 与 dev1/cam2 的帧；追加订阅后两路都送达。
func TestTileLevelRouting(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	viewer := newDirectClient(hub)
	hub.SetVideoView(viewer, "dev1", "cam1", true)

	nalu := []byte{0x00, 0x00, 0x00, 0x01, 0x65}

	// 未订阅的设备/画面不投递
	hub.BroadcastVideoNAL("dev2", "cam1", nalu)
	hub.BroadcastVideoNAL("dev1", "cam2", nalu)
	expectNothing(t, "viewer unsubscribed routes", viewer.send)

	// 追加订阅 dev2/cam1 后两路各自送达
	hub.SetVideoView(viewer, "dev2", "cam1", true)
	hub.BroadcastVideoNAL("dev1", "cam1", nalu)
	if m := expectReceived(t, viewer.send); m.Dev != "dev1" || m.Cam != "cam1" {
		t.Fatalf("expected dev1/cam1 frame, got %+v", m)
	}
	hub.BroadcastVideoNAL("dev2", "cam1", nalu)
	if m := expectReceived(t, viewer.send); m.Dev != "dev2" || m.Cam != "cam1" {
		t.Fatalf("expected dev2/cam1 frame, got %+v", m)
	}

	// 退订其中一路：另一路继续送达
	hub.SetVideoView(viewer, "dev1", "cam1", false)
	hub.BroadcastVideoNAL("dev1", "cam1", nalu)
	hub.BroadcastVideoNAL("dev2", "cam1", nalu)
	if m := expectReceived(t, viewer.send); m.Dev != "dev2" {
		t.Fatalf("expected only dev2 frame after unsubscribe, got %+v", m)
	}
	expectNothing(t, "viewer after selective unsubscribe", viewer.send)
}

// TestViewControlMessageContract 校验 view_control 消息契约：类型常量
// 与负载的 JSON 序列化前后端一致。
func TestViewControlMessageContract(t *testing.T) {
	if string(MsgTypeViewControl) != "view_control" {
		t.Fatalf("unexpected message type: %s", MsgTypeViewControl)
	}

	b, err := json.Marshal(ViewControlPayload{Action: ViewActionStart, Dev: "192.168.1.10", Cam: "profile_1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"action":"start","dev":"192.168.1.10","cam":"profile_1"}` {
		t.Fatalf("unexpected start payload: %s", b)
	}

	var p ViewControlPayload
	if err := json.Unmarshal([]byte(`{"action":"stop","dev":"192.168.1.10","cam":"profile_1"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Action != ViewActionStop || p.Dev != "192.168.1.10" || p.Cam != "profile_1" {
		t.Fatalf("unexpected round-trip: %+v", p)
	}
}

// TestJPEGClearsSkipUntilKey 锁定 JPEG 路的关键帧语义回归：JPEG 每帧
// 独立可解、Key 恒为 true——丢帧模式下 JPEG 必须照常送达并解除该路的
// 丢帧状态，否则快照/MJPEG 相机在慢客户端一次满仓后永久冻结。
func TestJPEGClearsSkipUntilKey(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	viewer := newDirectClient(hub)
	route := VideoRouteKey("dev1", "cam1")
	hub.SetVideoView(viewer, "dev1", "cam1", true)

	frag, _ := json.Marshal(&Message{Type: MsgTypeVideoFMP4, Dev: "dev1", Cam: "cam1", Kind: "frag"})

	// 灌满 send 缓冲后投递非关键帧 → 进入丢帧模式
	for len(viewer.send) < cap(viewer.send) {
		viewer.send <- []byte("x")
	}
	hub.dispatch(frag, route, false)
	for len(viewer.send) > 0 {
		<-viewer.send
	}
	hub.dispatch(frag, route, false)
	expectNothing(t, "viewer during skip", viewer.send)

	// JPEG 帧必须穿透丢帧模式送达（Key=true），并解除丢帧
	hub.BroadcastVideoJPEG("dev1", "cam1", []byte{0xFF, 0xD8, 0xFF, 0xE0})
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeVideoJPEG {
		t.Fatalf("expected video_jpeg delivered during skip mode, got %+v", m)
	}

	// 解除后普通帧恢复送达
	hub.dispatch(frag, route, false)
	if m := expectReceived(t, viewer.send); m.Type != MsgTypeVideoFMP4 {
		t.Fatalf("expected frag delivered after jpeg recovery, got %+v", m)
	}
}
