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
