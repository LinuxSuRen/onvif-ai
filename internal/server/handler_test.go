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

// TestCameraStatePTZCapsJSON 校验 device_state 中按能力区分的云台/变焦
// 字段契约：显式序列化（false 也必须下发，前端据此隐藏对应控制）。
func TestCameraStatePTZCapsJSON(t *testing.T) {
	// 手机后摄：仅变焦，无云台 → 方向键隐藏
	b, err := json.Marshal(DeviceState{Cameras: []CameraState{{Token: "t1", PTZSupported: true, PTZPanTilt: false, PTZZoom: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ptz_pan_tilt":false`) || !strings.Contains(string(b), `"ptz_zoom":true`) {
		t.Fatalf("ptz capability fields expected, got %s", b)
	}

	// 零值也必须显式携带（不可 omitempty）
	b, err = json.Marshal(DeviceState{Cameras: []CameraState{{Token: "t1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ptz_pan_tilt":false`) || !strings.Contains(string(b), `"ptz_zoom":false`) {
		t.Fatalf("explicit false caps expected, got %s", b)
	}
}

// TestDeviceStateTalkbackJSON 校验 device_state 中对讲通道字段的序列化契约：
// nil 省略（未协商）、不可用时显式携带原因码、可用时仅 available。
func TestDeviceStateTalkbackJSON(t *testing.T) {
	// nil：字段省略
	b, err := json.Marshal(DeviceState{Connected: true, Address: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"talkback"`) {
		t.Fatalf("nil talkback must be omitted, got %s", b)
	}

	// 设备无回传轨：available=false + no_backchannel
	b, err = json.Marshal(DeviceState{Talkback: &TalkbackState{Available: false, Reason: "no_backchannel"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"talkback":{"available":false,"reason":"no_backchannel"}`) {
		t.Fatalf("explicit no-backchannel state expected, got %s", b)
	}

	// 可用：仅 available=true
	b, err = json.Marshal(DeviceState{Talkback: &TalkbackState{Available: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"talkback":{"available":true}`) {
		t.Fatalf("available state expected, got %s", b)
	}

	// 前端解析方向：JSON 还原回结构
	var st DeviceState
	if err := json.Unmarshal([]byte(`{"connected":true,"talkback":{"available":true}}`), &st); err != nil {
		t.Fatal(err)
	}
	if st.Talkback == nil || !st.Talkback.Available || st.Talkback.Reason != "" {
		t.Fatalf("unexpected round-trip: %+v", st.Talkback)
	}
}
