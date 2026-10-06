package rtsp

import (
	"fmt"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/pion/rtp"
)

type Stream struct {
	rawURL string
	client *gortsplib.Client

	h264Dec *rtph264.Decoder
	g711Dec *rtplpcm.Decoder

	// SPS/PPS from the SDP. Many encoders put the parameter sets only in the
	// SDP (sprop-parameter-sets) and never resend them in-band; downstream
	// muxers (e.g. jmuxer in the browser) cannot initialize without them,
	// so we re-inject them ahead of every IDR frame.
	sps []byte
	pps []byte

	closeOnce sync.Once

	videoNALHandler func([]byte)
	audioPCMHandler func([]byte)
}

func NewStream(rtspURL string) *Stream {
	return &Stream{rawURL: rtspURL}
}

func (s *Stream) OnVideoNAL(handler func([]byte)) {
	s.videoNALHandler = handler
}

func (s *Stream) OnAudioPCM(handler func([]byte)) {
	s.audioPCMHandler = handler
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
	var videoFormat *format.H264
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
					videoFormat = ft
				}
			case *format.G711:
				if media.Type == description.MediaTypeAudio && audioMedia == nil {
					audioMedia = media
					audioFormat = ft
				}
			}
		}
	}

	if videoMedia != nil && videoFormat != nil {
		if _, err := s.client.Setup(baseURL, videoMedia, 0, 0); err != nil {
			s.Close()
			return fmt.Errorf("setup video: %w", err)
		}
		dec, err := videoFormat.CreateDecoder()
		if err != nil {
			s.Close()
			return fmt.Errorf("create H264 decoder: %w", err)
		}
		dec.Init()
		s.h264Dec = dec
		s.sps = videoFormat.SPS
		s.pps = videoFormat.PPS
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
		needsSetup = true
	}

	if needsSetup {
		if _, err := s.client.Play(nil); err != nil {
			s.Close()
			return fmt.Errorf("play: %w", err)
		}
	}

	if s.h264Dec != nil {
		s.client.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
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

	if s.g711Dec != nil {
		s.client.OnPacketRTP(audioMedia, audioFormat, func(pkt *rtp.Packet) {
			pcm, err := s.g711Dec.Decode(pkt)
			if err != nil || s.audioPCMHandler == nil {
				return
			}
			s.audioPCMHandler(pcm)
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
