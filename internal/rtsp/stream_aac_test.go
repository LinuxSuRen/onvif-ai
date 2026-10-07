package rtsp

import (
	"context"
	"math"
	"net"
	"testing"
	"time"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/adts"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

// TestStreamAAC verifies the dynamic AAC path end to end: an embedded RTSP
// server publishes an MPEG4-generic (RFC 3640) AAC-LC track, Stream connects
// as a reader and must deliver linear PCM via OnAudioPCM together with the
// sample rate / channel count negotiated from the SDP.
func TestStreamAAC(t *testing.T) {
	const sampleRate = 44100
	const channels = 2

	asc := &mpeg4audio.AudioSpecificConfig{
		Type:         mpeg4audio.ObjectTypeAACLC,
		SampleRate:   sampleRate,
		ChannelCount: channels,
	}
	aacForm := &format.MPEG4Audio{
		PayloadTyp:       97,
		Config:           asc,
		SizeLength:       13,
		IndexLength:      3,
		IndexDeltaLength: 3,
	}
	medi := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{aacForm},
	}
	desc := &description.Session{Medias: []*description.Media{medi}}

	// gortsplib 不会回报实际监听地址，先自选一个空闲端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	handler := &aacTestServer{}
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

	go handler.publishAAC(medi, aacForm)

	cl := NewStream("rtsp://" + addr + "/audio")
	type audioChunk struct {
		pcm      []byte
		rate     int
		channels int
	}
	chunks := make(chan audioChunk, 8)
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

	// 编码器存在前导/淡入帧：连续读多帧，取峰值幅度断言
	const inputPeak = 12000
	peak := 0
	frames := 0
	for frames < 8 {
		select {
		case chunk := <-chunks:
			if chunk.rate != sampleRate {
				t.Fatalf("expected sample rate %d negotiated from SDP, got %d", sampleRate, chunk.rate)
			}
			if chunk.channels != channels {
				t.Fatalf("expected %d channels, got %d", channels, chunk.channels)
			}
			// 交错立体声 16 位线性 PCM：每帧 1024 采样 × 2 声道 × 2 字节
			if len(chunk.pcm) == 0 || len(chunk.pcm)%4 != 0 {
				t.Fatalf("expected interleaved stereo 16-bit PCM, got %d bytes", len(chunk.pcm))
			}
			for i := 0; i+1 < len(chunk.pcm); i += 2 {
				v := int(int16(uint16(chunk.pcm[i]) | uint16(chunk.pcm[i+1])<<8))
				if v > peak {
					peak = v
				}
			}
			frames++
		case <-ctx.Done():
			t.Fatalf("only %d AAC chunks received in time", frames)
		}
	}
	// AAC 有损压缩，1kHz 正弦的峰值幅度应基本保留
	if peak < 6000 {
		t.Fatalf("decoded amplitude lost: input peak %d, got %d", inputPeak, peak)
	}
}

// aacTestServer serves one pre-created AAC stream to every reader.
type aacTestServer struct {
	stream *gortsplib.ServerStream
}

func (s *aacTestServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *aacTestServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *aacTestServer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// publishAAC encodes a continuous 1kHz stereo sine to AAC-LC and writes it
// as RTP MPEG4-generic packets on the server stream (~23ms per AU).
func (s *aacTestServer) publishAAC(medi *description.Media, form *format.MPEG4Audio) {
	rtpEnc, err := form.CreateEncoder()
	if err != nil {
		return
	}
	if err := rtpEnc.Init(); err != nil {
		return
	}

	enc, err := aac.NewEncoder(aac.Config{
		SampleRate: 44100,
		Channels:   2,
		Quality:    0.9,
	})
	if err != nil {
		return
	}

	const blockSamples = 1024
	block := make([]int16, 0, blockSamples*2)
	for i := 0; i < blockSamples; i++ {
		l := int16(12000 * math.Sin(2*math.Pi*float64(i)/44.1))
		r := int16(11000 * math.Sin(2*math.Pi*float64(i)/44.1*0.99))
		block = append(block, l, r)
	}

	var seq uint16
	var ts uint32
	for {
		// 编码器输出为 ADTS 帧；剥掉传输头得到 RFC 3640 需要的裸 AU
		adtsFrames, err := enc.Encode(block)
		if err != nil {
			return
		}
		for len(adtsFrames) > 0 {
			header, err := adts.Parse(adtsFrames)
			if err != nil {
				return
			}
			au := adtsFrames[header.PayloadOffset():header.FrameLength]
			adtsFrames = adtsFrames[header.FrameLength:]

			pkts, err := rtpEnc.Encode([][]byte{au})
			if err != nil {
				return
			}
			for _, p := range pkts {
				p.SequenceNumber = seq
				p.Timestamp = ts
				_ = s.stream.WritePacketRTP(medi, p)
				seq++
			}
			ts += blockSamples
		}
		time.Sleep(time.Millisecond * 1000 * blockSamples / 44100)
	}
}
