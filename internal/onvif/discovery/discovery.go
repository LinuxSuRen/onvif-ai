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

	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	defer conn.Close()

	if _, err := conn.WriteTo([]byte(probe), mcastAddr); err != nil {
		return nil, fmt.Errorf("send probe: %w", err)
	}

	devices := make(map[string]Device)
	var mu sync.Mutex

	buf := make([]byte, 65535)
	conn.SetReadDeadline(time.Now().Add(probeTimeout))

	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}

		for _, d := range parseDevices(string(buf[:n])) {
			mu.Lock()
			if _, exists := devices[d.Address]; !exists {
				devices[d.Address] = d
			}
			mu.Unlock()
		}
	}

	result := make([]Device, 0, len(devices))
	for _, d := range devices {
		result = append(result, d)
	}
	return result, nil
}

var Discover = Probe
