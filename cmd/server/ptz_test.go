package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onvif-ai/internal/onvif"
)

// newMockOnvifServer 起一个记录 SOAP 请求体的假 ONVIF 服务：
// 任意动作都回复空 Body 的合法 SOAP 信封（GetServices 返回空服务列表，
// 客户端会把 PTZ 地址回退到同一测试服务）。
func newMockOnvifServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(buf))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body></s:Body></s:Envelope>`))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// findSoap 返回包含指定片段的最近一条 SOAP 请求体。
func findSOAP(t *testing.T, bodies *[]string, fragment string) string {
	t.Helper()
	for i := len(*bodies) - 1; i >= 0; i-- {
		if strings.Contains((*bodies)[i], fragment) {
			return (*bodies)[i]
		}
	}
	t.Fatalf("no SOAP request contains %q, got %d requests", fragment, len(*bodies))
	return ""
}

// TestHandlePTZMoveZoomAxis 校验变焦方向映射到 ContinuousMove 的 zoom 轴：
// 按住为 ±0.5/10s 兜底，轻点步进（step）为全速 ±1.0/0.8s 自停；
// 方向键映射保持原状不受影响。
func TestHandlePTZMoveZoomAxis(t *testing.T) {
	srv, bodies := newMockOnvifServer(t)
	cm := newTestCameraManager()
	cm.mu.Lock()
	cm.onvifClient = onvif.NewClient(onvif.Config{DeviceAddr: srv.URL, Timeout: 5 * time.Second})
	cm.units = []*camUnit{{token: "cam-back", name: "后摄", ptz: true}}
	cm.mu.Unlock()

	cases := []struct {
		direction   string
		step        bool
		panTilt     string // PanTilt 元素期望值
		zoom        string // Zoom 元素期望值
		wantTimeout string
	}{
		{"zoom_in", false, `<tt:PanTilt x="0.000000" y="0.000000"`, `<tt:Zoom x="0.500000"`, "PT10.0S"},
		{"zoom_out", false, `<tt:PanTilt x="0.000000" y="0.000000"`, `<tt:Zoom x="-0.500000"`, "PT10.0S"},
		// 轻点步进：全速 ±1.0 × 0.8s 一步，松开后补发、到期自停
		{"zoom_in", true, `<tt:PanTilt x="0.000000" y="0.000000"`, `<tt:Zoom x="1.000000"`, "PT0.8S"},
		{"zoom_out", true, `<tt:PanTilt x="0.000000" y="0.000000"`, `<tt:Zoom x="-1.000000"`, "PT0.8S"},
		{"up", false, `<tt:PanTilt x="0.000000" y="1.000000"`, `<tt:Zoom x="0.000000"`, "PT2.0S"},
	}
	for _, tc := range cases {
		cm.handlePTZMove("cam-back", tc.direction, tc.step)
		body := findSOAP(t, bodies, "ContinuousMove")
		if !strings.Contains(body, tc.panTilt) {
			t.Errorf("%s(step=%v): PanTilt mismatch in %s", tc.direction, tc.step, body)
		}
		if !strings.Contains(body, tc.zoom) {
			t.Errorf("%s(step=%v): Zoom axis mismatch (want %s) in %s", tc.direction, tc.step, tc.zoom, body)
		}
		if !strings.Contains(body, tc.wantTimeout) {
			t.Errorf("%s(step=%v): timeout mismatch (want %s) in %s", tc.direction, tc.step, tc.wantTimeout, body)
		}
		if !strings.Contains(body, "<tptz:ProfileToken>cam-back</tptz:ProfileToken>") {
			t.Errorf("%s(step=%v): wrong profile token in %s", tc.direction, tc.step, body)
		}
	}
}

// TestHandlePTZStop 校验松开变焦按钮走 PTZStop，且 Pan/Tilt/Zoom 一并停止。
func TestHandlePTZStop(t *testing.T) {
	srv, bodies := newMockOnvifServer(t)
	cm := newTestCameraManager()
	cm.mu.Lock()
	cm.onvifClient = onvif.NewClient(onvif.Config{DeviceAddr: srv.URL, Timeout: 5 * time.Second})
	cm.units = []*camUnit{{token: "cam-back", name: "后摄", ptz: true}}
	cm.mu.Unlock()

	cm.handlePTZStop("cam-back")

	body := findSOAP(t, bodies, "Stop")
	for _, want := range []string{"<tptz:PanTilt>true</tptz:PanTilt>", "<tptz:Zoom>true</tptz:Zoom>", "<tptz:ProfileToken>cam-back</tptz:ProfileToken>"} {
		if !strings.Contains(body, want) {
			t.Errorf("PTZStop missing %s in %s", want, body)
		}
	}
}

// TestHandlePTZStopWithoutCamera 校验未连接/无 PTZ 能力时停止命令静默返回。
func TestHandlePTZStopWithoutCamera(t *testing.T) {
	cm := newTestCameraManager()
	cm.handlePTZStop("none") // 不应 panic
}
