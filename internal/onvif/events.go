// Package onvif 的 Event 订阅客户端（PullPoint 模型）：
// CreatePullPointSubscription → 循环 PullMessages（阻塞至 Timeout）→ 自动续期。
// 事件解析后通过回调投递给上层路由。设备不支持 Event 时静默降级。
package onvif

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// EventMessage 解析后的 ONVIF 事件。
type EventMessage struct {
	Topic   string            `json:"topic"`   // 如 tns1:VideoSource/SignalLoss
	Source  map[string]string `json:"source"`  // Source SimpleItem（如 Camera_1）
	Data    map[string]string `json:"data"`    // Data SimpleItem（如 State=true）
	UTCTime string            `json:"utc"`     // 设备侧 UTC 时间戳
	Device  string            `json:"device"`  // 来源设备地址（由订阅器填充）
}

// IsMotion 判断是否为移动侦测事件。
func (e *EventMessage) IsMotion() bool { return strings.HasSuffix(e.Topic, "/CellMotion/Motion") }

// IsSignalLoss 判断是否为信号丢失事件。
func (e *EventMessage) IsSignalLoss() bool { return strings.HasSuffix(e.Topic, "/SignalLoss") }

// EventSubscriber 为单台设备维持 PullPoint 订阅。
type EventSubscriber struct {
	client  *Client
	device  string
	logger  *slog.Logger
	handler func(EventMessage)

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
}

// 事件长轮询的时限契约：PullMessages 按规范可阻塞至 tev:Timeout 才返回
// （设备空闲时必然如此），因此事件路径的 HTTP 客户端超时必须大于该值。
// 若沿用连接时 5s 的通用超时，空闲长轮询会被客户端先中止、订阅被拆除
// 重建：重建间隙的事件丢失，且旧订阅从不 Unsubscribe，设备侧 pull point
// 堆积到规范最低支持数（≥5）后会拒绝新建订阅，事件链路彻底断流。
const (
	pullWaitXML   = "PT5S"               // PullMessages 长轮询时长（SOAP 内）
	eventHTTPWait = 15 * time.Second     // 事件路径 HTTP 超时，须 > pullWait
	unsubWait     = 3 * time.Second      // 拆除订阅的独立超时
)

// StartEventSubscriber 启动设备的 Event 订阅（后台 goroutine）。
// handler 在订阅 goroutine 上回调（不要阻塞）。
func (c *Client) StartEventSubscriber(deviceAddr string, logger *slog.Logger,
	handler func(EventMessage)) *EventSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	// 事件路径使用独立的更长 HTTP 超时客户端（其余配置沿用连接客户端）
	cfg := c.config
	cfg.Timeout = eventHTTPWait
	evtClient := NewClient(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	s := &EventSubscriber{
		client:  evtClient,
		device:  deviceAddr,
		logger:  logger.With("device", deviceAddr),
		handler: handler,
		cancel:  cancel,
	}
	go s.loop(ctx)
	return s
}

// Stop 停止订阅（幂等）。
func (s *EventSubscriber) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
}

func (s *EventSubscriber) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// loop 主循环：创建订阅 → 循环 Pull → 出错退避重试重建。
func (s *EventSubscriber) loop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for !s.isStopped() {
		addr, err := s.createSubscription(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Debug("event subscribe failed", "err", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min2(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		s.pullLoop(ctx, addr)
		s.unsubscribe(addr) // best-effort 显式拆除，不依赖设备侧 60s 超时回收
		if s.isStopped() || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// createSubscription 发送 CreatePullPointSubscription，返回 wsa:Address。
func (s *EventSubscriber) createSubscription(ctx context.Context) (string, error) {
	envelope := s.client.soapEnvelope(
		`<tev:CreatePullPointSubscription xmlns:tev="http://www.onvif.org/ver10/events/wsdl">` +
			`<tev:InitialTerminationTime>PT60S</tev:InitialTerminationTime>` +
			`</tev:CreatePullPointSubscription>`)
	if s.client.config.Username != "" {
		envelope = s.client.injectSecurityHeader(envelope)
	}
	resp, err := s.client.doSoapCall(ctx, s.client.deviceURL(), "CreatePullPointSubscription", envelope)
	if err != nil {
		return "", fmt.Errorf("create pull point: %w", err)
	}
	addr := extractTagText(resp, "Address")
	if addr == "" {
		return "", fmt.Errorf("no subscription address in response")
	}
	return addr, nil
}

// pullLoop 循环 PullMessages：阻塞至 Timeout 或有消息，收到后解析并回调。
func (s *EventSubscriber) pullLoop(ctx context.Context, subAddr string) {
	for !s.isStopped() && ctx.Err() == nil {
		envelope := s.client.soapEnvelope(
			`<tev:PullMessages xmlns:tev="http://www.onvif.org/ver10/events/wsdl">` +
				`<tev:Timeout>` + pullWaitXML + `</tev:Timeout>` +
				`<tev:MessageLimit>20</tev:MessageLimit>` +
				`</tev:PullMessages>`)
		if s.client.config.Username != "" {
			envelope = s.client.injectSecurityHeader(envelope)
		}
		resp, err := s.client.doSoapCall(ctx, subAddr, "PullMessages", envelope)
		if err != nil {
			if ctx.Err() != nil || s.isStopped() {
				return
			}
			s.logger.Debug("pull failed", "err", err.Error())
			return // 订阅可能过期，退出重建
		}
		for _, msg := range parseNotifications(string(resp)) {
			msg.Device = s.device
			if s.handler != nil {
				s.handler(msg)
			}
		}
	}
}

// unsubscribe 显式拆除订阅（best-effort，失败仅记日志）：使用独立于订阅
// 生命周期 ctx 的短超时，订阅因错误重建或正常停止时都尽量通知设备回收
// pull point，避免堆积到订阅上限。
func (s *EventSubscriber) unsubscribe(subAddr string) {
	ctx, cancel := context.WithTimeout(context.Background(), unsubWait)
	defer cancel()
	envelope := s.client.soapEnvelope(
		`<tev:Unsubscribe xmlns:tev="http://www.onvif.org/ver10/events/wsdl"/>`)
	if s.client.config.Username != "" {
		envelope = s.client.injectSecurityHeader(envelope)
	}
	if _, err := s.client.doSoapCall(ctx, subAddr, "Unsubscribe", envelope); err != nil {
		s.logger.Debug("unsubscribe", "err", err.Error())
	}
}

// ---- 事件解析 ----

var (
	reNotif   = regexp.MustCompile(`(?s)<wsnt:NotificationMessage>(.*?)</wsnt:NotificationMessage>`)
	reTopic   = regexp.MustCompile(`<wsnt:Topic[^>]*>([^<]+)</wsnt:Topic>`)
	reItem    = regexp.MustCompile(`<tt:SimpleItem Name="([^"]+)" Value="([^"]*)"\s*/>`)
	reSrcSec  = regexp.MustCompile(`(?s)<tt:Source>(.*?)</tt:Source>`)
	reDataSec = regexp.MustCompile(`(?s)<tt:Data>(.*?)</tt:Data>`)
	reUtc     = regexp.MustCompile(`UtcTime="([^"]+)"`)
)

func parseNotifications(body string) []EventMessage {
	var out []EventMessage
	for _, m := range reNotif.FindAllStringSubmatch(body, -1) {
		inner := m[1]
		msg := EventMessage{Source: map[string]string{}, Data: map[string]string{}}
		if tm := reTopic.FindStringSubmatch(inner); tm != nil {
			msg.Topic = tm[1]
		}
		if um := reUtc.FindStringSubmatch(inner); um != nil {
			msg.UTCTime = um[1]
		}
		if sm := reSrcSec.FindStringSubmatch(inner); sm != nil {
			for _, it := range reItem.FindAllStringSubmatch(sm[1], -1) {
				msg.Source[it[1]] = it[2]
			}
		}
		if dm := reDataSec.FindStringSubmatch(inner); dm != nil {
			for _, it := range reItem.FindAllStringSubmatch(dm[1], -1) {
				msg.Data[it[1]] = it[2]
			}
		}
		if msg.Topic != "" {
			out = append(out, msg)
		}
	}
	return out
}

func extractTagText(data []byte, tag string) string {
	re := regexp.MustCompile(`(?:<[\w]+:)?` + tag + `(?:[^>]*)>([^<]+)<`)
	if m := re.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

func min2(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
