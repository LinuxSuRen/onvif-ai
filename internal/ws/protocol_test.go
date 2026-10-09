package ws

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestMessageSerialization(t *testing.T) {
	msg := NewMessage(MsgTypeVideoNAL).WithData(base64.StdEncoding.EncodeToString([]byte{0x00, 0x01}))

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.Type != MsgTypeVideoNAL {
		t.Errorf("expected type video_nal, got %s", decoded.Type)
	}
}

func TestStatusMessage(t *testing.T) {
	payload, err := json.Marshal(StatusPayload{State: StatusListening})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	msg := &Message{
		Type:    MsgTypeStatus,
		Payload: payload,
	}

	data, _ := json.Marshal(msg)

	var decoded Message
	json.Unmarshal(data, &decoded)

	if decoded.Type != MsgTypeStatus {
		t.Errorf("expected status type, got %s", decoded.Type)
	}
}

func TestNewMessageHelpers(t *testing.T) {
	msg := NewMessage(MsgTypeTranscript).WithText("Hello").WithData("base64data")

	if msg.Type != MsgTypeTranscript {
		t.Errorf("wrong type: %s", msg.Type)
	}
	if msg.Text != "Hello" {
		t.Errorf("wrong text: %s", msg.Text)
	}
	if msg.Data != "base64data" {
		t.Errorf("wrong data: %s", msg.Data)
	}
}

func TestAudioModeConstants(t *testing.T) {
	if AudioModeBrowserMic != "browser_mic" {
		t.Errorf("unexpected browser_mic value: %s", AudioModeBrowserMic)
	}
	if AudioModeCameraMic != "camera_mic" {
		t.Errorf("unexpected camera_mic value: %s", AudioModeCameraMic)
	}
}

func TestBroadcastAudioPCMPayload(t *testing.T) {
	hub := NewHub()
	hub.BroadcastAudioPCM([]byte{0x01, 0x02, 0x03, 0x04}, 8000, 1)

	raw := <-hub.broadcast

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if msg.Type != MsgTypeAudioOut {
		t.Fatalf("expected type audio_out, got %s", msg.Type)
	}

	var payload AudioOutPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}
	if payload.Rate != 8000 || payload.Channels != 1 {
		t.Fatalf("expected rate=8000 channels=1, got rate=%d channels=%d", payload.Rate, payload.Channels)
	}

	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		t.Fatalf("decode data failed: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("expected 4 PCM bytes, got %d", len(data))
	}
}

func TestStatusStates(t *testing.T) {
	states := []StatusState{StatusIdle, StatusListening, StatusThinking, StatusSpeaking}
	expected := []string{"idle", "listening", "thinking", "speaking"}
	for i, s := range states {
		if string(s) != expected[i] {
			t.Errorf("state %d: expected %s, got %s", i, expected[i], s)
		}
	}
}

func TestBroadcastConnectErrorPayload(t *testing.T) {
	hub := NewHub()
	hub.BroadcastConnectError("http://192.168.1.64:80/onvif/device_service", ConnectErrAuthRequired)

	raw := <-hub.broadcast

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if msg.Type != MsgTypeConnectError {
		t.Fatalf("expected type connect_error, got %s", msg.Type)
	}

	var payload ConnectErrorPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}
	if payload.Address != "http://192.168.1.64:80/onvif/device_service" || payload.Code != "auth_required" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestConnectErrorCodes(t *testing.T) {
	codes := map[string]string{
		ConnectErrAuthRequired: "auth_required",
		ConnectErrAuthFailed:   "auth_failed",
	}
	for got, want := range codes {
		if got != want {
			t.Errorf("connect error code mismatch: got %s want %s", got, want)
		}
	}
}

func TestTalkbackMessageTypes(t *testing.T) {
	pairs := []struct {
		t    MessageType
		want string
	}{
		{MsgTypeTalkbackStart, "talkback_start"},
		{MsgTypeAudioIn, "audio_in"},
		{MsgTypeTalkbackStop, "talkback_stop"},
		{MsgTypeTalkbackState, "talkback_state"},
	}
	for _, p := range pairs {
		if string(p.t) != p.want {
			t.Errorf("expected %s, got %s", p.want, p.t)
		}
	}
}

func TestTalkbackSessionPayloadJSON(t *testing.T) {
	// 受理：仅 active 字段
	b, err := json.Marshal(TalkbackSessionPayload{Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"active":true}` {
		t.Fatalf("unexpected accept payload: %s", b)
	}

	// 拒绝：active=false + 稳定拒绝码
	b, err = json.Marshal(TalkbackSessionPayload{Active: false, Reason: TalkbackRejectBusy})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"active":false,"reason":"busy"}` {
		t.Fatalf("unexpected reject payload: %s", b)
	}

	// 前端解析方向：还原能够区分拒绝码
	var p TalkbackSessionPayload
	if err := json.Unmarshal([]byte(`{"active":false,"reason":"no_backchannel"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Active || p.Reason != TalkbackRejectNoBackchannel {
		t.Fatalf("unexpected round-trip: %+v", p)
	}
}

func TestTalkbackRejectCodes(t *testing.T) {
	codes := map[string]string{
		TalkbackRejectNoBackchannel: "no_backchannel",
		TalkbackRejectBusy:          "busy",
		TalkbackRejectInUse:         "talkback_in_use",
	}
	for got, want := range codes {
		if got != want {
			t.Errorf("reject code mismatch: got %s want %s", got, want)
		}
	}
}
