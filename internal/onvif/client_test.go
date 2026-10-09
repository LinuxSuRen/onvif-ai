package onvif

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testUser = "admin"
const testPass = "s3cret:p@ss"

/* ---------- 服务端校验器:与 ohos-ipcam-streamer 设备端同口径 ---------- */

var (
	reUsername = regexp.MustCompile(`<([A-Za-z0-9]+:)?Username[^>]*>([^<]+)<`)
	rePassword = regexp.MustCompile(`<([A-Za-z0-9]+:)?Password[^>]*>([^<]+)<`)
	reNonce    = regexp.MustCompile(`<([A-Za-z0-9]+:)?Nonce[^>]*>([^<]+)<`)
	reCreated  = regexp.MustCompile(`<([A-Za-z0-9]+:)?Created[^>]*>([^<]+)<`)
)

func xmlText(xmlStr string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(xmlStr)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[2])
}

/* verifyToken 重算 Base64(SHA1(nonce+Created+口令)) 并做时间窗校验 */
func verifyToken(envelope string) bool {
	if xmlText(envelope, reUsername) != testUser {
		return false
	}
	digest := xmlText(envelope, rePassword)
	nonceB64 := xmlText(envelope, reNonce)
	created := xmlText(envelope, reCreated)
	if digest == "" || nonceB64 == "" || created == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil || time.Since(t) > 5*time.Minute {
		return false
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return false
	}
	sum := sha1.Sum(append(append(append([]byte{}, nonce...), []byte(created)...), []byte(testPass)...))
	return base64.StdEncoding.EncodeToString(sum[:]) == digest
}

func soapEnvelopeOf(inner string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
	xmlns:tds="http://www.onvif.org/ver10/device/wsdl"
	xmlns:trt="http://www.onvif.org/ver10/media/wsdl"
	xmlns:tt="http://www.onvif.org/ver10/schema">
	<s:Body>` + inner + `</s:Body>
</s:Envelope>`
}

func deviceTimeBody() string {
	now := time.Now().UTC()
	return soapEnvelopeOf(fmt.Sprintf(
		`<tds:GetSystemDateAndTimeResponse><tds:SystemDateTime><tds:UTCDateTime>`+
			`<tdt:Year>%d</tdt:Year><tdt:Month>%d</tdt:Month><tdt:Day>%d</tdt:Day>`+
			`<tdt:Hour>%d</tdt:Hour><tdt:Minute>%d</tdt:Minute><tdt:Second>%d</tdt:Second>`+
			`</tds:UTCDateTime></tds:SystemDateTime></tds:GetSystemDateAndTimeResponse>`,
		now.Year(), int(now.Month()), now.Day(), now.Hour(), now.Minute(), now.Second()))
}

/* newAuthDevice 模拟开启认证的 ONVIF 设备:
对时免认证,其余操作要求合法 UsernameToken,否则 HTTP 401 */
func newAuthDevice(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		body := string(raw)

		if strings.Contains(body, "GetSystemDateAndTime") {
			w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
			fmt.Fprint(w, deviceTimeBody())
			return
		}
		if !verifyToken(body) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		if strings.Contains(body, "GetDeviceInformation") {
			fmt.Fprint(w, soapEnvelopeOf(
				`<tds:GetDeviceInformationResponse>`+
					`<td:Manufacturer>linuxsuren</td:Manufacturer><td:Model>test-cam</td:Model>`+
					`<td:FirmwareVersion>1.0</td:FirmwareVersion><td:SerialNumber>SN1</td:SerialNumber>`+
					`<td:HardwareId>HW1</td:HardwareId></tds:GetDeviceInformationResponse>`))
			return
		}
		fmt.Fprint(w, soapEnvelopeOf(`<tds:GetServicesResponse></tds:GetServicesResponse>`))
	}))
}

func TestClientWsSecurityDigestAccepted(t *testing.T) {
	srv := newAuthDevice(t)
	defer srv.Close()

	c := NewClient(Config{DeviceAddr: srv.URL, Username: testUser, Password: testPass})
	info, err := c.GetDeviceInformation(context.Background())
	if err != nil {
		t.Fatalf("GetDeviceInformation with credentials: %v", err)
	}
	if info.Manufacturer != "linuxsuren" || info.SerialNumber != "SN1" {
		t.Fatalf("unexpected device info: %+v", info)
	}
}

func TestClientUnauthorizedWithoutCredentials(t *testing.T) {
	srv := newAuthDevice(t)
	defer srv.Close()

	c := NewClient(Config{DeviceAddr: srv.URL})
	_, err := c.GetDeviceInformation(context.Background())
	if err == nil {
		t.Fatal("expected auth error without credentials")
	}
	if !strings.Contains(err.Error(), "认证") {
		t.Fatalf("error should mention 认证, got: %v", err)
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error should unwrap to *AuthError, got %T: %v", err, err)
	}
	if authErr.CredentialsProvided {
		t.Fatal("CredentialsProvided should be false without credentials")
	}
}

func TestClientWrongPasswordRejected(t *testing.T) {
	srv := newAuthDevice(t)
	defer srv.Close()

	c := NewClient(Config{DeviceAddr: srv.URL, Username: testUser, Password: "wrong"})
	_, err := c.GetDeviceInformation(context.Background())
	if err == nil {
		t.Fatal("expected auth failure with wrong password")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention 401, got: %v", err)
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error should unwrap to *AuthError, got %T: %v", err, err)
	}
	if !authErr.CredentialsProvided {
		t.Fatal("CredentialsProvided should be true when credentials were sent")
	}
}

func TestBuildSecurityHeaderDigestFormula(t *testing.T) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	created := "2026-01-02T03:04:05Z"
	header := buildSecurityHeader(testUser, testPass, nonce, created)

	if !strings.Contains(header, "<s:Header>") || !strings.Contains(header, "</wsse:Security></s:Header>") {
		t.Fatalf("header must live in SOAP Header block: %s", header)
	}
	if !strings.Contains(header, `s:mustUnderstand="1"`) {
		t.Fatalf("Security header must carry mustUnderstand=1")
	}
	sum := sha1.Sum(append(append(append([]byte{}, nonce...), []byte(created)...), []byte(testPass)...))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if !strings.Contains(header, ">"+want+"</wsse:Password>") {
		t.Fatalf("digest mismatch: want %s in %s", want, header)
	}
	if got := xmlText(header, reNonce); got != base64.StdEncoding.EncodeToString(nonce) {
		t.Fatalf("nonce encoding mismatch: %s", got)
	}
}

func TestWithCredentials(t *testing.T) {
	cases := []struct {
		name, in, user, pass, want string
	}{
		{
			name: "special chars percent-encoded",
			in:   "rtsp://192.168.1.10:8554/cam0", user: "admin", pass: "p@ss:word/1",
			want: "rtsp://admin:p%40ss%3Aword%2F1@192.168.1.10:8554/cam0",
		},
		{
			name: "plain creds",
			in:   "rtsp://192.168.1.10:8554/cam0", user: "admin", pass: "123456",
			want: "rtsp://admin:123456@192.168.1.10:8554/cam0",
		},
		{
			name: "no username unchanged",
			in:   "rtsp://192.168.1.10:8554/cam0", user: "", pass: "123456",
			want: "rtsp://192.168.1.10:8554/cam0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithCredentials(tc.in, tc.user, tc.pass); got != tc.want {
				t.Fatalf("WithCredentials = %q, want %q", got, tc.want)
			}
		})
	}
}

/* 确认旧实现残留的 X-WSSE HTTP 头已移除:认证信息只允许出现在 SOAP 信封内 */
func TestNoLegacyHttpAuthHeader(t *testing.T) {
	var gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-WSSE")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		fmt.Fprint(w, deviceTimeBody())
	}))
	defer srv.Close()

	c := NewClient(Config{DeviceAddr: srv.URL, Username: testUser, Password: testPass})
	if _, err := c.GetDeviceInformation(context.Background()); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("X-WSSE header should be gone, got %q", gotHeader)
	}
	if !strings.Contains(gotBody, "UsernameToken") {
		t.Fatalf("SOAP envelope should embed UsernameToken")
	}
}
