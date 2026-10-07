package rtsp

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// TestStreamMJPEG verifies the MJPEG (RFC 2435) path end to end: an embedded
// RTSP server publishes a JPEG frame stream, Stream connects as a reader and
// must deliver complete, decodable JPEG images via OnVideoJPEG.
func TestStreamMJPEG(t *testing.T) {
	medi := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{&format.MJPEG{}},
	}
	desc := &description.Session{Medias: []*description.Media{medi}}

	// gortsplib 不会回报实际监听地址，先自选一个空闲端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	handler := &mjpegTestServer{}
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

	go handler.publishFrames(medi)

	cl := NewStream("rtsp://" + addr + "/mjpeg")
	jpegs := make(chan []byte, 4)
	cl.OnVideoJPEG(func(img []byte) { jpegs <- img })

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

	if !cl.IsMJPEG() {
		t.Fatal("expected stream to be detected as MJPEG")
	}
	if cl.HasAudio() {
		t.Fatal("MJPEG-only stream must not be detected as carrying audio")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	select {
	case img := <-jpegs:
		if len(img) < 2 || img[0] != 0xFF || img[1] != 0xD8 {
			t.Fatalf("delivered frame is not a JPEG: first bytes %v", img[:min(4, len(img))])
		}
		if _, err := jpeg.Decode(bytes.NewReader(img)); err != nil {
			t.Fatalf("delivered JPEG is not decodable: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("no MJPEG frame received in time")
	}
}

// mjpegTestServer serves one pre-created MJPEG stream to every reader.
type mjpegTestServer struct {
	stream *gortsplib.ServerStream
}

func (s *mjpegTestServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *mjpegTestServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *mjpegTestServer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// publishFrames encodes a moving color pattern to JPEG and writes it as
// RTP/MJPEG packets on the server stream (5 fps).
func (s *mjpegTestServer) publishFrames(medi *description.Media) {
	form, ok := medi.Formats[0].(*format.MJPEG)
	if !ok {
		return
	}
	enc, err := form.CreateEncoder()
	if err != nil {
		return
	}
	if err := enc.Init(); err != nil {
		return
	}

	var seq uint16
	var ts uint32
	shade := uint8(0)
	for {
		img := image.NewRGBA(image.Rect(0, 0, 160, 120))
		for y := 0; y < 120; y++ {
			for x := 0; x < 160; x++ {
				img.Set(x, y, color.RGBA{R: shade, G: uint8(x), B: uint8(y), A: 255})
			}
		}
		shade += 17

		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
			return
		}

		pkts, err := enc.Encode(buf.Bytes())
		if err != nil {
			return
		}
		for _, p := range pkts {
			p.SequenceNumber = seq
			p.Timestamp = ts
			_ = s.stream.WritePacketRTP(medi, p)
			seq++
		}
		ts += 90000 / 5

		time.Sleep(200 * time.Millisecond)
	}
}
