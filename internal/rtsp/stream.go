package rtsp

import (
	"fmt"
	"log"
	"strings"
	"sync"

	aac "github.com/arabian9ts/aac-go"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/onvif-ai/internal/audio"
	"github.com/pion/rtp"
)

type Stream struct {
	rawURL string
	client *gortsplib.Client

	h264Dec   *rtph264.Decoder
	mjpegDec  *rtpmjpeg.Decoder
	mjpegForm *format.MJPEG
	g711Dec   *rtplpcm.Decoder

	// AAC（RFC 3640 mpeg4-generic）音频轨的解码链：RTP 解包 → 裸 AU →
	// ADTS 封装 → 纯 Go AAC-LC 解码 → 线性 PCM。解码失败仅降级音频
	// （记一次日志并重建解码器自愈），不影响视频通路。
	aacRTPDec    *rtpmpeg4audio.Decoder
	aacDec       *aac.Decoder
	aacFreqIndex int
	aacErrOnce   bool
	aacRTPOnce   bool

	// 音频轨道的实际参数（来自 SDP 协商结果），随 OnAudioPCM 回调透出，
	// 下游按此播放/处理，不写死采样率。
	audioSampleRate int
	audioChannels   int

	// SPS/PPS from the SDP. Many encoders put the parameter sets only in the
	// SDP (sprop-parameter-sets) and never resend them in-band; downstream
	// muxers (e.g. jmuxer in the browser) cannot initialize without them,
	// so we re-inject them ahead of every IDR frame.
	sps []byte
	pps []byte

	closeOnce sync.Once

	videoNALHandler  func([]byte)
	videoJPEGHandler func([]byte)
	audioPCMHandler  func(pcm []byte, sampleRate, channels int)
}

func NewStream(rtspURL string) *Stream {
	return &Stream{rawURL: rtspURL}
}

func (s *Stream) OnVideoNAL(handler func([]byte)) {
	s.videoNALHandler = handler
}

// OnVideoJPEG registers the handler for complete JPEG frames (MJPEG over
// RTP, RFC 2435). Each delivered buffer is a full, decodable JPEG image.
func (s *Stream) OnVideoJPEG(handler func([]byte)) {
	s.videoJPEGHandler = handler
}

// IsMJPEG reports whether the connected source delivers MJPEG (JPEG frames)
// instead of an H.264 NAL stream.
func (s *Stream) IsMJPEG() bool {
	return s.mjpegDec != nil
}

// OnAudioPCM registers the handler for linear PCM decoded from the stream's
// audio track. sampleRate/channels report the negotiated parameters of the
// source so the receiver can play the chunk correctly. The handler is only
// invoked when the source actually carries a supported audio track; streams
// without audio are completely unaffected.
func (s *Stream) OnAudioPCM(handler func(pcm []byte, sampleRate, channels int)) {
	s.audioPCMHandler = handler
}

// HasAudio reports whether the connected source carries a (supported) audio
// track. It is only meaningful after a successful Connect.
func (s *Stream) HasAudio() bool {
	return s.g711Dec != nil || s.aacDec != nil
}

func (s *Stream) Connect() error {
	if !strings.HasPrefix(s.rawURL, "rtsp://") && !strings.HasPrefix(s.rawURL, "rtsps://") {
		return fmt.Errorf("not an RTSP URL: %s", s.rawURL)
	}

	u, err := parseRTSPURL(s.rawURL)
	if err != nil {
		return fmt.Errorf("parse RTSP URL: %w", err)
	}

	s.client = &gortsplib.Client{
		Scheme: u.Scheme,
		Host:   u.Host,
	}

	if err := s.client.Start(); err != nil {
		return fmt.Errorf("start RTSP client: %w", err)
	}

	baseURL, err := base.ParseURL(s.rawURL)
	if err != nil {
		s.client.Close()
		return fmt.Errorf("parse base URL: %w", err)
	}

	desc, _, err := s.client.Describe(baseURL)
	if err != nil {
		s.client.Close()
		return fmt.Errorf("describe: %w", err)
	}

	var videoMedia *description.Media
	var audioMedia *description.Media
	var videoH264 *format.H264
	var videoMJPEG *format.MJPEG
	var audioG711 *format.G711
	var audioAAC *format.MPEG4Audio
	needsSetup := false

	// 音频格式选择分两轮，保证优先级与 SDP 中 m=audio 的出现顺序无关：
	// 第一轮只认 G.711（ONVIF 对讲事实标准、现有回传链路依赖）；
	// 第二轮在没有任何 G.711 时才接受 AAC-LC（RFC 3640 mpeg4-generic）。
	for _, media := range desc.Medias {
		if media.IsBackChannel {
			continue
		}
		for _, f := range media.Formats {
			if g711, ok := f.(*format.G711); ok &&
				media.Type == description.MediaTypeAudio && audioMedia == nil {
				audioMedia = media
				audioG711 = g711
			}
		}
	}
	if audioMedia == nil {
		for _, media := range desc.Medias {
			if media.IsBackChannel {
				continue
			}
			for _, f := range media.Formats {
				m4a, ok := f.(*format.MPEG4Audio)
				if !ok || media.Type != description.MediaTypeAudio {
					continue
				}
				// 仅接受 AAC-LC（解码器能力边界）；HE-AAC 等仍走不支持告警
				if m4a.Config != nil && m4a.Config.Type == mpeg4audio.ObjectTypeAACLC {
					audioMedia = media
					audioAAC = m4a
					break
				}
			}
			if audioAAC != nil {
				break
			}
		}
	}

	for _, media := range desc.Medias {
		if media.IsBackChannel {
			continue
		}
		for _, f := range media.Formats {
			switch ft := f.(type) {
			case *format.H264:
				if media.Type == description.MediaTypeVideo && videoMedia == nil {
					videoMedia = media
					videoH264 = ft
				}
			case *format.MJPEG:
				if media.Type == description.MediaTypeVideo && videoMedia == nil {
					videoMedia = media
					videoMJPEG = ft
				}
			}
		}
	}

	if videoMedia != nil {
		if _, err := s.client.Setup(baseURL, videoMedia, 0, 0); err != nil {
			s.Close()
			return fmt.Errorf("setup video: %w", err)
		}
		if videoH264 != nil {
			dec, err := videoH264.CreateDecoder()
			if err != nil {
				s.Close()
				return fmt.Errorf("create H264 decoder: %w", err)
			}
			dec.Init()
			s.h264Dec = dec
			s.sps = videoH264.SPS
			s.pps = videoH264.PPS
		}
		if videoMJPEG != nil {
			dec, err := videoMJPEG.CreateDecoder()
			if err != nil {
				s.Close()
				return fmt.Errorf("create MJPEG decoder: %w", err)
			}
			if err := dec.Init(); err != nil {
				s.Close()
				return fmt.Errorf("init MJPEG decoder: %w", err)
			}
			s.mjpegDec = dec
			s.mjpegForm = videoMJPEG
		}
		needsSetup = true
	}

	switch {
	case audioG711 != nil:
		if _, err := s.client.Setup(baseURL, audioMedia, 0, 0); err != nil {
			s.Close()
			return fmt.Errorf("setup audio: %w", err)
		}
		dec, err := audioG711.CreateDecoder()
		if err != nil {
			s.Close()
			return fmt.Errorf("create G711 decoder: %w", err)
		}
		dec.Init()
		s.g711Dec = dec
		s.audioSampleRate = audioG711.SampleRate
		s.audioChannels = audioG711.ChannelCount
		needsSetup = true

	case audioAAC != nil:
		freqIdx := aacSamplingFreqIndex(audioAAC.Config.SampleRate)
		if freqIdx < 0 {
			log.Printf("[rtsp] AAC sample rate %d not in ADTS table, audio disabled", audioAAC.Config.SampleRate)
		} else if d := aacNewDecoder(); d == nil {
			log.Printf("[rtsp] AAC decoder init failed, audio disabled")
		} else {
			if _, err := s.client.Setup(baseURL, audioMedia, 0, 0); err != nil {
				s.Close()
				return fmt.Errorf("setup audio: %w", err)
			}
			dec, err := audioAAC.CreateDecoder()
			if err != nil {
				s.Close()
				return fmt.Errorf("create AAC RTP decoder: %w", err)
			}
			dec.Init()
			s.aacRTPDec = dec
			s.aacDec = d
			s.aacFreqIndex = freqIdx
			s.audioSampleRate = audioAAC.Config.SampleRate
			s.audioChannels = audioAAC.Config.ChannelCount
			log.Printf("[rtsp] AAC audio track enabled: %d Hz, %d ch", s.audioSampleRate, s.audioChannels)
			needsSetup = true
		}

	default:
		// 源带音频轨道但编码不受支持（如 HE-AAC/MP3）时明确告警而非静默
		// 丢弃，方便排查“画面正常但没有声音”的情况；无音频轨道则不打印。
		for _, media := range desc.Medias {
			if media.Type == description.MediaTypeAudio && !media.IsBackChannel && media != audioMedia {
				kinds := make([]string, 0, len(media.Formats))
				for _, f := range media.Formats {
					kinds = append(kinds, fmt.Sprintf("%T", f))
				}
				log.Printf("[rtsp] audio track present but codec unsupported, ignoring: %s", strings.Join(kinds, ", "))
			}
		}
	}

	if needsSetup {
		if _, err := s.client.Play(nil); err != nil {
			s.Close()
			return fmt.Errorf("play: %w", err)
		}
	}

	if s.h264Dec != nil {
		s.client.OnPacketRTP(videoMedia, videoH264, func(pkt *rtp.Packet) {
			nalus, err := s.h264Dec.Decode(pkt)
			if err != nil || s.videoNALHandler == nil {
				return
			}
			for _, nalu := range nalus {
				if isIDRNAL(nalu) {
					// Parameter sets usually live only in the SDP; re-send
					// them ahead of each keyframe so any client that just
					// joined (or re-initialized its decoder) can start
					// decoding immediately.
					if len(s.sps) > 0 {
						s.videoNALHandler(s.sps)
					}
					if len(s.pps) > 0 {
						s.videoNALHandler(s.pps)
					}
				}
				s.videoNALHandler(nalu)
			}
		})
	}

	if s.mjpegDec != nil {
		s.client.OnPacketRTP(videoMedia, s.mjpegForm, func(pkt *rtp.Packet) {
			img, err := s.mjpegDec.Decode(pkt)
			if err != nil || len(img) == 0 || s.videoJPEGHandler == nil {
				return
			}
			// RFC 2435 解出的每帧即完整 JPEG，直接复用快照的浏览器通道
			s.videoJPEGHandler(img)
		})
	}

	if s.g711Dec != nil {
		mulaw := audioG711.MULaw
		s.client.OnPacketRTP(audioMedia, audioG711, func(pkt *rtp.Packet) {
			// rtplpcm 解码器只负责拆 RTP：payload 仍是 G.711 压缩字节，
			// 必须按协商的压扩律展开成线性 PCM 才能交给播放/识别
			g711, err := s.g711Dec.Decode(pkt)
			if err != nil || s.audioPCMHandler == nil {
				return
			}
			law := audio.G711ALaw
			if mulaw {
				law = audio.G711MuLaw
			}
			pcm := audio.DecodeG711ToPCM(g711, law)
			s.audioPCMHandler(pcm, s.audioSampleRate, s.audioChannels)
		})
	}

	if s.aacDec != nil {
		s.client.OnPacketRTP(audioMedia, audioAAC, func(pkt *rtp.Packet) {
			aus, err := s.aacRTPDec.Decode(pkt)
			if err != nil {
				// 分片/丢包等传输层错误：丢一拍等下一个完整 AU，
				// 只记一次日志避免 40+ 帧/秒刷屏
				if !s.aacRTPOnce {
					s.aacRTPOnce = true
					log.Printf("[rtsp] AAC RTP decode error (subsequent ones suppressed): %v", err)
				}
				return
			}
			for _, au := range aus {
				frame, ferr := aacADTSFrame(au, s.aacFreqIndex, s.audioChannels)
				if ferr != nil {
					continue
				}
				pcm, derr := s.aacDec.Decode(frame)
				if derr != nil {
					// 比特流损坏会让流式解码器内部缓存持续出错：
					// 记一次日志并重建解码器，尝试从后续帧自愈；
					// 视频通路完全不受影响
					if !s.aacErrOnce {
						s.aacErrOnce = true
						log.Printf("[rtsp] AAC decode error, decoder rebuilt (subsequent ones suppressed): %v", derr)
					}
					if d := aacNewDecoder(); d != nil {
						s.aacDec = d
					}
					continue
				}
				if len(pcm) > 0 && s.audioPCMHandler != nil {
					s.audioPCMHandler(pcm16ToBytes(pcm), s.audioSampleRate, s.audioChannels)
				}
			}
		})
	}

	return nil
}

// Close tears the stream down and is safe to call multiple times (failed
// connect attempts already close the client; reconnect loops may close again).
func (s *Stream) Close() {
	s.closeOnce.Do(func() {
		if s.client != nil {
			s.client.Close()
		}
	})
}

// isIDRNAL reports whether the NAL unit is an IDR slice (type 5).
func isIDRNAL(nalu []byte) bool {
	return len(nalu) > 0 && nalu[0]&0x1F == 5
}

// WatchDisconnect must be called after a successful Connect: it invokes fn
// once the RTSP session ends, whether because the server went away or because
// Close() was called. Callers combine it with their own connection-generation
// guard to distinguish the two cases.
func (s *Stream) WatchDisconnect(fn func()) {
	if s.client == nil || fn == nil {
		return
	}
	go func() {
		_ = s.client.Wait()
		fn()
	}()
}

type rtspURLInfo struct {
	Scheme string
	Host   string
}

func parseRTSPURL(rawURL string) (rtspURLInfo, error) {
	s := rawURL
	scheme := "rtsp"
	if after, found := strings.CutPrefix(s, "rtsp://"); found {
		s = after
	} else if after, found := strings.CutPrefix(s, "rtsps://"); found {
		s = after
		scheme = "rtsps"
	}
	host := s
	if idx := strings.IndexByte(s, '/'); idx >= 0 {
		host = s[:idx]
	}
	return rtspURLInfo{Scheme: scheme, Host: host}, nil
}
