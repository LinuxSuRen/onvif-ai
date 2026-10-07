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

// dialTestWS 对 handler 的 /ws 处理器建立一条真实 WebSocket 连接。
func dialTestWS(t *testing.T, h *Handler) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.handleWebSocket))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

// readTalkbackAck 读取下一条消息并断言其为 talkback_state。
func readTalkbackAck(t *testing.T, conn *websocket.Conn) ws.TalkbackSessionPayload {
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
	if msg.Type != ws.MsgTypeTalkbackState {
		t.Fatalf("expected talkback_state, got %s", msg.Type)
	}
	var p ws.TalkbackSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p
}

// TestTalkbackSessionOverWebSocket 端到端校验对讲会话协议：
// talkback_start 受理回执只发给发起方、audio_in 仅属主转发、
// talkback_stop 释放会话、属主断连自动释放（防悬挂阻塞 TTS）。
func TestTalkbackSessionOverWebSocket(t *testing.T) {
	hub := ws.NewHub()
	go hub.Run()
	h := NewHandler(hub, nil)

	starts, stops := 0, 0
	var gotAudio [][]byte
	acked := make(chan struct{})
	h.SetTalkbackCallbacks(
		func() (bool, string) {
			if starts > 0 {
				return false, ws.TalkbackRejectInUse
			}
			starts++
			return true, ""
		},
		func(pcm []byte) { gotAudio = append(gotAudio, pcm) },
		func() { stops++; close(acked) },
	)

	owner := dialTestWS(t, h)
	defer owner.Close()

	if err := owner.WriteJSON(&ws.Message{Type: ws.MsgTypeTalkbackStart}); err != nil {
		t.Fatalf("write talkback_start: %v", err)
	}
	if p := readTalkbackAck(t, owner); !p.Active || p.Reason != "" {
		t.Fatalf("expected acceptance, got %+v", p)
	}

	// 属主音频被转发
	chunk := []byte{0x01, 0x02, 0x03, 0x04}
	if err := owner.WriteJSON(&ws.Message{
		Type: ws.MsgTypeAudioIn,
		Data: base64.StdEncoding.EncodeToString(chunk),
	}); err != nil {
		t.Fatalf("write audio_in: %v", err)
	}

	// 第二个客户端：会话被拒 + 其音频不被转发
	bystander := dialTestWS(t, h)
	defer bystander.Close()
	if err := bystander.WriteJSON(&ws.Message{Type: ws.MsgTypeTalkbackStart}); err != nil {
		t.Fatalf("bystander talkback_start: %v", err)
	}
	if p := readTalkbackAck(t, bystander); p.Active || p.Reason != ws.TalkbackRejectInUse {
		t.Fatalf("expected in-use rejection, got %+v", p)
	}
	if err := bystander.WriteJSON(&ws.Message{
		Type: ws.MsgTypeAudioIn,
		Data: base64.StdEncoding.EncodeToString(chunk),
	}); err != nil {
		t.Fatalf("bystander audio_in: %v", err)
	}

	// 非属主的 talkback_stop 不产生副作用
	if err := bystander.WriteJSON(&ws.Message{Type: ws.MsgTypeTalkbackStop}); err != nil {
		t.Fatalf("bystander talkback_stop: %v", err)
	}

	// 属主断连：会话自动释放
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	select {
	case <-acked:
	case <-time.After(3 * time.Second):
		t.Fatal("talkback session was not released on owner disconnect")
	}
	if stops != 1 {
		t.Fatalf("expected exactly one stop, got %d", stops)
	}
}
