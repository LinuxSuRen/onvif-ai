package rtsp

import "testing"

// 认证凭证以 userinfo 形式注入取流地址(见 onvif.WithCredentials),
// 拨号主机必须剥离 userinfo,否则 dial 报 "lookup user:pass@host: no such host"。
func TestParseRTSPURLWithUserInfo(t *testing.T) {
	cases := []struct {
		name, in, scheme, host string
	}{
		{"userinfo with encoded special chars",
			"rtsp://admin:p%40ss%3Aword@192.168.1.21:8554/cam/cam1", "rtsp", "192.168.1.21:8554"},
		{"plain userinfo",
			"rtsp://admin:123456@127.0.0.1:8554/cam/cam1", "rtsp", "127.0.0.1:8554"},
		{"no userinfo",
			"rtsp://127.0.0.1:8554/cam/cam1", "rtsp", "127.0.0.1:8554"},
		{"rtsps",
			"rtsps://admin:pw@example.com:8322/path", "rtsps", "example.com:8322"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := parseRTSPURL(tc.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if info.Scheme != tc.scheme || info.Host != tc.host {
				t.Fatalf("got %s://%s, want %s://%s", info.Scheme, info.Host, tc.scheme, tc.host)
			}
		})
	}
}

func TestParseRTSPURLInvalid(t *testing.T) {
	if _, err := parseRTSPURL("not a url"); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}
