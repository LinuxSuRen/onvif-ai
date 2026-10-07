package rtsp

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
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
	return s.g711Dec != nil
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
	var audioFormat *format.G711
	needsSetup := false

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
			case *format.G711:
				if media.Type == description.MediaTypeAudio && audioMedia == nil {
					audioMedia = media
					audioFormat = ft
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

	if audioMedia != nil && audioFormat != nil {
		if _, err := s.client.Setup(baseURL, audioMedia, 0, 0); err != nil {
			s.Close()
			return fmt.Errorf("setup audio: %w", err)
		}
		dec, err := audioFormat.CreateDecoder()
		if err != nil {
			s.Close()
			return fmt.Errorf("create G711 decoder: %w", err)
		}
		dec.Init()
		s.g711Dec = dec
		s.audioSampleRate = audioFormat.SampleRate
		s.audioChannels = audioFormat.ChannelCount
		needsSetup = true
	} else {
		// 源带音频轨道但编码不受支持（如 AAC）时明确告警而非静默丢弃，
		// 方便排查“画面正常但没有声音”的情况；无音频轨道则不打印。
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
		mulaw := audioFormat.MULaw
		s.client.OnPacketRTP(audioMedia, audioFormat, func(pkt *rtp.Packet) {
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
