package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vex/qqbot/internal/llm"
)

// LLMRequest 是 mock 模型收到的请求，测试用它断言请求体是否正确。
//
// 重点看两个字段：
//   - Thinking：DeepSeek V4.1 默认开思考模式，机器人必须显式关掉，
//     否则每条消息多几秒延迟。这里断言的就是"真的关掉了"。
//   - Temperature：思考模式下该字段会被忽略，所以只在非思考模式发送。
type LLMRequest struct {
	Model       string        `json:"model"`
	Messages    []llm.Message `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Thinking    *struct {
		Type string `json:"type"`
	} `json:"thinking"`
}

// LastUserText 返回最后一条 user 消息的文本。
func (r LLMRequest) LastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Content
		}
	}
	return ""
}

// SystemText 返回 system 消息的文本。
func (r LLMRequest) SystemText() string {
	for _, m := range r.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// ThinkingType 返回 thinking.type，未设置时返回空串。
func (r LLMRequest) ThinkingType() string {
	if r.Thinking == nil {
		return ""
	}
	return r.Thinking.Type
}

// LLMOptions 是 mock 模型的启动参数。
type LLMOptions struct {
	// Addr 是监听地址。留空则监听 127.0.0.1 的随机端口（测试用）。
	Addr string
	// APIKey 非空时校验 Authorization，用于验证鉴权分支。
	APIKey string
	// Reply 是固定回复。为空时按"收到：<用户最后一句>"生成。
	Reply string
}

// LLMServer 是一个 OpenAI 兼容的假模型服务。
type LLMServer struct {
	opts    LLMOptions
	httpSrv *http.Server
	addr    string

	mu       sync.Mutex
	reply    string
	delay    time.Duration
	failNext int
	requests []LLMRequest
	lastFail string
}

// NewLLM 启动 mock 模型服务，监听 127.0.0.1 的随机端口。
func NewLLM(opts LLMOptions) (*LLMServer, error) {
	s := &LLMServer{opts: opts, reply: opts.Reply}

	ln, err := net.Listen("tcp", listenAddr(opts.Addr))
	if err != nil {
		return nil, err
	}
	s.addr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.httpSrv.Serve(ln) }()

	return s, nil
}

// URL 返回可填进 BaseURL 的地址（不带 /v1，客户端会自己补）。
func (s *LLMServer) URL() string { return "http://" + s.addr }

// Addr 返回监听地址。
func (s *LLMServer) Addr() string { return s.addr }

// Close 关闭服务。
func (s *LLMServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}

// SetReply 设置固定回复。
func (s *LLMServer) SetReply(text string) {
	s.mu.Lock()
	s.reply = text
	s.mu.Unlock()
}

// SetDelay 设置每次响应前的延迟，用于验证超时与并发行为。
func (s *LLMServer) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// FailNext 让接下来 n 次调用返回 HTTP 500，用于验证重试与兜底。
func (s *LLMServer) FailNext(n int) {
	s.mu.Lock()
	s.failNext = n
	s.mu.Unlock()
}

// Requests 返回收到的请求快照。
func (s *LLMServer) Requests() []LLMRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LLMRequest(nil), s.requests...)
}

// Calls 返回累计调用次数。
func (s *LLMServer) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Reset 清空请求记录。
func (s *LLMServer) Reset() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
}

// WaitCalls 阻塞等待调用次数达到 n。
func (s *LLMServer) WaitCalls(ctx context.Context, n int) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.Calls() >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *LLMServer) handle(w http.ResponseWriter, r *http.Request) {
	// /v1/models 给健康检查用，不参与断言
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"id": "deepseek-flash", "object": "model"}},
		})
		return
	}

	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		writeJSON(w, http.StatusNotFound, errorBody("mock 只实现了 /v1/chat/completions"))
		return
	}

	if s.opts.APIKey != "" {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got != s.opts.APIKey {
			writeJSON(w, http.StatusUnauthorized, errorBody("无效的 API Key"))
			return
		}
	}

	var req LLMRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("请求体解析失败: "+err.Error()))
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	fail := s.failNext > 0
	if fail {
		s.failNext--
		s.lastFail = req.LastUserText()
	}
	reply := s.reply
	delay := s.delay
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	if fail {
		writeJSON(w, http.StatusInternalServerError, errorBody("mock 上游故障"))
		return
	}

	text := reply
	if text == "" {
		text = defaultReply(req)
	}
	usage := usageOf(req, text)

	if req.Stream {
		s.writeStream(w, text, usage)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": usage,
	})
}

func (s *LLMServer) writeStream(w http.ResponseWriter, text string, usage map[string]any) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// 按 8 个字符一块推送，模拟真实流式返回
	for _, chunk := range chunkRunes(text, 8) {
		payload, _ := json.Marshal(map[string]any{
			"id":     "chatcmpl-mock",
			"object": "chat.completion.chunk",
			"model":  "deepseek-flash",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": chunk},
				"finish_reason": nil,
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 末尾单独一帧带 usage、choices 为空，和真实 OpenAI 行为一致
	final, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"choices": []any{},
		"usage":   usage,
	})
	fmt.Fprintf(w, "data: %s\n\n", final)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func defaultReply(req LLMRequest) string {
	last := strings.TrimSpace(req.LastUserText())
	if last == "" {
		return "嗯，我在。"
	}
	r := []rune(last)
	if len(r) > 20 {
		return "收到：" + string(r[:20]) + "…"
	}
	return "收到：" + last
}

func usageOf(req LLMRequest, text string) map[string]any {
	prompt := 0
	for _, m := range req.Messages {
		prompt += llm.EstimateTokens(m.Content)
	}
	completion := llm.EstimateTokens(text)
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

func chunkRunes(s string, size int) []string {
	r := []rune(s)
	if size <= 0 {
		size = 8
	}
	var out []string
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func errorBody(msg string) map[string]any {
	return map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error"},
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
