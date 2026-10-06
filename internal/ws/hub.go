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
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan []byte
	mu     sync.Mutex
	closed bool

	// slowSince 由 Run 循环独占访问：send 持续满仓的起始时刻
	slowSince time.Time
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
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
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
					client.slowSince = time.Time{}
				default:
					// 缓冲满：宽限期内丢消息（下个 IDR 自动恢复画面），
					// 持续落后才移除，防止卡死客户端拖垮广播
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
	}
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

// BroadcastVideoNAL sends one H.264 NAL unit; cam is the media profile token
// of the camera it belongs to (may be empty for single-camera devices).
func (h *Hub) BroadcastVideoNAL(cam string, nalu []byte) {
	msg := &Message{
		Type: MsgTypeVideoNAL,
		Data: base64.StdEncoding.EncodeToString(nalu),
		Ts:   time.Now().UnixMilli(),
		Cam:  cam,
	}
	h.BroadcastMessage(msg)
}

// BroadcastVideoJPEG sends one JPEG snapshot frame for the given camera.
func (h *Hub) BroadcastVideoJPEG(cam string, jpeg []byte) {
	msg := &Message{
		Type: MsgTypeVideoJPEG,
		Data: base64.StdEncoding.EncodeToString(jpeg),
		Cam:  cam,
	}
	h.BroadcastMessage(msg)
}

func (h *Hub) BroadcastAudioPCM(pcm []byte) {
	msg := &Message{
		Type: MsgTypeAudioOut,
		Data: base64.StdEncoding.EncodeToString(pcm),
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
