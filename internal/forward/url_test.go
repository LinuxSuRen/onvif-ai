package forward

import "testing"

// TestForwardPath 校验 mediamtx 路径的命名契约：稳定、可读、段内合法。
func TestForwardPath(t *testing.T) {
	cases := []struct {
		name, host, profName, token, want string
	}{
		{"中文名清洗为空回落 token", "192.168.1.21", "门前走廊", "prof1", "onvif-ai/192.168.1.21/prof1"},
		{"名称清洗后为空回落 token", "192.168.1.21", "中文名", "Profile_1", "onvif-ai/192.168.1.21/profile_1"},
		{"英文名保留并转小写", "192.168.1.21", "Front Door", "tok", "onvif-ai/192.168.1.21/front-door"},
		{"无名无 token 兜底", "cam.local", "", "", "onvif-ai/cam.local/profile"},
		{"host 含斜杠被清洗", "a/b", "cam", "t1", "onvif-ai/a-b/cam"},
		{"token 带点保留", "10.0.0.8", "", "main.stream", "onvif-ai/10.0.0.8/main.stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ForwardPath(tc.host, tc.profName, tc.token)
			if got != tc.want {
				t.Fatalf("ForwardPath(%q,%q,%q) = %q, want %q", tc.host, tc.profName, tc.token, got, tc.want)
			}
		})
	}
}

// TestBuildTargetURL 校验目标推流地址构造：scheme 补全、TLS、认证 userinfo、
// 路径前缀追加各变体。
func TestBuildTargetURL(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "基础: rtsp 地址追加路径",
			cfg:  Config{TargetURL: "rtsp://192.168.1.10:8554"},
			path: "onvif-ai/192.168.1.21/cam1",
			want: "rtsp://192.168.1.10:8554/onvif-ai/192.168.1.21/cam1",
		},
		{
			name: "缺 scheme 默认 rtsp",
			cfg:  Config{TargetURL: "192.168.1.10:8554"},
			path: "onvif-ai/h/cam1",
			want: "rtsp://192.168.1.10:8554/onvif-ai/h/cam1",
		},
		{
			name: "认证进 userinfo（密码特殊字符需转义）",
			cfg:  Config{TargetURL: "rtsp://192.168.1.10:8554", Username: "admin", Password: "p@ss:word"},
			path: "p/cam",
			want: "rtsp://admin:p%40ss%3Aword@192.168.1.10:8554/p/cam",
		},
		{
			name: "TLS 强制 rtsps",
			cfg:  Config{TargetURL: "rtsp://media.example.com:8322", UseTLS: true},
			path: "p/cam",
			want: "rtsps://media.example.com:8322/p/cam",
		},
		{
			name: "TLS 与认证叠加",
			cfg:  Config{TargetURL: "media.example.com", UseTLS: true, Username: "u", Password: "pw"},
			path: "p/cam",
			want: "rtsps://u:pw@media.example.com/p/cam",
		},
		{
			name: "已有路径前缀保留",
			cfg:  Config{TargetURL: "rtsp://h:8554/prefix/"},
			path: "p/cam",
			want: "rtsp://h:8554/prefix/p/cam",
		},
		{
			name:    "空地址报错",
			cfg:     Config{TargetURL: "  "},
			wantErr: true,
		},
		{
			name:    "非 rtsp scheme 报错",
			cfg:     Config{TargetURL: "http://192.168.1.10:8080"},
			wantErr: true,
		},
		{
			name:    "无 host 报错",
			cfg:     Config{TargetURL: "rtsp://"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildTargetURL(tc.cfg, tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildTargetURL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeDeviceAddr 校验设备键归一化：连接入口与发现 XAddr 的路径
// 差异必须归并到同一键，否则同一设备会被当成两台重复转发。
func TestNormalizeDeviceAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://192.168.1.21:80/onvif/device_service", "http://192.168.1.21:80"},
		{"http://192.168.1.21:80/onvif/media_service", "http://192.168.1.21:80"},
		{"192.168.1.21:80/onvif/device_service", "http://192.168.1.21:80"},
		{"192.168.1.21", "http://192.168.1.21"},
	}
	for _, tc := range cases {
		if got := normalizeDeviceAddr(tc.in); got != tc.want {
			t.Fatalf("normalizeDeviceAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDeviceHostFromAddr 校验转发路径里的设备标识取主机名（去端口去路径）。
func TestDeviceHostFromAddr(t *testing.T) {
	if got := deviceHostFromAddr("http://192.168.1.21:80/onvif/device_service"); got != "192.168.1.21" {
		t.Fatalf("got %q", got)
	}
	if got := deviceHostFromAddr("192.168.1.21:8899"); got != "192.168.1.21" {
		t.Fatalf("got %q", got)
	}
}

// TestRedactUserinfo 校验对外展示的 URL 不携带凭证：源地址与推流地址
// 都可能内嵌 user:pass，状态 API 与日志不允许原样外泄。
func TestRedactUserinfo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"rtsp://admin:secretpw@192.168.1.21:554/Streaming/Channels/101", "rtsp://192.168.1.21:554/Streaming/Channels/101"},
		{"rtsp://admin@192.168.1.21:554/cam", "rtsp://192.168.1.21:554/cam"},
		{"rtsps://mediamtx:8554/onvif-ai/192.168.1.21/cam1", "rtsps://mediamtx:8554/onvif-ai/192.168.1.21/cam1"}, // 无凭证原样返回
		{"", ""},
		{"192.168.1.21:554", "192.168.1.21:554"}, // 无 scheme 不动
	}
	for _, tc := range cases {
		if got := RedactUserinfo(tc.in); got != tc.want {
			t.Fatalf("RedactUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
