package discovery

import (
	"encoding/xml"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	multicastAddr = "239.255.255.250:3702"
	probeTimeout  = 3 * time.Second
)

type Device struct {
	Address         string   `json:"address"`
	Types           []string `json:"types"`
	Scopes          []string `json:"scopes"`
	XAddrs          []string `json:"xaddrs"`
	MetadataVersion int      `json:"metadata_version"`
}

type probeMatch struct {
	XMLName         xml.Name `xml:"ProbeMatch"`
	Types           string   `xml:"Types"`
	Scopes          string   `xml:"Scopes"`
	XAddrs          string   `xml:"XAddrs"`
	MetadataVersion int      `xml:"MetadataVersion"`
}

type probeMatchesResponse struct {
	XMLName      xml.Name     `xml:"ProbeMatches"`
	ProbeMatches []probeMatch `xml:"ProbeMatch"`
}

type Listener struct {
	conn    *net.UDPConn
	devices map[string]Device
	mu      sync.RWMutex
	stopCh  chan struct{}
}

func NewListener() *Listener {
	return &Listener{
		devices: make(map[string]Device),
		stopCh:  make(chan struct{}),
	}
}

func (l *Listener) Start() error {
	mcastAddr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return fmt.Errorf("resolve multicast: %w", err)
	}

	conn, err := net.ListenMulticastUDP("udp4", nil, mcastAddr)
	if err != nil {
		return fmt.Errorf("listen multicast: %w", err)
	}

	l.conn = conn

	go l.listenLoop()

	return nil
}

func (l *Listener) Stop() {
	close(l.stopCh)
	if l.conn != nil {
		l.conn.Close()
	}
}

func (l *Listener) Devices() []Device {
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]Device, 0, len(l.devices))
	for _, d := range l.devices {
		result = append(result, d)
	}
	return result
}

func (l *Listener) listenLoop() {
	buf := make([]byte, 65535)

	for {
		select {
		case <-l.stopCh:
			return
		default:
		}

		l.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "closed") {
				continue
			}
			log.Printf("Discovery listener error: %v", err)
			continue
		}

		l.parseAndStore(buf[:n])
	}
}

func (l *Listener) parseAndStore(data []byte) {
	for _, d := range parseDevices(string(data)) {
		l.addDevice(d)
	}
}

// 相机对 WS-Discovery 命名空间前缀的选择并不统一（d:、wsdd:、无前缀都常见），
// 解析必须与具体前缀解耦：块提取用「可选前缀」正则，反序列化前统一剥离前缀。
var (
	nsPrefixPattern   = regexp.MustCompile(`(?i)(</?)[A-Za-z_][\w.\-]*:`)
	probeMatchesBlock = regexp.MustCompile(`(?is)<(?:[A-Za-z_][\w.\-]*:)?ProbeMatches\b[^>]*>.*?</(?:[A-Za-z_][\w.\-]*:)?ProbeMatches\s*>`)
	helloBlock        = regexp.MustCompile(`(?is)<(?:[A-Za-z_][\w.\-]*:)?Hello\b[^>]*>.*?</(?:[A-Za-z_][\w.\-]*:)?Hello\s*>`)
)

// stripNSPrefix 去掉元素名上的任意命名空间前缀（<wsdd:ProbeMatch> → <ProbeMatch>）。
func stripNSPrefix(s string) string {
	return nsPrefixPattern.ReplaceAllString(s, "$1")
}

// helloMessage 对应 WS-Discovery Hello 消息，携带字段与 ProbeMatch 相同。
type helloMessage struct {
	XMLName         xml.Name `xml:"Hello"`
	Types           string   `xml:"Types"`
	Scopes          string   `xml:"Scopes"`
	XAddrs          string   `xml:"XAddrs"`
	MetadataVersion int      `xml:"MetadataVersion"`
}

func devicesFrom(types, scopes, xaddrs string, metadataVersion int) (devices []Device) {
	fields := strings.Fields(xaddrs)
	for _, addr := range fields {
		devices = append(devices, Device{
			Address:         addr,
			Types:           strings.Fields(types),
			Scopes:          strings.Fields(scopes),
			XAddrs:          fields,
			MetadataVersion: metadataVersion,
		})
	}
	return
}

// parseDevices 从一条 SOAP 报文中提取设备，兼容任意命名空间前缀，
// 支持 ProbeMatches 与 Hello 两种消息。
func parseDevices(body string) (devices []Device) {
	if block := probeMatchesBlock.FindString(body); block != "" {
		var resp probeMatchesResponse
		if err := xml.Unmarshal([]byte(stripNSPrefix(block)), &resp); err == nil {
			for _, pm := range resp.ProbeMatches {
				devices = append(devices, devicesFrom(pm.Types, pm.Scopes, pm.XAddrs, pm.MetadataVersion)...)
			}
		}
	}
	if block := helloBlock.FindString(body); block != "" {
		var hello helloMessage
		if err := xml.Unmarshal([]byte(stripNSPrefix(block)), &hello); err == nil {
			devices = append(devices, devicesFrom(hello.Types, hello.Scopes, hello.XAddrs, hello.MetadataVersion)...)
		}
	}
	return
}

func (l *Listener) addDevice(d Device) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.devices[d.Address]; !exists {
		l.devices[d.Address] = d
		log.Printf("Discovered ONVIF device: %s", d.Address)
	}
}

var probeTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://www.w3.org/2003/05/soap-envelope"
	xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"
	xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"
	xmlns:dn="http://www.onvif.org/ver10/network/wsdl">
	<Header>
		<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>
		<a:MessageID>urn:uuid:%s</a:MessageID>
		<a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To>
	</Header>
	<Body>
		<d:Probe>
			<d:Types>dn:NetworkVideoTransmitter</d:Types>
		</d:Probe>
	</Body>
</Envelope>`

func Probe(ifaceName string) ([]Device, error) {
	msgID := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		time.Now().UnixNano()&0xFFFFFFFF,
		time.Now().UnixNano()>>32&0xFFFF,
		time.Now().UnixNano()>>48&0xFFFF,
		0x4000|(time.Now().UnixNano()>>52&0x0FFF),
		time.Now().UnixNano()>>16&0xFFFFFFFFFFFF)

	probe := fmt.Sprintf(probeTemplate, msgID)

	mcastAddr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve multicast: %w", err)
	}

	// 多网卡机器上，绑定 0.0.0.0 时 Windows 会按组播路由表选接口，
	// 可能选中不可达的虚拟网卡导致 wsasendto: unreachable（实测），
	// Probe 一个包都发不出去。改为逐个候选源地址绑定发送：
	// 绑定源 IP 后组播从对应接口发出，发送失败的接口直接跳过。
	var conns []net.PacketConn
	for _, ip := range candidateIPs(ifaceName) {
		conn, err := net.ListenPacket("udp4", ip+":0")
		if err != nil {
			continue
		}
		if _, err := conn.WriteTo([]byte(probe), mcastAddr); err != nil {
			_ = conn.Close()
			continue
		}
		conns = append(conns, conn)
	}
	if len(conns) == 0 {
		// 兜底退回历史行为（单网卡机器上依旧可用）。
		conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
		if err != nil {
			return nil, fmt.Errorf("listen UDP: %w", err)
		}
		if _, err := conn.WriteTo([]byte(probe), mcastAddr); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("send probe: %w", err)
		}
		conns = append(conns, conn)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	devices := make(map[string]Device)
	var mu sync.Mutex

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c net.PacketConn) {
			defer wg.Done()
			buf := make([]byte, 65535)
			for {
				_ = c.SetReadDeadline(time.Now().Add(probeTimeout))
				n, _, err := c.ReadFrom(buf)
				if err != nil {
					return // 超时或连接关闭
				}
				mu.Lock()
				for _, d := range parseDevices(string(buf[:n])) {
					if _, exists := devices[d.Address]; !exists {
						devices[d.Address] = d
					}
				}
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	result := make([]Device, 0, len(devices))
	for _, d := range devices {
		result = append(result, d)
	}
	return result, nil
}

// virtualIfaceHints 常见虚拟/隧道网卡的名称特征（小写包含匹配）。
// 这些接口通常不具备可用的组播路径，作为候选源地址只会徒增失败发送。
var virtualIfaceHints = []string{
	"vethernet", "wsl", "virtualbox", "vbox", "tailscale",
	"docker", "bluetooth", "蓝牙", "loopback", "tap", "tun",
}

func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, h := range virtualIfaceHints {
		if strings.Contains(n, h) {
			return true
		}
	}
	return false
}

// candidateIPs 返回探测应绑定的本机 IPv4 源地址：
//   - ifaceName 为 IP 时直接使用；为接口名时取该接口的 IPv4；
//   - 为空时枚举所有 up 的非回环物理候选接口（过滤常见虚拟网卡）。
func candidateIPs(ifaceName string) []string {
	if ifaceName != "" {
		if ip := net.ParseIP(ifaceName); ip != nil && ip.To4() != nil {
			return []string{ip.String()}
		}
		if iface, err := net.InterfaceByName(ifaceName); err == nil {
			return ipv4Addrs(iface)
		}
		return nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualIface(ifc.Name) {
			continue
		}
		ips = append(ips, ipv4Addrs(&ifc)...)
	}
	return ips
}

func ipv4Addrs(iface *net.Interface) []string {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var ips []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, ipn.IP.String())
	}
	return ips
}

var Discover = Probe
