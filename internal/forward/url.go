package forward

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// pathSegmentPattern 划定 RTSP 路径段的合法字符集：mediamtx 等服务器
// 直接用路径段做流名，中文、空格、斜杠等既不可读也可能引发路径歧义，
// 统一折叠为连字符。
var pathSegmentPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizePathSegment 清洗单个路径段：非法字符折叠为连字符、去首尾
// 分隔符、长度截断；清洗后为空则返回 fallback，保证路径永远非空。
func sanitizePathSegment(seg, fallback string) string {
	cleaned := strings.ToLower(strings.TrimSpace(pathSegmentPattern.ReplaceAllString(seg, "-")))
	cleaned = strings.Trim(cleaned, "-._")
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

// ForwardPath 生成 mediamtx 上的流路径：onvif-ai/<设备host>/<profile>。
// host 取自设备地址，profile 优先用可读名、无则用 token。路径稳定可读，
// 便于在流媒体服务器侧直接按路径订阅。
func ForwardPath(deviceHost, profileName, profileToken string) string {
	profile := sanitizePathSegment(profileName, "")
	if profile == "" {
		profile = sanitizePathSegment(profileToken, "profile")
	}
	return "onvif-ai/" + sanitizePathSegment(deviceHost, "device") + "/" + profile
}

// BuildTargetURL 把用户配置的目标地址与转发路径拼成完整推流地址：
//   - 缺 scheme 时默认 rtsp://（用户常只填 host:port）；
//   - UseTLS 时强制 rtsps 加密；
//   - 认证凭证放入 userinfo，gortsplib 收到 401 会自动完成重试；
//   - 用户填写的路径前缀保留，转发路径追加在其后。
func BuildTargetURL(cfg Config, path string) (string, error) {
	raw := strings.TrimSpace(cfg.TargetURL)
	if raw == "" {
		return "", fmt.Errorf("目标地址为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "rtsp://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("目标地址无效: %s", cfg.TargetURL)
	}
	switch u.Scheme {
	case "rtsp", "rtsps":
	default:
		return "", fmt.Errorf("目标地址仅支持 rtsp/rtsps: %s", u.Scheme)
	}
	if cfg.UseTLS {
		u.Scheme = "rtsps"
	}
	if cfg.Username != "" {
		// 显式凭证覆盖地址里可能内嵌的 userinfo
		u.User = url.UserPassword(cfg.Username, cfg.Password)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")
	return u.String(), nil
}

// normalizeDeviceAddr 把设备地址归一化为 scheme://host:port 形式，
// 作为 Manager 里的设备唯一键：连接入口与 WS-Discovery 返回的 XAddr
// 路径部分可能不同（/onvif/device_service 等），只比较服务端点。
func normalizeDeviceAddr(addr string) string {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// deviceHostFromAddr 提取设备地址的主机名（不含端口），用作转发路径里的
// 设备标识；解析失败时回退为清洗前的原值交由 sanitize 兜底。
func deviceHostFromAddr(addr string) string {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return addr
}
