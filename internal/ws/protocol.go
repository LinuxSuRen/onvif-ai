// Package ws provides WebSocket hub, client handler, and message protocol.
package ws

import "encoding/json"

// MessageType defines the type of WebSocket message.
type MessageType string

const (
	// Client → Server
	MsgTypeAudioStart  MessageType = "audio_start"  // Start mic capture
	MsgTypeAudioData   MessageType = "audio_data"   // Audio chunk (base64 PCM)
	MsgTypeAudioStop   MessageType = "audio_stop"   // Stop mic capture
	MsgTypeSpeechText   MessageType = "speech_text"   // Recognized speech text from browser
	MsgTypeCameraListen MessageType = "camera_listen" // Trigger STT on buffered camera audio
	MsgTypeClearHistory MessageType = "clear_history"  // Clear conversation history
	MsgTypeSwitchMode   MessageType = "switch_mode"    // Switch audio mode

	// Server → Client
	MsgTypeVideoNAL  MessageType = "video_nal"  // H.264 NAL unit (base64)
	MsgTypeVideoJPEG MessageType = "video_jpeg" // JPEG snapshot frame (base64)
	MsgTypeTranscript MessageType = "transcript" // LLM text response
	MsgTypeStatus    MessageType = "status"     // System status
	MsgTypeError     MessageType = "error"      // Error message
	MsgTypeAudioOut   MessageType = "audio_out"   // PCM audio for browser playback (base64)
	MsgTypeDeviceState MessageType = "device_state" // Device connection state update
)

// Message is the JSON envelope for all WebSocket messages.
type Message struct {
	Type    MessageType     `json:"type"`
	Data    string          `json:"data,omitempty"`    // base64 encoded binary data
	Text    string          `json:"text,omitempty"`    // text content
	Payload json.RawMessage `json:"payload,omitempty"` // structured data
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
