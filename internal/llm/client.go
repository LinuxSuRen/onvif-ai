package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
)

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type ImageContent struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Stream     bool      `json:"stream"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type Client struct {
	config Config
	http   *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		config: cfg,
		http:   &http.Client{},
	}
}

func (c *Client) UpdateConfig(baseURL, apiKey, model string) {
	if baseURL != "" {
		c.config.BaseURL = baseURL
	}
	if apiKey != "" {
		c.config.APIKey = apiKey
	}
	if model != "" {
		c.config.Model = model
	}
}

func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	url := c.buildURL("/chat/completions")

	reqBody := chatRequest{
		Model:    c.config.Model,
		Messages: messages,
		Stream:   false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(errBody[:min(len(errBody), 500)]))
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return result.Choices[0].Message.Content, nil
}

func (c *Client) ChatStream(ctx context.Context, messages []Message, callback func(chunk string) error) (string, error) {
	url := c.buildURL("/chat/completions")

	reqBody := chatRequest{
		Model:    c.config.Model,
		Messages: messages,
		Stream:   true,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(errBody[:min(len(errBody), 500)]))
	}

	var fullText strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk chatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 {
			content := chunk.Choices[0].Delta.Content
			if content != "" {
				fullText.WriteString(content)
				if callback != nil {
					if err := callback(content); err != nil {
						return fullText.String(), err
					}
				}
			}
		}
	}

	return fullText.String(), scanner.Err()
}

func (c *Client) ChatWithTools(ctx context.Context, messages []Message, tools []Tool) (string, []ToolCall, error) {
	url := c.buildURL("/chat/completions")

	reqBody := chatRequest{
		Model:    c.config.Model,
		Messages: messages,
		Stream:   false,
		Tools:    tools,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("marshal request: %w", err)
	}

	log.Printf("[LLM] ChatWithTools: model=%s, tools=%d", c.config.Model, len(tools))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(errBody[:min(len(errBody), 500)]))
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", nil, fmt.Errorf("no choices in LLM response")
	}

	choice := result.Choices[0]
	return choice.Message.Content, choice.Message.ToolCalls, nil
}

func (c *Client) Transcribe(ctx context.Context, audioData []byte, format string) (string, error) {
	url := c.buildURL("/audio/transcriptions")

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("language", "zh")

	part, err := writer.CreateFormFile("file", "audio."+format)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	part.Write(audioData)
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("whisper returned %d: %s", resp.StatusCode, string(errBody[:min(len(errBody), 300)]))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode whisper response: %w", err)
	}

	return strings.TrimSpace(result.Text), nil
}

func (c *Client) buildURL(path string) string {
	base := strings.TrimRight(c.config.BaseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1" + path
}
