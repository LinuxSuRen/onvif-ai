// Package forward 把已有 RTSP 流转发到外部流媒体服务器（如 mediamtx）：
// 从摄像头直拉源流，把相同的媒体轨集合原样转推到目标服务器，源或目标
// 不可达时按指数退避自动重试，恢复后继续推送。
package forward

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultDiscoverInterval 是自动发现 ONVIF 设备的默认周期。
const DefaultDiscoverInterval = 30 * time.Second

// Config 是转发功能的运行配置。环境变量只作为启动默认值，API 可在运行时
// 覆盖；覆盖仅保存在内存中，进程重启后回落到环境变量（与 LLM 配置一致）。
type Config struct {
	// Enabled 总开关：关闭时停止所有转发与自动发现
	Enabled bool
	// TargetURL 是流媒体服务器地址，如 rtsp://192.168.1.10:8554，
	// 允许省略 scheme（默认 rtsp）与路径前缀
	TargetURL string
	// Username/Password 是流媒体服务器的发布认证凭证（可空）
	Username string
	Password string
	// UseTLS 为 true 时用 rtsps 加密推送
	UseTLS bool
	// AutoDiscover 为 true 时定期主动探测 ONVIF 设备并自动转发新设备
	AutoDiscover bool
	// DiscoverInterval 是自动发现周期，非正值按 DefaultDiscoverInterval
	DiscoverInterval time.Duration
}

// ConfigFromEnv 从环境变量读取默认配置：
// MEDIAMTX_URL / MEDIAMTX_USERNAME / MEDIAMTX_PASSWORD / MEDIAMTX_TLS /
// FORWARD_AUTODISCOVER / FORWARD_DISCOVER_INTERVAL。
func ConfigFromEnv() Config {
	interval := DefaultDiscoverInterval
	if raw := strings.TrimSpace(os.Getenv("FORWARD_DISCOVER_INTERVAL")); raw != "" {
		// 同时接受 "45s" 这类时长与 "45" 这类纯秒数，降低配置心智
		if d, err := time.ParseDuration(raw); err == nil {
			interval = d
		} else if secs, err := strconv.Atoi(raw); err == nil {
			interval = time.Duration(secs) * time.Second
		}
	}
	return Config{
		TargetURL:        strings.TrimSpace(os.Getenv("MEDIAMTX_URL")),
		Username:         os.Getenv("MEDIAMTX_USERNAME"),
		Password:         os.Getenv("MEDIAMTX_PASSWORD"),
		UseTLS:           parseBoolEnv(os.Getenv("MEDIAMTX_TLS")),
		AutoDiscover:     parseBoolEnv(os.Getenv("FORWARD_AUTODISCOVER")),
		DiscoverInterval: interval,
	}
}

// parseBoolEnv 宽松解析布尔环境变量（1/true/yes/t 均视为开）。
func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "t":
		return true
	default:
		return false
	}
}
