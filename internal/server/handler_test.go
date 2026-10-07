package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDeviceStateAudioJSON 校验 device_state 中音频字段的序列化契约：
// nil 省略（未知/未协商）、Available=false 显式携带（明确“无音频”）、
// 可用时携带编码与参数，前端按此渲染音频状态。
func TestDeviceStateAudioJSON(t *testing.T) {
	// nil：字段省略
	b, err := json.Marshal(DeviceState{Connected: true, Address: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"audio"`) {
		t.Fatalf("nil audio must be omitted, got %s", b)
	}

	// 无音频：audio.available=false 必须显式出现
	b, err = json.Marshal(DeviceState{Audio: &AudioState{Available: false}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"audio":{"available":false}`) {
		t.Fatalf("explicit no-audio state expected, got %s", b)
	}

	// 接收中：编码 + 采样率 + 声道
	b, err = json.Marshal(DeviceState{Audio: &AudioState{
		Available:  true,
		Codec:      "AAC-LC",
		SampleRate: 44100,
		Channels:   2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"codec":"AAC-LC"`, `"sample_rate":44100`, `"channels":2`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("expected %s in %s", want, b)
		}
	}

	// 降级：degraded + reason
	b, err = json.Marshal(DeviceState{Audio: &AudioState{
		Available: true,
		Codec:     "G.711",
		Degraded:  true,
		Reason:    "音频解码失败",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"degraded":true`) {
		t.Fatalf("degraded flag expected, got %s", b)
	}

	// 前端解析方向：JSON 还原回结构
	var st DeviceState
	if err := json.Unmarshal([]byte(`{"connected":true,"audio":{"available":true,"codec":"G.711","sample_rate":8000,"channels":1}}`), &st); err != nil {
		t.Fatal(err)
	}
	if st.Audio == nil || !st.Audio.Available || st.Audio.Codec != "G.711" || st.Audio.SampleRate != 8000 || st.Audio.Channels != 1 {
		t.Fatalf("unexpected round-trip: %+v", st.Audio)
	}
}

// TestCameraStateResolutionJSON 校验摄像头条目分辨率字段的序列化契约：
// 未知时省略，已知时携带 width/height。
func TestCameraStateResolutionJSON(t *testing.T) {
	b, err := json.Marshal(DeviceState{Cameras: []CameraState{{Token: "t1", Name: "cam"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"width"`) || strings.Contains(string(b), `"height"`) {
		t.Fatalf("unknown resolution must be omitted, got %s", b)
	}

	b, err = json.Marshal(DeviceState{Cameras: []CameraState{{Token: "t1", Width: 1280, Height: 720}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"width":1280`) || !strings.Contains(string(b), `"height":720`) {
		t.Fatalf("resolution expected, got %s", b)
	}
}
