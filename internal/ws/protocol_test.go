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

func TestStatusStates(t *testing.T) {
	states := []StatusState{StatusIdle, StatusListening, StatusThinking, StatusSpeaking}
	expected := []string{"idle", "listening", "thinking", "speaking"}
	for i, s := range states {
		if string(s) != expected[i] {
			t.Errorf("state %d: expected %s, got %s", i, expected[i], s)
		}
	}
}
