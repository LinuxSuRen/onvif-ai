package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onvif-ai/internal/forward"
	"github.com/onvif-ai/internal/ws"
)

// newForwardTestHandler 构造带转发配置种子的 handler 与路由。
func newForwardTestHandler(t *testing.T) (*Handler, http.Handler) {
	t.Helper()
	hub := ws.NewHub()
	go hub.Run()

	h := NewHandler(hub, nil)
	h.SetForwardConfig(&ForwardConfig{
		Enabled:           true,
		TargetURL:         "rtsp://192.168.1.10:8554",
		Username:          "admin",
		Password:          "super-secret-password",
		UseTLS:            false,
		AutoDiscover:      false,
		DiscoverIntervalS: 30,
	})
	return h, h.RegisterRoutes()
}

// TestForwardConfigGetMasksPassword 校验 GET 脱敏：密码不出现明文，
// 其余字段原样返回。
func TestForwardConfigGetMasksPassword(t *testing.T) {
	_, router := newForwardTestHandler(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/forward/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret-password") {
		t.Fatalf("password leaked: %s", body)
	}
	var cfg ForwardConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetURL != "rtsp://192.168.1.10:8554" || cfg.Username != "admin" || !cfg.Enabled {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Password == "" || !strings.Contains(cfg.Password, "***") {
		t.Fatalf("password should be masked, got %q", cfg.Password)
	}
}

// TestForwardConfigSetValidation 校验 PUT 校验规则：启用但缺地址、
// 地址非法均被 400 拒绝。
func TestForwardConfigSetValidation(t *testing.T) {
	_, router := newForwardTestHandler(t)

	cases := []struct {
		name, body, wantErr string
	}{
		{"启用缺地址", `{"enabled":true}`, "必须填写流媒体服务器地址"},
		{"启用非法地址", `{"enabled":true,"target_url":"http://x"}`, "rtsp"},
		{"非法 host", `{"enabled":true,"target_url":"rtsp://"}`, "目标地址无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/forward/config", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Fatalf("error %q should contain %q", rec.Body.String(), tc.wantErr)
			}
		})
	}
}

// TestForwardConfigSetTriggersUpdate 校验热更新链路：PUT 成功后回调携带
// 合并后的完整配置（密码未回传明文时保留旧值），handler 内部状态同步。
func TestForwardConfigSetTriggersUpdate(t *testing.T) {
	h, router := newForwardTestHandler(t)

	got := make(chan ForwardConfig, 2)
	h.SetForwardUpdateCallback(func(c ForwardConfig) { got <- c })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/forward/config",
		strings.NewReader(`{"enabled":true,"target_url":"192.168.1.20:8554","username":"u","password":"***","use_tls":true,"auto_discover":true,"discover_interval":45}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case c := <-got:
		// 密码回传的是 GET 的脱敏值，必须保留旧密码
		if c.Password != "super-secret-password" {
			t.Fatalf("password should be preserved, got %q", c.Password)
		}
		if !c.UseTLS || !c.AutoDiscover || c.DiscoverIntervalS != 45 {
			t.Fatalf("unexpected merged config: %+v", c)
		}
		if c.TargetURL != "192.168.1.20:8554" {
			t.Fatalf("target should update, got %q", c.TargetURL)
		}
		// ToForward 换算契约：秒 → Duration
		if fc := c.ToForward(); fc.DiscoverInterval != 45*time.Second || !fc.UseTLS {
			t.Fatalf("ToForward conversion wrong: %+v", fc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("update callback not fired")
	}

	// UI 不回传发现周期（非正数）：保留种子值而非冲回默认
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/forward/config",
		strings.NewReader(`{"enabled":true,"target_url":"192.168.1.20:8554"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("second put status = %d", rec.Code)
	}
	select {
	case c := <-got:
		if c.DiscoverIntervalS != 45 {
			t.Fatalf("discover interval should be preserved, got %d", c.DiscoverIntervalS)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second update callback not fired")
	}
}

// TestForwardStatusEndpoint 校验状态端点：未注入 provider 返回空集且
// enabled 标志随配置；注入后透传 Manager 状态。
func TestForwardStatusEndpoint(t *testing.T) {
	h, router := newForwardTestHandler(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/forward/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Enabled  bool             `json:"enabled"`
		Forwards []forward.Status `json:"forwards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled || len(resp.Forwards) != 0 {
		t.Fatalf("unexpected empty status: %+v", resp)
	}

	h.SetForwardStatusProvider(func() []forward.Status {
		return []forward.Status{{
			Source: "rtsp://cam/stream",
			Path:   "onvif-ai/192.168.1.21/front-door",
			State:  forward.StateRunning,
		}}
	})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/forward/status", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Forwards) != 1 || resp.Forwards[0].Path != "onvif-ai/192.168.1.21/front-door" {
		t.Fatalf("unexpected status: %+v", resp)
	}
}
