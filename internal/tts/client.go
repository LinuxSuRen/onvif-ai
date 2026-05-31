package tts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Config struct {
	Provider string // "edge-tts" (default), or custom HTTP endpoint
	Voice    string // e.g., "zh-CN-XiaoxiaoNeural"
	Endpoint string // custom TTS HTTP endpoint
}

type Client struct {
	config Config
	http   *http.Client
}

func NewClient(cfg Config) *Client {
	if cfg.Provider == "" {
		cfg.Provider = "edge-tts"
	}
	if cfg.Voice == "" {
		cfg.Voice = "zh-CN-XiaoxiaoNeural"
	}
	return &Client{
		config: cfg,
		http:   &http.Client{},
	}
}

func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, error) {
	switch c.config.Provider {
	case "edge-tts":
		return c.edgeTTS(ctx, text)
	case "http":
		return c.httpTTS(ctx, text)
	default:
		return nil, fmt.Errorf("unknown TTS provider: %s", c.config.Provider)
	}
}

func (c *Client) edgeTTS(ctx context.Context, text string) ([]byte, error) {
	ssml := fmt.Sprintf(`<speak version="1.0" xmlns="http://www.w3.org/2001/10/synthesis" xml:lang="zh-CN">
	<voice name="%s">%s</voice>
</speak>`, c.config.Voice, text)

	apiURL := fmt.Sprintf(
		"https://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=6A5AA1D4EAFF4E9FB37E23D68491D6F4&ConnectionId=onvif-ai",
	)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(ssml))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold")
	req.Header.Set("X-Microsoft-OutputFormat", "raw-16khz-16bit-mono-pcm")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("edge-tts request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("edge-tts returned %d: %s", resp.StatusCode, string(body[:min(len(body), 300)]))
	}

	return io.ReadAll(resp.Body)
}

func (c *Client) httpTTS(ctx context.Context, text string) ([]byte, error) {
	endpoint := c.config.Endpoint

	params := url.Values{}
	params.Set("text", text)
	params.Set("voice", c.config.Voice)
	params.Set("format", "pcm")

	fullURL := endpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS returned %d: %s", resp.StatusCode, string(body[:min(len(body), 300)]))
	}

	return io.ReadAll(resp.Body)
}
