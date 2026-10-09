package forward

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// retryInitialBackoff/retryMaxBackoff 是转发重试的指数退避区间：5s 起步、
// 30s 封顶。源设备重启、流媒体服务器短暂离线都会触发重试，封顶值避免长
// 时间离线后过度频繁探测。声明为 var 仅供单测缩短等待，运行时只读。
var (
	retryInitialBackoff = 5 * time.Second
	retryMaxBackoff     = 30 * time.Second
)

// 一路转发的状态取值（对外稳定枚举，前端按此渲染）。
const (
	StateRunning  = "running"
	StateRetrying = "retrying"
	StateError    = "error"
)

// Forwarder 负责一路画面（一个 RTSP 流）的拉取与转推，生命周期由 ctx
// 控制：ctx 取消即干净退出。所有方法线程安全，状态可随时被 Status 读取。
type Forwarder struct {
	sourceURL string // 源 RTSP 地址（可能内嵌凭证 userinfo）
	targetURL string // 完整推流地址（含路径与凭证）
	path      string // 流媒体服务器上的路径，供状态展示

	mu      sync.Mutex
	state   string
	lastErr string
	// backoff 是下一轮失败后的等待时长，成功进入 running 后复位
	backoff time.Duration
	// publishErr 记录推流写失败（写回调里拿到，主循环据此结束本轮）
	publishErr error
}

// NewForwarder 创建一路转发。targetURL 必须已是拼好路径的完整地址。
func NewForwarder(sourceURL, targetURL, path string) *Forwarder {
	return &Forwarder{
		sourceURL: sourceURL,
		targetURL: targetURL,
		path:      path,
		state:     StateRetrying,
		backoff:   retryInitialBackoff,
	}
}

func (f *Forwarder) setState(state, errMsg string) {
	f.mu.Lock()
	f.state = state
	f.lastErr = errMsg
	f.mu.Unlock()
}

// Snapshot 返回该路的对外状态快照。
func (f *Forwarder) Snapshot() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Status{
		// 源地址可能内嵌凭证，对外展示一律脱敏（RedactUserinfo）
		Source:    RedactUserinfo(f.sourceURL),
		Path:      f.path,
		State:     f.state,
		LastError: f.lastErr,
	}
}

// Run 阻塞执行「拉流 → 转推」循环，直到 ctx 取消。任何一端断开或失败
// 都会拆掉整轮连接，退避后重试——源与目标常需成对重建（目标重置会话时
// 源的 RTP 也需重新订阅），整轮重来最简单也最稳。
func (f *Forwarder) Run(ctx context.Context) {
	defer f.setState(StateError, "已停止")
	for {
		if ctx.Err() != nil {
			return
		}
		err := f.cycle(ctx)
		if ctx.Err() != nil {
			return
		}
		f.mu.Lock()
		wait := f.backoff
		f.backoff = min(wait*2, retryMaxBackoff)
		f.mu.Unlock()
		f.setState(StateRetrying, errString(err))
		log.Printf("[forward] %s 断开，%v 后重试: %v", f.path, wait, err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// cycle 完成一轮连接：拉起源端、把相同媒体轨集合发布到目标端，然后
// 阻塞等待任一端断开。返回 nil 语义为「非错误中断」，上层一律重试。
func (f *Forwarder) cycle(ctx context.Context) error {
	srcU, err := base.ParseURL(f.sourceURL)
	if err != nil {
		return fmt.Errorf("源地址无效: %w", err)
	}
	dstU, err := base.ParseURL(f.targetURL)
	if err != nil {
		return fmt.Errorf("目标地址无效: %w", err)
	}

	// 源端：Describe 拿到媒体轨集合后全部 Setup（凭证留在 URL userinfo，
	// gortsplib 遇 401 自动携带凭证重试）
	src := &gortsplib.Client{Scheme: srcU.Scheme, Host: srcU.Host}
	if err := src.Start(); err != nil {
		return fmt.Errorf("连接源失败: %w", err)
	}
	defer src.Close()

	desc, _, err := src.Describe(srcU)
	if err != nil {
		return fmt.Errorf("获取源描述失败: %w", err)
	}

	// 对讲回传轨只收不发，对转推无意义，发布出去反而让目标端会话挂异常
	medias := make([]*description.Media, 0, len(desc.Medias))
	for _, m := range desc.Medias {
		if !m.IsBackChannel {
			medias = append(medias, m)
		}
	}
	if len(medias) == 0 {
		return errors.New("源没有可转发的媒体轨")
	}
	if err := src.SetupAll(srcU, medias); err != nil {
		return fmt.Errorf("订阅源媒体轨失败: %w", err)
	}

	// 目标端：以与源相同的媒体轨集合 ANNOUNCE + RECORD。
	// 注意 StartRecording 内部自带 Start()，此处不能再手动 Start，
	// 否则客户端跑起两个主循环，请求匹配错乱后触发库内 panic。
	pub := &gortsplib.Client{Scheme: dstU.Scheme, Host: dstU.Host}
	defer pub.Close() // StartRecording 失败时也会自关，重复 Close 是幂等的

	pubDesc := &description.Session{Medias: medias}
	if err := pub.StartRecording(f.targetURL, pubDesc); err != nil {
		return fmt.Errorf("发布到流媒体服务器失败: %w", err)
	}

	// 源包原样透传（不解不编，RTP 直通），推流失败时关闭源端让本轮结束
	f.mu.Lock()
	f.publishErr = nil
	f.mu.Unlock()
	src.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if err := pub.WritePacketRTP(medi, pkt); err != nil {
			f.mu.Lock()
			if f.publishErr == nil {
				f.publishErr = err
			}
			f.mu.Unlock()
			src.Close() // 触发 src.Wait() 返回，结束本轮
		}
	})

	if _, err := src.Play(nil); err != nil {
		return fmt.Errorf("开始拉流失败: %w", err)
	}

	f.mu.Lock()
	f.backoff = retryInitialBackoff // 一轮完整建起即认为恢复，复位退避
	f.mu.Unlock()
	f.setState(StateRunning, "")
	log.Printf("[forward] %s 转发中: %s → %s", f.path, RedactUserinfo(f.sourceURL), RedactUserinfo(f.targetURL))

	done := make(chan error, 2)
	go func() { done <- src.Wait() }()
	go func() { done <- pub.Wait() }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		f.mu.Lock()
		pubErr := f.publishErr
		f.mu.Unlock()
		if pubErr != nil {
			return fmt.Errorf("推送中断: %w", pubErr)
		}
		if err != nil {
			return fmt.Errorf("连接中断: %w", err)
		}
		return errors.New("连接中断")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
