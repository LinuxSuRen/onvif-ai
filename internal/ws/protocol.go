// Package ws provides WebSocket hub, client handler, and message protocol.
package ws

import "encoding/json"

// MessageType defines the type of WebSocket message.
type MessageType string

const (
	// Client → Server
	MsgTypeAudioStart    MessageType = "audio_start"    // Start mic capture
	MsgTypeAudioData     MessageType = "audio_data"     // Audio chunk (base64 PCM)
	MsgTypeAudioStop     MessageType = "audio_stop"     // Stop mic capture
	MsgTypeSpeechText    MessageType = "speech_text"    // Recognized speech text from browser
	MsgTypeCameraListen  MessageType = "camera_listen"  // Trigger STT on buffered camera audio
	MsgTypeClearHistory  MessageType = "clear_history"  // Clear conversation history
	MsgTypePTZMove       MessageType = "ptz_move"       // PTZ direction command
	MsgTypeSwitchMode    MessageType = "switch_mode"    // Switch audio mode
	MsgTypeClockSync     MessageType = "clock_sync"     // Clock offset probe (both directions)
	MsgTypeTalkbackStart MessageType = "talkback_start" // Start intercom session (browser mic → camera speaker)
	MsgTypeAudioIn       MessageType = "audio_in"       // Intercom audio chunk (base64 PCM16 16k mono)
	MsgTypeTalkbackStop  MessageType = "talkback_stop"  // Stop intercom session

	// Server → Client
	MsgTypeVideoNAL      MessageType = "video_nal"      // H.264 NAL unit (base64)
	MsgTypeVideoJPEG     MessageType = "video_jpeg"     // JPEG snapshot frame (base64)
	MsgTypeTranscript    MessageType = "transcript"     // LLM text response
	MsgTypeStatus        MessageType = "status"         // System status
	MsgTypeError         MessageType = "error"          // Error message
	MsgTypeAudioOut      MessageType = "audio_out"      // PCM audio for browser playback (base64)
	MsgTypeDeviceState   MessageType = "device_state"   // Device connection state update
	MsgTypePTZCommand    MessageType = "ptz_command"    // PTZ command result (direction + text)
	MsgTypeTalkbackState MessageType = "talkback_state" // Intercom session accept/reject result
)

// Message is the JSON envelope for all WebSocket messages.
type Message struct {
	Type    MessageType     `json:"type"`
	Data    string          `json:"data,omitempty"`    // base64 encoded binary data
	Text    string          `json:"text,omitempty"`    // text content
	Payload json.RawMessage `json:"payload,omitempty"` // structured data
	// Ts is the server wall-clock time (unix ms) at which a frame was
	// received from the RTSP source; used by the browser to measure latency.
	Ts int64 `json:"ts,omitempty"`
	// Cam identifies which camera (media profile token) a frame belongs to
	// when a single ONVIF device exposes multiple cameras. Empty in the
	// legacy single-camera case, where the frontend treats it as the only
	// camera.
	Cam string `json:"cam,omitempty"`
}

// StatusState represents the system state.
type StatusState string

const (
	StatusIdle      StatusState = "idle"
	StatusListening StatusState = "listening"
	StatusThinking  StatusState = "thinking"
	StatusSpeaking  StatusState = "speaking"
)

// StatusPayload is the payload for status messages.
type StatusPayload struct {
	State StatusState `json:"state"`
}

// AudioMode represents the audio input mode.
type AudioMode string

const (
	AudioModeBrowserMic AudioMode = "browser_mic"
	AudioModeCameraMic  AudioMode = "camera_mic"
)

// AudioOutPayload describes the PCM chunk carried by an audio_out message.
// The parameters come from the negotiated source track (e.g. SDP clock rate
// of the camera's G.711 audio) so browsers can play the stream at the
// correct rate without hard-coding anything. Every audio_out message
// carries it, letting clients that join mid-stream self-configure.
type AudioOutPayload struct {
	Rate     int `json:"rate"`     // sample rate in Hz (e.g. 8000)
	Channels int `json:"channels"` // channel count (G.711 sources are mono)
}

// Talkback 会话拒绝码（稳定契约，前端据此映射文案）。
const (
	TalkbackRejectNoBackchannel = "no_backchannel"  // 设备无对讲回传通道
	TalkbackRejectBusy          = "busy"            // TTS 语音播报占用中
	TalkbackRejectInUse         = "talkback_in_use" // 其他对讲会话占用中
)

// TalkbackSessionPayload 是 talkback_state 消息的负载：服务器对
// talkback_start 的受理结果，仅回复给发起的客户端。
type TalkbackSessionPayload struct {
	Active bool   `json:"active"`           // true=会话已受理
	Reason string `json:"reason,omitempty"` // 拒绝码，见 TalkbackReject* 常量
}

// NewMessage creates a new Message of the given type.
func NewMessage(t MessageType) *Message {
	return &Message{Type: t}
}

// WithData sets the data field (base64).
func (m *Message) WithData(data string) *Message {
	m.Data = data
	return m
}

// WithText sets the text field.
func (m *Message) WithText(text string) *Message {
	m.Text = text
	return m
}

// WithPayload sets the payload field.
func (m *Message) WithPayload(p interface{}) *Message {
	b, _ := json.Marshal(p)
	m.Payload = b
	return m
}
