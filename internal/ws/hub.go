package ws

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
				default:
					go h.removeClient(client)
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
		send: make(chan []byte, 256),
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
