package rtsp

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/onvif-ai/internal/audio"
)

// ErrNoBackchannel 表示 DESCRIBE 结果里没有任何 G.711 回传轨
// （设备不支持对讲）。上层用 errors.Is 分类后向前端回报稳定原因码。
var ErrNoBackchannel = errors.New("no audio backchannel found")

type Backchannel struct {
	rawURL    string
	client    *gortsplib.Client
	media     *description.Media
	encoder   *rtplpcm.Encoder
	connected bool
	// writeMu 串行化 WritePCM：TTS 回传与浏览器对讲共用同一通道，
	// 并发写会在 RTP 编码器与底层连接上产生数据竞争。
	writeMu sync.Mutex
}

func NewBackchannel(rtspURL string) *Backchannel {
	return &Backchannel{rawURL: rtspURL}
}

func (b *Backchannel) Connect() error {
	u, err := url.Parse(b.rawURL)
	if err != nil {
		return fmt.Errorf("parse RTSP URL: %w", err)
	}

	b.client = &gortsplib.Client{
		Scheme:              u.Scheme,
		Host:                u.Host,
		RequestBackChannels: true,
	}

	if err := b.client.Start(); err != nil {
		return fmt.Errorf("start RTSP client: %w", err)
	}

	baseURL, err := base.ParseURL(b.rawURL)
	if err != nil {
		b.client.Close()
		return fmt.Errorf("parse base URL: %w", err)
	}

	desc, _, err := b.client.Describe(baseURL)
	if err != nil {
		b.client.Close()
		return fmt.Errorf("describe: %w", err)
	}

	media, g711 := findG711BackChannelMedia(desc)
	if media == nil {
		b.client.Close()
		return fmt.Errorf("find backchannel media: %w", ErrNoBackchannel)
	}

	b.media = media

	if _, err := b.client.Setup(baseURL, media, 0, 0); err != nil {
		b.client.Close()
		return fmt.Errorf("setup backchannel: %w", err)
	}

	if _, err := b.client.Play(nil); err != nil {
		b.client.Close()
		return fmt.Errorf("play backchannel: %w", err)
	}

	enc, err := g711.CreateEncoder()
	if err != nil {
		b.client.Close()
		return fmt.Errorf("create G711 encoder: %w", err)
	}
	enc.Init()
	b.encoder = enc
	b.connected = true

	go b.keepAlive()
	return nil
}

// WritePCM 接收 16kHz 单声道 16 位小端 PCM，内部重采样到 8kHz 并按
// A-law 编码为 G.711 RTP 推送。调用方（TTS 回传、浏览器对讲）可能并发，
// writeMu 保证一次完整写入不被交错。
func (b *Backchannel) WritePCM(pcm16 []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	if !b.connected {
		return fmt.Errorf("backchannel not connected")
	}

	resampled, err := audio.ResamplePCM(pcm16, audio.PCM16kSampleRate, audio.G711SampleRate)
	if err != nil {
		return fmt.Errorf("resample: %w", err)
	}

	g711Bytes, err := audio.EncodePCMToG711(resampled, audio.G711ALaw)
	if err != nil {
		return fmt.Errorf("encode G.711: %w", err)
	}

	pkts, err := b.encoder.Encode(g711Bytes)
	if err != nil {
		return fmt.Errorf("encode RTP: %w", err)
	}

	for _, pkt := range pkts {
		if err := b.client.WritePacketRTP(b.media, pkt); err != nil {
			return fmt.Errorf("write RTP: %w", err)
		}
	}

	return nil
}

func (b *Backchannel) keepAlive() {
	silence := make([]byte, audio.G711SampleRate/10)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if !b.connected {
			return
		}
		b.encoder.Encode(silence)
	}
}

func (b *Backchannel) Close() {
	b.connected = false
	if b.client != nil {
		b.client.Close()
	}
}

func findG711BackChannelMedia(desc *description.Session) (*description.Media, *format.G711) {
	for _, media := range desc.Medias {
		if media.IsBackChannel {
			for _, f := range media.Formats {
				if g711, ok := f.(*format.G711); ok {
					return media, g711
				}
			}
		}
	}
	return nil, nil
}
