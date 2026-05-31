package discovery

import (
	"encoding/xml"
	"fmt"
	"log"
	"net"
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
	body := string(data)

	hasHello := strings.Contains(body, "Hello")
	hasProbeMatch := strings.Contains(body, "ProbeMatch")
	if !hasHello && !hasProbeMatch {
		return
	}

	startTag := "<d:ProbeMatches>"
	endTag := "</d:ProbeMatches>"
	if hasHello {
		startTag = "<d:Hello>"
		endTag = "</d:Hello>"
	}

	start := strings.Index(body, startTag)
	end := strings.Index(body, endTag)
	if start < 0 || end < 0 {
		return
	}

	xmlBody := body[start : end+len(endTag)]
	xmlBody = strings.ReplaceAll(xmlBody, "d:", "")
	xmlBody = strings.ReplaceAll(xmlBody, "dn:", "")
	xmlBody = strings.ReplaceAll(xmlBody, "a:", "")

	var matches probeMatchesResponse
	if hasHello {
		matches = probeMatchesResponse{}
		xml.Unmarshal([]byte("<root>"+xmlBody+"</root>"), &matches)
	}

	for _, pm := range matches.ProbeMatches {
		xaddrs := strings.Fields(pm.XAddrs)
		for _, addr := range xaddrs {
			l.addDevice(Device{
				Address:         addr,
				Types:           strings.Fields(pm.Types),
				Scopes:          strings.Fields(pm.Scopes),
				XAddrs:          xaddrs,
				MetadataVersion: pm.MetadataVersion,
			})
		}
	}

	if hasHello {
		pm := probeMatch{}
		if err := xml.Unmarshal([]byte("<root>"+xmlBody+"</root>"), &pm); err == nil {
			xaddrs := strings.Fields(pm.XAddrs)
			for _, addr := range xaddrs {
				l.addDevice(Device{
					Address:         addr,
					Types:           strings.Fields(pm.Types),
					Scopes:          strings.Fields(pm.Scopes),
					XAddrs:          xaddrs,
					MetadataVersion: pm.MetadataVersion,
				})
			}
		}
	}
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

		body := string(buf[:n])
		startTag := "<d:ProbeMatches>"
		endTag := "</d:ProbeMatches>"
		start := strings.Index(body, startTag)
		end := strings.Index(body, endTag)
		if start < 0 || end < 0 {
			continue
		}

		xmlBody := body[start : end+len(endTag)]
		xmlBody = strings.ReplaceAll(xmlBody, "d:", "")
		xmlBody = strings.ReplaceAll(xmlBody, "dn:", "")

		var resp probeMatchesResponse
		if err := xml.Unmarshal([]byte(xmlBody), &resp); err != nil {
			continue
		}

		for _, pm := range resp.ProbeMatches {
			xaddrs := strings.Fields(pm.XAddrs)
			for _, addr := range xaddrs {
				key := addr
				mu.Lock()
				if _, exists := devices[key]; !exists {
					devices[key] = Device{
						Address:         addr,
						Types:           strings.Fields(pm.Types),
						Scopes:          strings.Fields(pm.Scopes),
						XAddrs:          xaddrs,
						MetadataVersion: pm.MetadataVersion,
					}
				}
				mu.Unlock()
			}
		}
	}

	result := make([]Device, 0, len(devices))
	for _, d := range devices {
		result = append(result, d)
	}
	return result, nil
}

var Discover = Probe
