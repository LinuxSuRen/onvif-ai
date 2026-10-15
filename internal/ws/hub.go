package ws

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 客户端发送缓冲与慢客户端策略：
// 视频流高峰约 70 条消息/秒，页面偶发的 JS 停顿（GC、渲染）会让消费短暂
// 变慢。缓冲满时先丢弃消息（视频流丢帧可由下一个 IDR 自动恢复），
// 持续落后超过 slowClientGrace 才断开，避免正常客户端被误杀。
const (
	clientSendBuffer = 1024
	slowClientGrace  = 3 * time.Second
	writeTimeout     = 10 * time.Second
)

type Hub struct {
	clients   map[*Client]bool
	broadcast chan []byte
	// videoBroadcast 承载视频帧：与 broadcast 分离，投递时只发给
	// 已显式请求画面的连接（issue #26），其余消息仍全员广播
	videoBroadcast chan videoMsg
	register       chan *Client
	unregister     chan *Client
	mu             sync.RWMutex
}

// videoMsg 视频广播单元：序列化后的消息 + 关键帧语义（供丢到关键帧策略）。
type videoMsg struct {
	data []byte
	cam  string
	key  bool
}

type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan []byte
	mu     sync.Mutex
	closed bool

	// wantVideo 由 hub.mu 保护：该连接是否已显式请求接收视频帧。
	// 默认 false（页面打开不自动推流），view_control start 置 true。
	// 状态挂在连接上，断开即销毁，重连自然回到默认关闭。
	wantVideo bool

	// slowSince 由 Run 循环独占访问：send 持续满仓的起始时刻
	slowSince time.Time

	// skipUntilKey 由 Run 循环独占访问：cam → 是否丢弃该路非关键帧。
	// send 满仓丢帧后置位：H.264 丢掉 GOP 中间的参考帧后，后续 P 帧会
	// 解码出绿色/花屏伪影；改为持续丢弃非关键帧直到下一个关键帧，
	// 表现为画面短暂冻结而非花屏。
	skipUntilKey map[string]bool
}

func NewHub() *Hub {
	return &Hub{
		clients:        make(map[*Client]bool),
		broadcast:      make(chan []byte, 256),
		videoBroadcast: make(chan videoMsg, 512),
		register:       make(chan *Client),
		unregister:     make(chan *Client),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			_, stillRegistered := h.clients[client]
			if stillRegistered {
				delete(h.clients, client)
			}
			h.mu.Unlock()
			if stillRegistered {
				client.markClosed() // 关闭 send 前置 closed 标志，Send 不再竞态
			}

		case message := <-h.broadcast:
			h.dispatch(message, false, "", false)

		case vm := <-h.videoBroadcast:
			h.dispatch(vm.data, true, vm.cam, vm.key)
		}
	}
}

// dispatch 把一条已序列化的消息投递给客户端；videoOnly 为 true 时仅投给
// 已显式请求画面的连接。video 消息带 key 语义（cam 非空时启用丢到关键帧
// 策略）。只在 Run 循环内调用，slowSince/skipUntilKey 的独占访问约定
// 保持不变。
func (h *Hub) dispatch(message []byte, videoOnly bool, cam string, key bool) {
	h.mu.RLock()
	for client := range h.clients {
		if videoOnly && !client.wantVideo {
			continue
		}
		if videoOnly && cam != "" && client.skipUntilKey[cam] && !key {
			// 该路正处丢帧期：非关键帧直接丢弃（不计慢、不触发断开）
			continue
		}
		select {
		case client.send <- message:
			client.slowSince = time.Time{}
			if videoOnly && cam != "" {
				if client.skipUntilKey == nil {
					client.skipUntilKey = map[string]bool{}
				}
				delete(client.skipUntilKey, cam)
			}
		default:
			// 缓冲满：宽限期内丢消息，并进入丢到关键帧模式；
			// 持续落后才移除，防止卡死客户端拖垮广播
			if client.skipUntilKey == nil {
				client.skipUntilKey = map[string]bool{}
			}
			if videoOnly && cam != "" {
				client.skipUntilKey[cam] = true
			}
			if client.slowSince.IsZero() {
				client.slowSince = time.Now()
				continue
			}
			if time.Since(client.slowSince) > slowClientGrace {
				go h.removeClient(client)
			}
		}
	}
	h.mu.RUnlock()
}

func (h *Hub) removeClient(client *Client) {
	h.mu.Lock()
	_, stillRegistered := h.clients[client]
	if stillRegistered {
		delete(h.clients, client)
	}
	h.mu.Unlock()
	if stillRegistered {
		client.markClosed()
		log.Printf("[ws] slow client removed (send buffer full): %s", client.conn.RemoteAddr())
	}
}

func (h *Hub) BroadcastMessage(msg *Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.broadcast <- data
}

// BroadcastVideo 把视频消息只投递给已显式请求画面的客户端（issue #26：
// 未打开画面的连接不搬运视频帧，省带宽），其余消息仍走 BroadcastMessage
// 全员广播。cam/key 驱动丢到关键帧策略（见 dispatch）。
func (h *Hub) BroadcastVideo(msg *Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.videoBroadcast <- videoMsg{data: data, cam: msg.Cam, key: msg.Key}
}

// BroadcastVideoFMP4 sends one fMP4 segment (init or fragment).
// kind is "init" or "frag"; key marks a keyframe fragment.
func (h *Hub) BroadcastVideoFMP4(cam, kind string, data []byte, key bool, codec string) {
	msg := &Message{
		Type:  MsgTypeVideoFMP4,
		Data:  base64.StdEncoding.EncodeToString(data),
		Kind:  kind,
		Key:   key,
		Cam:   cam,
		Codec: codec,
		Ts:    time.Now().UnixMilli(),
	}
	h.BroadcastVideo(msg)
}

// SetVideoView 设置某条连接是否接收视频广播，是 view_control start/stop
// 的落地动作。写侧持 hub 锁，与 dispatch 的读侧互斥。
func (h *Hub) SetVideoView(client *Client, enabled bool) {
	h.mu.Lock()
	client.wantVideo = enabled
	h.mu.Unlock()
}

// BroadcastVideoNAL sends one H.264 NAL unit; cam is the media profile token
// of the camera it belongs to (may be empty for single-camera devices).
func (h *Hub) BroadcastVideoNAL(cam string, nalu []byte) {
	msg := &Message{
		Type: MsgTypeVideoNAL,
		Data: base64.StdEncoding.EncodeToString(nalu),
		Ts:   time.Now().UnixMilli(),
		Cam:  cam,
	}
	h.BroadcastVideo(msg)
}

// BroadcastVideoJPEG sends one JPEG snapshot frame for the given camera.
// Key 恒为 true：JPEG 每帧独立可解，语义上等价关键帧——否则「丢到
// 关键帧」策略在 JPEG 路永远无法解除，慢客户端一次满仓后该路永久冻结。
func (h *Hub) BroadcastVideoJPEG(cam string, jpeg []byte) {
	msg := &Message{
		Type: MsgTypeVideoJPEG,
		Data: base64.StdEncoding.EncodeToString(jpeg),
		Cam:  cam,
		Key:  true,
	}
	h.BroadcastVideo(msg)
}

// BroadcastAudioPCM sends one linear PCM chunk from the camera's audio track.
// sampleRate/channels describe the chunk and are forwarded in the payload so
// the browser can set up playback dynamically (no hard-coded rate).
func (h *Hub) BroadcastAudioPCM(pcm []byte, sampleRate, channels int) {
	payload, _ := json.Marshal(AudioOutPayload{
		Rate:     sampleRate,
		Channels: channels,
	})
	msg := &Message{
		Type:    MsgTypeAudioOut,
		Data:    base64.StdEncoding.EncodeToString(pcm),
		Payload: payload,
	}
	h.BroadcastMessage(msg)
}

func (h *Hub) BroadcastTranscript(text string) {
	msg := &Message{
		Type: MsgTypeTranscript,
		Text: text,
	}
	h.BroadcastMessage(msg)
}

func (h *Hub) BroadcastStatus(state StatusState) {
	payload, _ := json.Marshal(StatusPayload{State: state})
	msg := &Message{
		Type:    MsgTypeStatus,
		Payload: payload,
	}
	h.BroadcastMessage(msg)
}

func (h *Hub) BroadcastError(errMsg string) {
	msg := &Message{
		Type: MsgTypeError,
		Text: errMsg,
	}
	h.BroadcastMessage(msg)
}

func (h *Hub) BroadcastDeviceState(state interface{}) {
	payload, _ := json.Marshal(state)
	msg := &Message{
		Type:    MsgTypeDeviceState,
		Payload: payload,
	}
	h.BroadcastMessage(msg)
}

// BroadcastConnectError 广播结构化连接失败：code 为稳定失败码，
// 前端据此触发交互（如认证错误时引导填写凭证），展示文案由前端映射。
func (h *Hub) BroadcastConnectError(address, code string) {
	payload, _ := json.Marshal(ConnectErrorPayload{Address: address, Code: code})
	msg := &Message{
		Type:    MsgTypeConnectError,
		Payload: payload,
	}
	h.BroadcastMessage(msg)
}

func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) RegisterClient(conn *websocket.Conn) *Client {
	client := &Client{
		hub:  h,
		conn: conn,
		send: make(chan []byte, clientSendBuffer),
	}
	h.register <- client
	return client
}

// Send delivers a message to this client only. It is used to seed freshly
// connected clients with the current device state, which they would otherwise
// miss until the next state broadcast.
//
// The hub may concurrently remove the client and close its send channel (e.g.
// after a write timeout); the closed flag below closes that race — sending on
// a closed channel would panic and take the whole process down.
func (c *Client) Send(msg *Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	select {
	case c.send <- data:
	default:
	}
	c.mu.Unlock()
}

// markClosed marks the client closed and closes its send channel exactly
// once, waking the WritePump. Callers must hold no other locks.
func (c *Client) markClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.send)
	}
}

func (c *Client) WritePump() {
	defer func() {
		c.conn.Close()
	}()

	for message := range c.send {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		// 卡死的 TCP 连接若无写超时会永久阻塞排空，拖垮整个客户端
		c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		err := c.conn.WriteMessage(websocket.TextMessage, message)
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (c *Client) ReadPump(handler func(*Message)) {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if handler != nil {
			handler(&msg)
		}
	}
}

func (c *Client) Close() {
	c.markClosed()
}
