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
// GetServices 固定回复空服务列表；其余动作回复 optionsBody。
func newSOAPTestServer(t *testing.T, optionsBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		inner := ""
		switch {
		case strings.Contains(string(body), "GetServices"):
			inner = "<GetServicesResponse></GetServicesResponse>"
		case strings.Contains(string(body), "GetConfigurationOptions"):
			inner = optionsBody
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
	srv := newSOAPTestServer(t, `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:PTZTimeout><tt:Min>PT1S</tt:Min><tt:Max>PT60S</tt:Max></tt:PTZTimeout>
			<tt:Spaces>
				<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:ContinuousZoomVelocitySpace>
				<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:AbsoluteZoomPositionSpace>
			</tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`)
	c := newTestClient(srv.URL)

	spaces, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || !supported {
		t.Fatalf("expected supported, got supported=%v err=%v", supported, err)
	}
	if spaces.PanTilt || !spaces.Zoom {
		t.Fatalf("expected panTilt=false zoom=true, got %+v", spaces)
	}
}

// TestGetConfigurationOptionsBoth 校验传统云台相机形态：Pan/Tilt 与
// Zoom 空间并存 → 均为 true。
func TestGetConfigurationOptionsBoth(t *testing.T) {
	srv := newSOAPTestServer(t, `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:Spaces>
				<tt:ContinuousPanTiltVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange><tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange></tt:ContinuousPanTiltVelocitySpace>
				<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange></tt:ContinuousZoomVelocitySpace>
			</tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`)
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
	srv := newSOAPTestServer(t, `<tptz:GetConfigurationOptionsResponse>
		<tptz:PTZConfigurationOptions>
			<tt:Spaces></tt:Spaces>
		</tptz:PTZConfigurationOptions>
	</tptz:GetConfigurationOptionsResponse>`)
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
	srv := newSOAPTestServer(t, `<tptz:GetConfigurationOptionsResponse></tptz:GetConfigurationOptionsResponse>`)
	c := newTestClient(srv.URL)

	_, supported, err := c.GetConfigurationOptions(context.Background(), "ptz_1")
	if err != nil || supported {
		t.Fatalf("expected unsupported without error, got supported=%v err=%v", supported, err)
	}
}
