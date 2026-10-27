// Package llm 实现 OpenAI 兼容的 Chat Completions 客户端。
//
// 默认面向 DeepSeek V4.1 Flash，但对任何 OpenAI 兼容端点都能用。
// 关键细节：DeepSeek 的思考模式默认是开启的，机器人场景必须显式关闭，
// 否则每条消息都会先跑一段思维链，首字延迟从 1 秒级涨到 5 秒级，token 也翻几倍。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Config 是单次调用使用的模型配置。
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	Thinking    bool
	Timeout     time.Duration
}

// Usage 是 token 消耗。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Result 是一次调用的完整结果。
type Result struct {
	Content string
	Usage   Usage
	// FirstTokenLatency 用于验证思考模式是否真的关掉了。
	FirstTokenLatency time.Duration
}

// Client 是无状态的模型客户端，可并发使用。
type Client struct {
	http *http.Client
}

// New 创建客户端。
func New() *Client {
	return &Client{
		http: &http.Client{
			// 不设整体超时，靠每次请求的 context 控制，避免长回复被误杀
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []Message     `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Thinking    *thinkingType `json:"thinking,omitempty"`
	StreamOpts  *streamOpts   `json:"stream_options,omitempty"`
}

type thinkingType struct {
	Type string `json:"type"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// buildRequest 组装请求体。
func buildRequest(cfg Config, msgs []Message, stream bool) *chatRequest {
	req := &chatRequest{
		Model:     cfg.Model,
		Messages:  msgs,
		Stream:    stream,
		MaxTokens: cfg.MaxTokens,
		Thinking:  &thinkingType{Type: "disabled"},
	}
	if cfg.Thinking {
		req.Thinking.Type = "enabled"
	} else {
		// 非思考模式下 temperature 才生效；思考模式会忽略它
		t := cfg.Temperature
		req.Temperature = &t
	}
	if stream {
		req.StreamOpts = &streamOpts{IncludeUsage: true}
	}
	return req
}

func (c *Client) endpoint(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	// 允许用户填 https://api.deepseek.com 或带 /v1 的形式
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// Chat 发起一次非流式对话，返回完整文本。
func (c *Client) Chat(ctx context.Context, cfg Config, msgs []Message) (*Result, error) {
	return c.do(ctx, cfg, msgs, false, nil)
}

// ChatStream 发起流式对话，每收到一段增量文本就回调 onDelta。
// 返回时 Result.Content 是拼接后的完整文本。
func (c *Client) ChatStream(ctx context.Context, cfg Config, msgs []Message, onDelta func(string)) (*Result, error) {
	return c.do(ctx, cfg, msgs, true, onDelta)
}

func (c *Client) do(ctx context.Context, cfg Config, msgs []Message, stream bool, onDelta func(string)) (*Result, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("未配置模型 API Key")
	}
	if cfg.Model == "" {
		return nil, errors.New("未配置模型名称")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}

	// 最多尝试 3 次（2 次重试），只对网络错误和 5xx 重试
	var lastErr error
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
		}
		res, err := c.once(ctx, cfg, msgs, stream, onDelta)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isRetryable(err) {
			break
		}
	}
	return nil, lastErr
}

// retryableError 标记可重试的错误。
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

func (c *Client) once(ctx context.Context, cfg Config, msgs []Message, stream bool, onDelta func(string)) (*Result, error) {
	reqCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	body, err := json.Marshal(buildRequest(cfg, msgs, stream))
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint(cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &retryableError{fmt.Errorf("请求模型失败: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(raw))
		var e chatResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		err := fmt.Errorf("模型返回 HTTP %d: %s", resp.StatusCode, msg)
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, &retryableError{err}
		}
		return nil, err
	}

	if !stream {
		var out chatResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, &retryableError{fmt.Errorf("解析响应失败: %w", err)}
		}
		if len(out.Choices) == 0 {
			return nil, errors.New("模型返回空结果")
		}
		res := &Result{
			Content:           out.Choices[0].Message.Content,
			FirstTokenLatency: time.Since(start),
		}
		if out.Usage != nil {
			res.Usage = *out.Usage
		}
		return res, nil
	}

	return readSSE(resp.Body, start, onDelta)
}

// readSSE 解析 Server-Sent Events 流。
func readSSE(r io.Reader, start time.Time, onDelta func(string)) (*Result, error) {
	scanner := bufio.NewScanner(r)
	// 单行可能很长（大块 delta），放宽缓冲上限
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	res := &Result{}
	var sb strings.Builder

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单块解析失败不致命，跳过
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return nil, fmt.Errorf("模型返回错误: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			res.Usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		if res.FirstTokenLatency == 0 {
			res.FirstTokenLatency = time.Since(start)
		}
		sb.WriteString(delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, &retryableError{fmt.Errorf("读取流失败: %w", err)}
	}

	res.Content = sb.String()
	if res.Content == "" {
		return nil, errors.New("模型返回空结果")
	}
	return res, nil
}

// EstimateTokens 粗略估算 token 数。
//
// 不引入 tiktoken 之类的分词器：体积大、多语言支持参差，
// 而这里只需要"宁保守勿超限"的预算判断。
// 中日韩字符约 0.7 token/字，其余约 0.25 token/字符。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var f float64
	for _, r := range s {
		if isCJK(r) {
			f += 0.7
		} else {
			f += 0.25
		}
	}
	n := int(f)
	if f-float64(n) > 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意文字
		(r >= 0x3400 && r <= 0x4DBF) || // 扩展 A
		(r >= 0x3000 && r <= 0x303F) || // CJK 标点
		(r >= 0xFF00 && r <= 0xFFEF) || // 全角字符
		(r >= 0x3040 && r <= 0x30FF) || // 日文假名
		(r >= 0xAC00 && r <= 0xD7AF) // 韩文
}
