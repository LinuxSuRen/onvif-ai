package discovery

import (
	"net"
	"testing"
)

// TestIsVirtualIface 锁定虚拟网卡黑名单的子串语义（仅优化用途，
// 正确性不依赖它——见 Probe 的逐接口重试与兜底）。
func TestIsVirtualIface(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"vEthernet (WSL)", true},   // Windows/WSL FriendlyName
		{"Docker0", true},           // 大小写不敏感
		{"tailscale0", true},        // 小写
		{"Bluetooth PAN", true},     // 含 bluetooth
		{"utun4", true},             // macOS（tun 子串）
		{"eth0", false},             // Linux 物理
		{"en0", false},              // macOS 物理
		{"Wi-Fi", false},            // Windows 无线
		{"Ethernet", false},         // Windows 有线
	}
	for _, tc := range cases {
		if got := isVirtualIface(tc.name); got != tc.want {
			t.Fatalf("isVirtualIface(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCandidateIPsExplicitIP 显式 IP 透传：合法 IPv4 原样作为唯一候选。
func TestCandidateIPsExplicitIP(t *testing.T) {
	ips, err := candidateIPs("192.0.2.10")
	if err != nil || len(ips) != 1 || ips[0] != "192.0.2.10" {
		t.Fatalf("candidateIPs(192.0.2.10) = %v, %v; want [192.0.2.10], nil", ips, err)
	}
}

// TestCandidateIPsExplicitInvalid 显式指定接口/IP 无效必须报错：
// 静默回退全网卡探测会丢弃「只在指定接口探测」的语义，配置错误无感知。
func TestCandidateIPsExplicitInvalid(t *testing.T) {
	// 不存在的接口名
	if _, err := candidateIPs("onvif-test-no-such-iface"); err == nil {
		t.Fatal("bogus iface name should error, got nil")
	}
	// IPv6 输入当前不支持，必须报错而非静默回退
	if _, err := candidateIPs("fe80::1"); err == nil {
		t.Fatal("ipv6 input should error, got nil")
	}
	// 存在但无可用 IPv4 的接口（回环：ipv4Addrs 过滤 127.x）
	lo := loopbackIface(t)
	if _, err := candidateIPs(lo.Name); err == nil {
		t.Fatalf("loopback iface %q (no usable ipv4) should error, got nil", lo.Name)
	}
}

// TestCandidateIPsEnumerate 空参数枚举：候选不含回环与链路本地地址。
func TestCandidateIPsEnumerate(t *testing.T) {
	ips, err := candidateIPs("")
	if err != nil {
		t.Fatalf("candidateIPs(\"\") error: %v", err)
	}
	for _, ip := range ips {
		p := net.ParseIP(ip)
		if p == nil || p.To4() == nil {
			t.Fatalf("candidate %q is not IPv4", ip)
		}
		if p.IsLoopback() {
			t.Fatalf("candidate %q is loopback", ip)
		}
		if p.IsLinkLocalUnicast() {
			t.Fatalf("candidate %q is link-local", ip)
		}
	}
}

// TestIPv4AddrsFilters 用回环接口（跨平台存在）锁定过滤规则：
// 回环与链路本地地址不得进入候选。
func TestIPv4AddrsFilters(t *testing.T) {
	if got := ipv4Addrs(loopbackIface(t)); len(got) != 0 {
		t.Fatalf("ipv4Addrs(loopback) = %v, want empty", got)
	}
}

// loopbackIface 取本机回环接口（跨平台：按 FlagLoopback 查找而非写死名字）。
func loopbackIface(t *testing.T) *net.Interface {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 {
			return &ifaces[i]
		}
	}
	t.Skip("no loopback interface found")
	return nil
}
