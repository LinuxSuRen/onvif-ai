package rtsp

import (
	"context"
	"math"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/onvif-ai/internal/audio"
)

// TestStreamG711Audio verifies the dynamic audio path end to end: an embedded
// RTSP server publishes a G.711 (PCMA) audio track, Stream connects as a
// reader and must deliver linear PCM via OnAudioPCM together with the
// sample rate / channel count negotiated from the SDP.
func TestStreamG711Audio(t *testing.T) {
	g711Form := &format.G711{
		PayloadTyp:   8,
		MULaw:        false,
		SampleRate:   8000,
		ChannelCount: 1,
	}
	medi := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{g711Form},
	}
	desc := &description.Session{Medias: []*description.Media{medi}}

	// gortsplib 不会回报实际监听地址，先自选一个空闲端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	handler := &audioTestServer{}
	srv := &gortsplib.Server{
		Handler:     handler,
		RTSPAddress: addr,
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	stream := &gortsplib.ServerStream{Server: srv, Desc: desc}
	if err := stream.Initialize(); err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	handler.stream = stream

	go handler.publishAudio(medi, g711Form)

	cl := NewStream("rtsp://" + addr + "/audio")
	type audioChunk struct {
		pcm      []byte
		rate     int
		channels int
	}
	chunks := make(chan audioChunk, 4)
	cl.OnAudioPCM(func(pcm []byte, rate, channels int) {
		select {
		case chunks <- audioChunk{pcm: pcm, rate: rate, channels: channels}:
		default:
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		err := cl.Connect()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer cl.Close()

	if !cl.HasAudio() {
		t.Fatal("expected stream to be detected as carrying audio")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	select {
	case chunk := <-chunks:
		if chunk.rate != 8000 {
			t.Fatalf("expected sample rate 8000 negotiated from SDP, got %d", chunk.rate)
		}
		if chunk.channels != 1 {
			t.Fatalf("expected 1 channel, got %d", chunk.channels)
		}
		// 交付的必须是展开后的 16 位线性 PCM（长度为压缩字节的 2 倍）
		if len(chunk.pcm) == 0 || len(chunk.pcm)%2 != 0 {
			t.Fatalf("expected non-empty 16-bit linear PCM, got %d bytes", len(chunk.pcm))
		}
		// A-law 往返存在量化损失（最坏约半个分段步长），幅度应基本保留
		orig := handler.originalSamples()
		if len(chunk.pcm) != len(orig) {
			t.Fatalf("expected %d PCM bytes, got %d", len(orig), len(chunk.pcm))
		}
		peakOrig, peakGot := 0, 0
		for i := 0; i+1 < len(chunk.pcm); i += 2 {
			o := int(int16(uint16(orig[i]) | uint16(orig[i+1])<<8))
			g := int(int16(uint16(chunk.pcm[i]) | uint16(chunk.pcm[i+1])<<8))
			if o > peakOrig {
				peakOrig = o
			}
			if g > peakGot {
				peakGot = g
			}
			if d := g - o; d < -600 || d > 600 {
				t.Fatalf("sample %d diverges beyond A-law quantization error: %d vs %d", i/2, o, g)
			}
		}
		if peakOrig > 0 && peakGot < peakOrig/2 {
			t.Fatalf("amplitude lost in decoding: original peak %d, got %d", peakOrig, peakGot)
		}
	case <-ctx.Done():
		t.Fatal("no audio chunk received in time")
	}
}

// audioTestServer serves one pre-created G.711 stream to every reader.
type audioTestServer struct {
	stream *gortsplib.ServerStream
	pcm    []byte
}

func (s *audioTestServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *audioTestServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *audioTestServer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *audioTestServer) originalSamples() []byte {
	return s.pcm
}

// publishAudio encodes a 20ms sine segment (8kHz mono) to A-law and writes it
// as RTP/G711 packets on the server stream (50 pps).
func (s *audioTestServer) publishAudio(medi *description.Media, form *format.G711) {
	enc, err := form.CreateEncoder()
	if err != nil {
		return
	}
	if err := enc.Init(); err != nil {
		return
	}

	const samplesPerPacket = 160 // 8000Hz × 20ms
	pcm := make([]byte, 0, samplesPerPacket*2)
	for i := 0; i < samplesPerPacket; i++ {
		v := int16(12000 * math.Sin(2*math.Pi*float64(i)/8)) // ~1kHz
		pcm = append(pcm, byte(v), byte(v>>8))
	}
	s.pcm = pcm

	g711Bytes, err := audio.EncodePCMToG711(pcm, audio.G711ALaw)
	if err != nil {
		return
	}

	var seq uint16
	var ts uint32
	for {
		pkts, err := enc.Encode(g711Bytes)
		if err != nil {
			return
		}
		for _, p := range pkts {
			p.SequenceNumber = seq
			p.Timestamp = ts
			_ = s.stream.WritePacketRTP(medi, p)
			seq++
		}
		ts += samplesPerPacket
		time.Sleep(20 * time.Millisecond)
	}
}
