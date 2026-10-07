package rtsp

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// TestFindG711BackChannelMedia 校验回传轨识别：仅 IsBackChannel 标记的
// G.711 轨会被选中，普通音频轨（无回传标记）不算对讲通道。
func TestFindG711BackChannelMedia(t *testing.T) {
	desc := &description.Session{
		Medias: []*description.Media{
			{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{&format.G711{SampleRate: 8000, ChannelCount: 1}},
			},
		},
	}

	// 普通音频轨：不是回传通道
	media, g711 := findG711BackChannelMedia(desc)
	if media != nil || g711 != nil {
		t.Fatalf("expected no backchannel media for plain audio track, got %v", media)
	}

	// 标记回传后应被选中
	desc.Medias[0].IsBackChannel = true
	media, g711 = findG711BackChannelMedia(desc)
	if media == nil || g711 == nil {
		t.Fatal("expected backchannel media to be found")
	}
	if media != desc.Medias[0] {
		t.Fatal("expected the marked media to be selected")
	}
	if g711.SampleRate != 8000 {
		t.Fatalf("unexpected sample rate: %d", g711.SampleRate)
	}
}

// TestWritePCMNotConnected 校验未连接时写入直接报错，
// 不会触碰编码器与底层连接。
func TestWritePCMNotConnected(t *testing.T) {
	b := NewBackchannel("rtsp://127.0.0.1:1/test")
	if err := b.WritePCM(make([]byte, 320)); err == nil {
		t.Fatal("expected error when writing to unconnected backchannel")
	}
}
