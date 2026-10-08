package onvif

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newSOAPTestServer 起一个按动作回复指定 SOAP Body 的假 ONVIF 服务：
// GetServices 固定回复空服务列表，其余动作按 responses（key 为请求体
// 中的动作名）回复；未匹配的动作回复空 Body。
func newSOAPTestServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		inner := ""
		switch {
		case strings.Contains(string(body), "GetServices"):
			inner = "<GetServicesResponse></GetServicesResponse>"
		default:
			for action, resp := range responses {
				if strings.Contains(string(body), action) {
					inner = resp
					break
				}
			}
		}
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` + inner + `</s:Body></s:Envelope>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(addr string) *Client {
	return NewClient(Config{DeviceAddr: addr, Timeout: 5 * time.Second})
}

// TestGetConfigurationOptionsZoomOnly 校验手机端形态：仅上报
// ContinuousZoomVelocitySpace（无 PanTilt 空间）→ 云台 false、变焦 true。
func TestGetConfigurationOptionsZoomOnly(t *testing.T) {
	srv := newSOAPTestServer(t, map[string]string{"GetConfigurationOptions": `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:PTZTimeout><tt:Min>PT1S</tt:Min><tt:Max>PT60S</tt:Max></tt:PTZTimeout>
			<tt:Spaces>
				<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:ContinuousZoomVelocitySpace>
				<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:AbsoluteZoomPositionSpace>
			</tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`})
	c := newTestClient(srv.URL)

	spaces, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || !supported {
		t.Fatalf("expected supported, got supported=%v err=%v", supported, err)
	}
	if spaces.PanTilt || !spaces.Zoom {
		t.Fatalf("expected panTilt=false zoom=true, got %+v", spaces)
	}
	// 标准绝对位置空间（[0,1]，无自定义 URI）不构成倍率范围
	if spaces.HasRatio {
		t.Fatalf("standard absolute space must not be treated as ratio range, got %+v", spaces)
	}
}

// TestGetConfigurationOptionsZoomRatio 校验自定义倍率空间（zoom/ratio
// URI）的解析：Min/Max 即真实倍率范围，与标准空间并存时按 URI 区分。
func TestGetConfigurationOptionsZoomRatio(t *testing.T) {
	srv := newSOAPTestServer(t, map[string]string{"GetConfigurationOptions": `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:Spaces>
				<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:ContinuousZoomVelocitySpace>
				<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange><tt:URI>http://www.onvif.org/ver10/tptz/ZoomSpaces/AbsoluteZoomGenericSpace</tt:URI></tt:AbsoluteZoomPositionSpace>
				<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0.55</tt:Min><tt:Max>100</tt:Max></tt:XRange><tt:URI>http://www.linuxsuren.org/onvif/zoom/ratio</tt:URI></tt:AbsoluteZoomPositionSpace>
			</tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`})
	c := newTestClient(srv.URL)

	spaces, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || !supported {
		t.Fatalf("expected supported, got supported=%v err=%v", supported, err)
	}
	if !spaces.HasRatio || spaces.RatioMin != 0.55 || spaces.RatioMax != 100 {
		t.Fatalf("expected ratio range [0.55,100], got %+v", spaces)
	}
}

// TestPTZGetStatus 校验 GetStatus 归一位置的解析与「未上报位置」。
func TestPTZGetStatus(t *testing.T) {
	srv := newSOAPTestServer(t, map[string]string{"GetStatus": `<tptz:GetStatusResponse>
		<tptz:PTZStatus>
			<tt:Position><tt:Zoom x="0.625" space="http://www.onvif.org/ver10/tptz/ZoomSpaces/PositionGenericSpace"/></tt:Position>
			<tt:MoveStatus><tt:PanTilt>IDLE</tt:PanTilt><tt:Zoom>IDLE</tt:Zoom></tt:MoveStatus>
		</tptz:PTZStatus>
	</tptz:GetStatusResponse>`})
	c := newTestClient(srv.URL)

	pos, ok, err := c.PTZGetStatus(context.Background(), "profile_1")
	if err != nil || !ok || pos != 0.625 {
		t.Fatalf("expected pos=0.625 ok=true, got pos=%v ok=%v err=%v", pos, ok, err)
	}

	// 无 Position 节点（手机端未知位置时不报）→ ok=false 而非位置 0
	srv2 := newSOAPTestServer(t, map[string]string{"GetStatus": `<tptz:GetStatusResponse>
		<tptz:PTZStatus><tt:MoveStatus><tt:PanTilt>IDLE</tt:PanTilt><tt:Zoom>IDLE</tt:Zoom></tt:MoveStatus></tptz:PTZStatus>
	</tptz:GetStatusResponse>`})
	pos2, ok2, err2 := newTestClient(srv2.URL).PTZGetStatus(context.Background(), "profile_1")
	if err2 != nil || ok2 || pos2 != 0 {
		t.Fatalf("expected ok=false without error, got pos=%v ok=%v err=%v", pos2, ok2, err2)
	}
}

// TestGetConfigurationOptionsBoth 校验传统云台相机形态：Pan/Tilt 与
// Zoom 空间并存 → 均为 true。
func TestGetConfigurationOptionsBoth(t *testing.T) {
	srv := newSOAPTestServer(t, map[string]string{"GetConfigurationOptions": `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:Spaces>
				<tt:ContinuousPanTiltVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange><tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange></tt:ContinuousPanTiltVelocitySpace>
				<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:ContinuousZoomVelocitySpace>
			</tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`})
	c := newTestClient(srv.URL)

	spaces, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || !supported {
		t.Fatalf("expected supported, got supported=%v err=%v", supported, err)
	}
	if !spaces.PanTilt || !spaces.Zoom {
		t.Fatalf("expected panTilt=true zoom=true, got %+v", spaces)
	}
}

// TestGetConfigurationOptionsEmptySpaces 校验上报了空 Spaces（如手机
// 无变焦的前置）：能力采信为双双不可用（supported=true）。
func TestGetConfigurationOptionsEmptySpaces(t *testing.T) {
	srv := newSOAPTestServer(t, map[string]string{"GetConfigurationOptions": `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:Spaces></tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`})
	c := newTestClient(srv.URL)

	spaces, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || !supported {
		t.Fatalf("expected supported, got supported=%v err=%v", supported, err)
	}
	if spaces.PanTilt || spaces.Zoom {
		t.Fatalf("expected both false, got %+v", spaces)
	}
}

// TestGetConfigurationOptionsUnsupported 校验老设备形态：响应不含
// Spaces 节点 → supported=false，调用方回退全能力显示。
func TestGetConfigurationOptionsUnsupported(t *testing.T) {
	srv := newSOAPTestServer(t, map[string]string{"GetConfigurationOptions": `<tptz:GetConfigurationOptionsResponse></tptz:GetConfigurationOptionsResponse>`})
	c := newTestClient(srv.URL)

	_, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || supported {
		t.Fatalf("expected unsupported without error, got supported=%v err=%v", supported, err)
	}
}
