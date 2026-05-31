package onvif

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	_ "github.com/onvif-ai/internal/onvif/xsd"
)

type Client struct {
	config       Config
	http         *http.Client
	mediaXAddr   string
	ptzXAddr     string
	discovered   bool
}

func NewClient(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Client{
		config: cfg,
		http:   &http.Client{Timeout: cfg.Timeout},
	}
}

func (c *Client) deviceURL() string {
	addr := c.config.DeviceAddr
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return addr
}

func (c *Client) serviceURL(servicePath string) string {
	base := c.deviceURL()
	idx := strings.Index(base, "/onvif/")
	if idx > 0 {
		return base[:idx] + servicePath
	}
	return strings.TrimRight(base, "/") + servicePath
}

func (c *Client) GetCapabilities(ctx context.Context) (*Capabilities, error) {
	body := c.soapEnvelope(`
		<tds:GetCapabilities>
			<tds:Category>All</tds:Category>
		</tds:GetCapabilities>
	`)

	resp, err := c.soapCall(ctx, c.deviceURL(), deviceServiceURL, "GetCapabilities", body)
	if err != nil {
		return nil, err
	}

	var caps Capabilities
	if err := c.parseSOAPResponse(resp, "GetCapabilitiesResponse", &caps); err != nil {
		return nil, fmt.Errorf("parse capabilities: %w", err)
	}
	return &caps, nil
}

func (c *Client) discoverMediaURL(ctx context.Context) error {
	return c.discoverServices(ctx)
}

func (c *Client) discoverServices(ctx context.Context) error {
	if c.discovered {
		return nil
	}

	body := c.soapEnvelope(`<tds:GetServices><tds:IncludeCapability>true</tds:IncludeCapability></tds:GetServices>`)

	resp, err := c.soapCall(ctx, c.deviceURL(), c.deviceURL(), "GetServices", body)
	if err != nil {
		return fmt.Errorf("GetServices: %w", err)
	}

	var result struct {
		XMLName  xml.Name `xml:"GetServicesResponse"`
		Services []struct {
			Namespace string `xml:"Namespace"`
			XAddr     string `xml:"XAddr"`
		} `xml:"Service"`
	}
	if err := c.parseSOAPResponse(resp, "GetServicesResponse", &result); err != nil {
		return fmt.Errorf("parse services: %w", err)
	}

	for _, svc := range result.Services {
		if strings.Contains(svc.Namespace, "media") || strings.Contains(svc.Namespace, "Media") {
			c.mediaXAddr = svc.XAddr
		}
		if strings.Contains(svc.Namespace, "ptz") || strings.Contains(svc.Namespace, "PTZ") {
			c.ptzXAddr = svc.XAddr
		}
	}

	if c.mediaXAddr == "" {
		c.mediaXAddr = c.serviceURL("/onvif/media_service")
	}

	c.discovered = true
	return nil
}

func (c *Client) mediaURL() string {
	if c.mediaXAddr != "" {
		return c.mediaXAddr
	}
	return c.serviceURL("/onvif/media_service")
}

func (c *Client) ptzURL() string {
	if c.ptzXAddr != "" {
		return c.ptzXAddr
	}
	return c.serviceURL("/onvif/ptz_service")
}

func (c *Client) GetProfiles(ctx context.Context) ([]Profile, error) {
	c.discoverMediaURL(ctx)

	body := c.soapEnvelope(`<trt:GetProfiles/>`)

	resp, err := c.soapCall(ctx, c.deviceURL(), c.mediaURL(), "GetProfiles", body)
	if err != nil {
		return nil, err
	}

	var result struct {
		XMLName  xml.Name `xml:"GetProfilesResponse"`
		Profiles []struct {
			Token string `xml:"token,attr"`
			Name  string `xml:"Name"`
		} `xml:"Profiles"`
	}
	if err := c.parseSOAPResponse(resp, "GetProfilesResponse", &result); err != nil {
		return nil, fmt.Errorf("parse profiles: %w", err)
	}

	profiles := make([]Profile, len(result.Profiles))
	for i, p := range result.Profiles {
		profiles[i] = Profile{Token: p.Token, Name: p.Name}
	}
	return profiles, nil
}

func (c *Client) GetStreamURI(ctx context.Context, profileToken string) (*StreamURI, error) {
	c.discoverMediaURL(ctx)

	body := c.soapEnvelope(fmt.Sprintf(`
		<trt:GetStreamUri>
			<trt:StreamSetup>
				<tt:Stream>RTP-Unicast</tt:Stream>
				<tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport>
			</trt:StreamSetup>
			<trt:ProfileToken>%s</trt:ProfileToken>
		</trt:GetStreamUri>
	`, xmlEscape(profileToken)))

	resp, err := c.soapCall(ctx, c.deviceURL(), c.mediaURL(), "GetStreamUri", body)
	if err != nil {
		return nil, err
	}

	var result struct {
		XMLName xml.Name `xml:"GetStreamUriResponse"`
		MediaURI struct {
			URI                 string `xml:"Uri"`
			Timeout             string `xml:"Timeout"`
			InvalidAfterConnect bool   `xml:"InvalidAfterConnect"`
			InvalidAfterReboot  bool   `xml:"InvalidAfterReboot"`
		} `xml:"MediaUri"`
	}
	if err := c.parseSOAPResponse(resp, "GetStreamUriResponse", &result); err != nil {
		return nil, fmt.Errorf("parse stream URI: %w", err)
	}

	return &StreamURI{
		URI:                 result.MediaURI.URI,
		Timeout:             result.MediaURI.Timeout,
		InvalidAfterConnect: result.MediaURI.InvalidAfterConnect,
		InvalidAfterReboot:  result.MediaURI.InvalidAfterReboot,
	}, nil
}

func (c *Client) GetSnapshotURI(ctx context.Context, profileToken string) (string, error) {
	c.discoverMediaURL(ctx)

	body := c.soapEnvelope(fmt.Sprintf(`
		<trt:GetSnapshotUri>
			<trt:ProfileToken>%s</trt:ProfileToken>
		</trt:GetSnapshotUri>
	`, xmlEscape(profileToken)))

	resp, err := c.soapCall(ctx, c.deviceURL(), c.mediaURL(), "GetSnapshotUri", body)
	if err != nil {
		return "", err
	}

	var result struct {
		XMLName  xml.Name `xml:"GetSnapshotUriResponse"`
		MediaURI struct {
			URI string `xml:"Uri"`
		} `xml:"MediaUri"`
	}
	if err := c.parseSOAPResponse(resp, "GetSnapshotUriResponse", &result); err != nil {
		return "", fmt.Errorf("parse snapshot URI: %w", err)
	}
	return result.MediaURI.URI, nil
}

func (c *Client) soapEnvelope(body string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
	xmlns:tds="http://www.onvif.org/ver10/device/wsdl"
	xmlns:trt="http://www.onvif.org/ver10/media/wsdl"
	xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl"
	xmlns:tt="http://www.onvif.org/ver10/schema"
	xmlns:xsd="http://www.w3.org/2001/XMLSchema"
	xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
	<s:Body>%s</s:Body>
</s:Envelope>`, body)
}

func (c *Client) soapCall(ctx context.Context, deviceAddr, serviceURL, action, body string) ([]byte, error) {
	soapAction := fmt.Sprintf("http://www.onvif.org/ver10/%s/wsdl/%s", soapActionDomain(action), action)

	req, err := http.NewRequestWithContext(ctx, "POST", serviceURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	req.Header.Set("SOAPAction", soapAction)

	if c.config.Username != "" {
		c.setWSSecurity(req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SOAP call %s: %w", action, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read SOAP response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("SOAP call %s returned %d: %s", action, resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}

	return respBody, nil
}

func (c *Client) setWSSecurity(req *http.Request) {
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i*7 + 3)
	}
	created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	digestInput := string(nonce) + created + c.config.Password
	hash := sha1.Sum([]byte(digestInput))
	digest := base64.StdEncoding.EncodeToString(hash[:])

	wsse := fmt.Sprintf(`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
		<wsse:UsernameToken>
			<wsse:Username>%s</wsse:Username>
			<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>
			<wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</wsse:Nonce>
			<wsu:Created xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">%s</wsu:Created>
		</wsse:UsernameToken>
	</wsse:Security>`, xmlEscape(c.config.Username), digest, nonceB64, created)

	req.Header.Set("X-WSSE", wsse)
}

func (c *Client) parseSOAPResponse(body []byte, responseTag string, result interface{}) error {
	var envelope struct {
		XMLName xml.Name `xml:"http://www.w3.org/2003/05/soap-envelope Envelope"`
		Body    struct {
			InnerXML string `xml:",innerxml"`
		} `xml:"http://www.w3.org/2003/05/soap-envelope Body"`
	}
	if err := xml.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("unmarshal SOAP envelope: %w", err)
	}
	cleaned := stripNSPrefix(envelope.Body.InnerXML)
	return xml.Unmarshal([]byte("<root>"+cleaned+"</root>"), result)
}

func stripNSPrefix(xmlStr string) string {
	prefixes := []string{"tds:", "trt:", "tt:", "tptz:", "dn:", "d:", "a:", "s:", "xsd:", "xsi:", "wsse:", "wsu:"}
	for _, p := range prefixes {
		xmlStr = strings.ReplaceAll(xmlStr, "<"+p, "<")
		xmlStr = strings.ReplaceAll(xmlStr, "</"+p, "</")
	}
	return xmlStr
}

func (c *Client) PTZContinuousMove(ctx context.Context, profileToken string, pan, tilt, zoom float64, duration time.Duration) error {
	if err := c.discoverServices(ctx); err != nil {
		return err
	}

	ptzURL := c.ptzURL()
	log.Printf("[PTZ] Sending ContinuousMove to %s (pan=%.1f, tilt=%.1f, dur=%v)", ptzURL, pan, tilt, duration)
	if c.mediaXAddr != "" {
		base := c.deviceURL()
		if idx := strings.Index(base, "/onvif/"); idx > 0 {
			ptzURL = base[:idx] + "/onvif/ptz_service"
		}
	}

	body := c.soapEnvelope(fmt.Sprintf(`
		<tptz:ContinuousMove>
			<tptz:ProfileToken>%s</tptz:ProfileToken>
			<tptz:Velocity>
				<tt:PanTilt x="%f" y="%f" space="http://www.onvif.org/ver10/tptz/PanTiltSpaces/VelocityGenericSpace"/>
				<tt:Zoom x="%f" space="http://www.onvif.org/ver10/tptz/ZoomSpaces/VelocityGenericSpace"/>
			</tptz:Velocity>
			<tptz:Timeout>%s</tptz:Timeout>
		</tptz:ContinuousMove>
	`, xmlEscape(profileToken), pan, tilt, zoom, formatISO8601(duration)))

	resp, err := c.soapCall(ctx, c.deviceURL(), ptzURL, "ContinuousMove", body)
	if err != nil {
		return err
	}
	_ = resp
	return nil
}

func (c *Client) PTZStop(ctx context.Context, profileToken string) error {
	if err := c.discoverServices(ctx); err != nil {
		return err
	}

	ptzURL := c.ptzURL()
	if c.mediaXAddr != "" {
		base := c.deviceURL()
		if idx := strings.Index(base, "/onvif/"); idx > 0 {
			ptzURL = base[:idx] + "/onvif/ptz_service"
		}
	}

	body := c.soapEnvelope(fmt.Sprintf(`
		<tptz:Stop>
			<tptz:ProfileToken>%s</tptz:ProfileToken>
			<tptz:PanTilt>true</tptz:PanTilt>
			<tptz:Zoom>true</tptz:Zoom>
		</tptz:Stop>
	`, xmlEscape(profileToken)))

	resp, err := c.soapCall(ctx, c.deviceURL(), ptzURL, "Stop", body)
	if err != nil {
		return err
	}
	_ = resp
	return nil
}

func formatISO8601(d time.Duration) string {
	sec := d.Seconds()
	return fmt.Sprintf("PT%.1fS", sec)
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// ContinuousMove sends a PTZ continuous move command for the specified direction and duration.
// After the duration elapses, a Stop command is automatically sent.
func (c *Client) ContinuousMove(ctx context.Context, profileToken, direction string, duration time.Duration) error {
	var pan, tilt float64
	switch direction {
	case "left":
		pan = -0.5
	case "right":
		pan = 0.5
	case "up":
		tilt = 0.5
	case "down":
		tilt = -0.5
	default:
		return fmt.Errorf("unknown direction: %s", direction)
	}

	body := c.soapEnvelope(fmt.Sprintf(`
		<tptz:ContinuousMove>
			<tptz:ProfileToken>%s</tptz:ProfileToken>
			<tptz:Velocity>
				<tt:PanTilt x="%.1f" y="%.1f"/>
				<tt:Zoom x="0"/>
			</tptz:Velocity>
			<tptz:Timeout>PT1S</tptz:Timeout>
		</tptz:ContinuousMove>
	`, xmlEscape(profileToken), pan, tilt))

	_, err := c.soapCall(ctx, c.deviceURL(), c.deviceURL(), "ContinuousMove", body)
	if err != nil {
		return fmt.Errorf("ContinuousMove %s: %w", direction, err)
	}

	// Auto-stop after duration
	time.Sleep(duration)

	stopBody := c.soapEnvelope(fmt.Sprintf(`
		<tptz:Stop>
			<tptz:ProfileToken>%s</tptz:ProfileToken>
			<tptz:PanTilt>true</tptz:PanTilt>
			<tptz:Zoom>true</tptz:Zoom>
		</tptz:Stop>
	`, xmlEscape(profileToken)))

	_, err = c.soapCall(ctx, c.deviceURL(), c.deviceURL(), "Stop", stopBody)
	return err
}

func soapActionDomain(action string) string {
	deviceActions := map[string]bool{"GetCapabilities": true, "GetServices": true, "GetDeviceInformation": true}
	ptzActions := map[string]bool{"ContinuousMove": true, "Stop": true}
	if deviceActions[action] {
		return "device"
	}
	if ptzActions[action] {
		return "ptz"
	}
	return "media"
}
