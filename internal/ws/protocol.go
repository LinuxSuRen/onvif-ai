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
	MsgTypeViewControl   MessageType = "view_control"   // Open/close video push for this connection (start/stop)

	// Server → Client
	MsgTypeVideoNAL      MessageType = "video_nal"      // H.264 NAL unit (base64)（旧路径，保留兼容）
	MsgTypeVideoFMP4     MessageType = "video_fmp4"     // fMP4 段：init（moov）或 frag（moof+mdat），base64
	MsgTypeVideoJPEG     MessageType = "video_jpeg"     // JPEG snapshot frame (base64)
	MsgTypeTranscript    MessageType = "transcript"     // LLM text response
	MsgTypeStatus        MessageType = "status"         // System status
	MsgTypeError         MessageType = "error"          // Error message
	MsgTypeAudioOut      MessageType = "audio_out"      // PCM audio for browser playback (base64)
	MsgTypeDeviceState   MessageType = "device_state"   // Device connection state update
	MsgTypePTZCommand    MessageType = "ptz_command"    // PTZ command result (direction + text)
	MsgTypeTalkbackState MessageType = "talkback_state" // Intercom session accept/reject result
	MsgTypeConnectError  MessageType = "connect_error"  // Structured connect failure (stable code)
	MsgTypeDeviceEvent   MessageType = "device_event"   // ONVIF device event (SignalLoss/Motion/Battery/AuthFailure)
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
	// Kind 限定 video_fmp4 消息：init（初始化段）或 frag（媒体段）。
	Kind string `json:"kind,omitempty"`
	// Key 标记 video_fmp4/frag 是否关键帧；hub 丢帧时据此实现"丢到下个
	// 关键帧"（丢弃期间画面冻结而非花屏）。
	Key bool `json:"key,omitempty"`
	// Codec 限定 video_fmp4/init：MSE SourceBuffer 的 codec 串（avc1.XXXX）。
	Codec string `json:"codec,omitempty"`
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

// view_control 动作（稳定契约，前端原样发送）。
const (
	ViewActionStart = "start" // 该连接开始接收视频帧
	ViewActionStop  = "stop"  // 该连接停止接收视频帧
)

// ViewControlPayload 是 view_control 消息的负载。视频帧带宽高，
// 页面打开不等于用户想看画面，因此服务端默认不向新连接推送视频，
// 由用户点击画面前景占位符后显式请求（issue #26）。
type ViewControlPayload struct {
	Action string `json:"action"` // start / stop，见 ViewAction* 常量
}

// TalkbackSessionPayload 是 talkback_state 消息的负载：服务器对
// talkback_start 的受理结果，仅回复给发起的客户端。
type TalkbackSessionPayload struct {
	Active bool   `json:"active"`           // true=会话已受理
	Reason string `json:"reason,omitempty"` // 拒绝码，见 TalkbackReject* 常量
}

// ConnectError 连接失败码（稳定契约，前端据此映射文案与交互）。
const (
	ConnectErrAuthRequired = "auth_required" // 设备要求认证但未提供凭证
	ConnectErrAuthFailed   = "auth_failed"   // 凭证被设备拒绝
)

// ConnectErrorPayload 是 connect_error 消息的负载：定位失败设备并
// 携带稳定失败码，具体展示由前端决定。
type ConnectErrorPayload struct {
	Address string `json:"address"` // 连接的目标设备地址
	Code    string `json:"code"`    // 失败码，见 ConnectErr* 常量
}

// NewMessage creates a new Message of the given type.
func NewMessage(t MessageType) *Message {
	return &Message{Type: t}
}

// DeviceEventPayload 是 device_event 消息的负载：来自 ONVIF 设备的实时事件。
// Topic 为标准 ONVIF topic（如 tns1:VideoSource/SignalLoss），Data 携带键值对。
type DeviceEventPayload struct {
	Topic  string            `json:"topic"`
	Source map[string]string `json:"source,omitempty"`
	Data   map[string]string `json:"data,omitempty"`
	Device string            `json:"device,omitempty"` // 设备地址
	Time   string            `json:"time,omitempty"`   // 设备侧 UTC 时间
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
