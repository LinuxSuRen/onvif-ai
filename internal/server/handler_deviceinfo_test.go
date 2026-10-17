package server

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/onvif-ai/internal/ws"
)

// fakeOnvifDevice 要求 WS-Security UsernameToken:有合法令牌回设备信息,
// 否则 401。用于验证 device-info 的凭证确实送达到了 ONVIF 客户端。
func fakeOnvifDevice(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if !strings.Contains(body, "UsernameToken") ||
			!strings.Contains(body, "<wsse:Username>"+user+"</wsse:Username>") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// 校验摘要(Base64(SHA1(nonce+created+pass)))
		nonceB64 := extractMatch(body, `Nonce[^>]*>([^<]+)<`)
		created := extractMatch(body, `Created>([^<]+)<`)
		digest := extractMatch(body, `PasswordDigest[^>]*>([^<]+)<`)
		nonce, err := base64.StdEncoding.DecodeString(nonceB64)
		if err != nil || created == "" || digest == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sum := sha1.Sum(append(append(append([]byte{}, nonce...), []byte(created)...), []byte(pass)...))
		if !hmac.Equal([]byte(base64.StdEncoding.EncodeToString(sum[:])), []byte(digest)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		_, _ = w.Write([]byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` +
			`<tds:GetDeviceInformationResponse><td:Manufacturer>fake</td:Manufacturer>` +
			`<td:Model>cam</td:Model><td:FirmwareVersion>1</td:FirmwareVersion>` +
			`<td:SerialNumber>SN1</td:SerialNumber><td:HardwareId>HW1</td:HardwareId>` +
			`</tds:GetDeviceInformationResponse></s:Body></s:Envelope>`))
	}))
}

func extractMatch(s, pattern string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// POST body 携带的凭证应送达到 ONVIF 客户端(设备校验通过回 200)。
func TestDeviceInfoPostBodyCredentials(t *testing.T) {
	const user, pass = "admin", "s3cret"
	dev := fakeOnvifDevice(t, user, pass)
	defer dev.Close()

	h := NewHandler(ws.NewHub(), nil)
	router := h.RegisterRoutes()

	body, _ := json.Marshal(map[string]string{
		"address": dev.URL, "username": user, "password": pass,
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/camera/device-info",
		strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with body credentials = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Manufacturer string `json:"Manufacturer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Manufacturer != "fake" {
		t.Fatalf("unexpected response: %v %s", err, rec.Body.String())
	}
}

// 凭证放 URL query 不得生效(GET 只认地址)——防密码被访问日志明文记录。
func TestDeviceInfoQueryCredentialsIgnored(t *testing.T) {
	const user, pass = "admin", "s3cret"
	dev := fakeOnvifDevice(t, user, pass)
	defer dev.Close()

	h := NewHandler(ws.NewHub(), nil)
	router := h.RegisterRoutes()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/camera/device-info?address="+dev.URL+"&username="+user+"&password="+pass, nil))
	// 错误路径沿既有约定返回 200 + error JSON,断言看内容:
	// query 凭证被忽略 → 设备应答 401 → 响应为认证错误而非设备信息
	if strings.Contains(rec.Body.String(), `"Manufacturer":"fake"`) {
		t.Fatalf("credentials in query must be ignored, got device info: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "认证") {
		t.Fatalf("expected auth error from device 401, got: %s", rec.Body.String())
	}
}

// POST body 非法/缺地址 → 400。
func TestDeviceInfoPostValidation(t *testing.T) {
	h := NewHandler(ws.NewHub(), nil)
	router := h.RegisterRoutes()

	for _, tc := range []struct {
		name, body string
	}{
		{"empty body", `{}`},
		{"malformed", `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/camera/device-info",
				strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400", tc.name, rec.Code)
			}
		})
	}
}
