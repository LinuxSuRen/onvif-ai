package discovery

import (
	"strings"
	"testing"
)

// 鐪熷疄鐩告満甯歌鐨?wsdd: 鍓嶇紑 ProbeMatches 搴旂瓟
const wsddProbeMatches = `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope">
 <SOAP-ENV:Body>
  <wsdd:ProbeMatches xmlns:wsdd="http://schemas.xmlsoap.org/ws/2005/04/discovery">
   <wsdd:ProbeMatch>
    <wsa:EndpointReference xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing">
     <wsa:Address>urn:uuid:2108c95a-6bba-6bba-6bba-c0fdfba5eab6</wsa:Address>
    </wsa:EndpointReference>
    <wsdd:Types>dn:NetworkVideoTransmitter</wsdd:Types>
    <wsdd:Scopes>onvif://www.onvif.org/Profile/Streaming onvif://www.onvif.org/type/video_encoder</wsdd:Scopes>
    <wsdd:XAddrs>http://192.168.1.64:80/onvif/device_service</wsdd:XAddrs>
    <wsdd:MetadataVersion>1</wsdd:MetadataVersion>
   </wsdd:ProbeMatch>
  </wsdd:ProbeMatches>
 </SOAP-ENV:Body>
</SOAP-ENV:Envelope>`

const helloBody = `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope">
 <SOAP-ENV:Body>
  <d:Hello xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
   <a:EndpointReference><a:Address>urn:uuid:2108c95a-6bba-6bba-6bba-c0fdfba5eab6</a:Address></a:EndpointReference>
   <d:Types>dn:NetworkVideoTransmitter</d:Types>
   <d:Scopes>onvif://www.onvif.org/Profile/Streaming</d:Scopes>
   <d:XAddrs>http://192.168.1.65:80/onvif/device_service</d:XAddrs>
   <d:MetadataVersion>1</d:MetadataVersion>
  </d:Hello>
 </SOAP-ENV:Body>
</SOAP-ENV:Envelope>`

func TestParseDevicesProbeMatchesPrefixVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"wsdd prefix (as sent by many cameras)", wsddProbeMatches},
		{"d prefix", strings.NewReplacer("wsdd:", "d:").Replace(wsddProbeMatches)},
		{"no prefix", strings.NewReplacer("wsdd:", "").Replace(wsddProbeMatches)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devices := parseDevices(c.body)
			if len(devices) != 1 {
				t.Fatalf("expected 1 device, got %d", len(devices))
			}
			d := devices[0]
			if d.Address != "http://192.168.1.64:80/onvif/device_service" {
				t.Fatalf("unexpected address: %s", d.Address)
			}
			if len(d.Types) != 1 || d.Types[0] != "dn:NetworkVideoTransmitter" {
				t.Fatalf("unexpected types: %v", d.Types)
			}
			if len(d.Scopes) != 2 {
				t.Fatalf("unexpected scopes: %v", d.Scopes)
			}
			if d.MetadataVersion != 1 {
				t.Fatalf("unexpected metadata version: %d", d.MetadataVersion)
			}
		})
	}
}

func TestParseDevicesHello(t *testing.T) {
	devices := parseDevices(helloBody)
	if len(devices) != 1 {
		t.Fatalf("expected 1 device from Hello, got %d", len(devices))
	}
	if devices[0].Address != "http://192.168.1.65:80/onvif/device_service" {
		t.Fatalf("unexpected address: %s", devices[0].Address)
	}
}

func TestParseDevicesHelloWithoutPrefix(t *testing.T) {
	body := strings.NewReplacer("d:", "", "a:", "").Replace(helloBody)
	devices := parseDevices(body)
	if len(devices) != 1 {
		t.Fatalf("expected 1 device from prefix-less Hello, got %d", len(devices))
	}
}

func TestParseDevicesMultipleXAddrs(t *testing.T) {
	body := strings.Replace(wsddProbeMatches,
		"http://192.168.1.64:80/onvif/device_service",
		"http://192.168.1.64:80/onvif/device_service http://192.168.1.64:8000/onvif/device_service", 1)
	devices := parseDevices(body)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
}

func TestParseDevicesGarbage(t *testing.T) {
	for _, body := range []string{
		"",
		"not xml at all",
		`<?xml version="1.0"?><Envelope><Body><Other>data</Other></Body></Envelope>`,
		`<?xml version="1.0"?><Envelope><Body><sayHello>x</sayHello></Body></Envelope>`,
	} {
		if devices := parseDevices(body); len(devices) != 0 {
			t.Fatalf("expected no devices for %q, got %d", body, len(devices))
		}
	}
}

func TestListenerStoresParsedDevices(t *testing.T) {
	l := NewListener()
	l.parseAndStore([]byte(wsddProbeMatches))
	l.parseAndStore([]byte(helloBody))

	devices := l.Devices()
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices stored, got %d", len(devices))
	}
	addresses := map[string]bool{}
	for _, d := range devices {
		addresses[d.Address] = true
	}
	if !addresses["http://192.168.1.64:80/onvif/device_service"] ||
		!addresses["http://192.168.1.65:80/onvif/device_service"] {
		t.Fatalf("missing expected addresses: %v", addresses)
	}
}
