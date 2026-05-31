package rtsp

import (
	"fmt"
	"net/url"

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
	u, err := url.Parse(s.rawURL)
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
			return fmt.Errorf("setup video: %w", err)
		}
		dec, err := videoFormat.CreateDecoder()
		if err != nil {
			return fmt.Errorf("create H264 decoder: %w", err)
		}
		dec.Init()
		s.h264Dec = dec
		needsSetup = true
	}

	if audioMedia != nil && audioFormat != nil {
		if _, err := s.client.Setup(baseURL, audioMedia, 0, 0); err != nil {
			return fmt.Errorf("setup audio: %w", err)
		}
		dec, err := audioFormat.CreateDecoder()
		if err != nil {
			return fmt.Errorf("create G711 decoder: %w", err)
		}
		dec.Init()
		s.g711Dec = dec
		needsSetup = true
	}

	if needsSetup {
		if _, err := s.client.Play(nil); err != nil {
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

func (s *Stream) Close() {
	if s.client != nil {
		s.client.Close()
	}
}
