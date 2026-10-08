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

// newMockOnvifServer 起一个记录 SOAP 请求体的假 ONVIF 服务：GetServices
// 返回空服务列表（客户端把 PTZ 地址回退到同一测试服务）；GetStatus 返回
// 归一变焦位置 0.5；其余动作回复空 Body 的合法 SOAP 信封。
func newMockOnvifServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(buf))
		mu.Unlock()
		inner := ""
		if strings.Contains(string(buf), "GetStatus") {
			inner = `<tptz:GetStatusResponse><tptz:PTZStatus>` +
				`<tt:Position><tt:Zoom x="0.500000" space="http://www.onvif.org/ver10/tptz/ZoomSpaces/PositionGenericSpace"/></tt:Position>` +
				`<tt:MoveStatus><tt:PanTilt>IDLE</tt:PanTilt><tt:Zoom>IDLE</tt:Zoom></tt:MoveStatus>` +
				`</tptz:PTZStatus></tptz:GetStatusResponse>`
		}
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` + inner + `</s:Body></s:Envelope>`))
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

// TestHandlePTZStatus 校验变焦状态查询：GetStatus 归一位置 0.5，带
// 自定义倍率范围 [0.55,100] 时换算 ratio=0.55+0.5×99.45；无倍率范围
// 时 Ratio 为 nil（前端回退百分比）。
func TestHandlePTZStatus(t *testing.T) {
	srv, bodies := newMockOnvifServer(t)
	cm := newTestCameraManager()
	cm.mu.Lock()
	cm.onvifClient = onvif.NewClient(onvif.Config{DeviceAddr: srv.URL, Timeout: 5 * time.Second})
	cm.units = []*camUnit{{
		token: "cam-back", name: "后摄", ptz: true, ptzZoom: true,
		hasZoomRng: true, zoomRngMin: 0.55, zoomRngMax: 100,
	}}
	cm.mu.Unlock()

	result := cm.handlePTZStatus("cam-back")
	findSOAP(t, bodies, "GetStatus")
	if result == nil || result.Camera != "cam-back" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Position == nil || *result.Position != 0.5 {
		t.Fatalf("expected position 0.5, got %+v", result)
	}
	if result.Ratio == nil {
		t.Fatalf("expected ratio, got %+v", result)
	}
	if want := 0.55 + 0.5*(100-0.55); *result.Ratio < want-1e-9 || *result.Ratio > want+1e-9 {
		t.Fatalf("expected ratio %v, got %v", want, *result.Ratio)
	}

	// 无自定义倍率范围：仅归一位置，Ratio 为 nil
	cm.mu.Lock()
	cm.units[0].hasZoomRng = false
	cm.mu.Unlock()

	result = cm.handlePTZStatus("cam-back")
	if result.Position == nil || *result.Position != 0.5 || result.Ratio != nil {
		t.Fatalf("expected position-only result, got %+v", result)
	}

	// 未连接（无客户端/无摄像头）：仅回 camera，不携带位置。
	// 注：token 未命中时按现有回退语义落到第一路，故用全新 manager。
	fresh := newTestCameraManager()
	result = fresh.handlePTZStatus("cam-back")
	if result == nil || result.Camera != "cam-back" || result.Position != nil || result.Ratio != nil {
		t.Fatalf("expected bare result, got %+v", result)
	}
}
