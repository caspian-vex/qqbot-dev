package panel

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// LogLine 是一条面板可见的日志。
type LogLine struct {
	Time   time.Time         `json:"time"`
	Level  string            `json:"level"`
	Msg    string            `json:"msg"`
	Attrs  map[string]string `json:"attrs,omitempty"`
	Source string            `json:"source,omitempty"`
}

// LogHub 维护一个环形缓冲区，并向所有 SSE 订阅者广播。
//
// 为什么不直接读 docker logs：面板跑在容器内，读不到宿主机日志；
// 而通过 socket 读又需要挂 docker.sock（安全上不可接受）。
// 所以让进程内的日志直接进 hub，面板实时展示。
type LogHub struct {
	mu   sync.RWMutex
	buf  []LogLine
	max  int
	subs map[chan LogLine]struct{}
}

func NewLogHub(max int) *LogHub {
	if max <= 0 {
		max = 500
	}
	return &LogHub{
		buf:  make([]LogLine, 0, max),
		max:  max,
		subs: make(map[chan LogLine]struct{}),
	}
}

// Push 写入一条日志并广播。
func (h *LogHub) Push(l LogLine) {
	h.mu.Lock()
	if len(h.buf) >= h.max {
		copy(h.buf, h.buf[1:])
		h.buf[len(h.buf)-1] = l
	} else {
		h.buf = append(h.buf, l)
	}
	subs := make([]chan LogLine, 0, len(h.subs))
	for ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- l:
		default:
			// 订阅者读得慢就丢，绝不能阻塞日志写入
		}
	}
}

// Recent 返回最近的 n 条日志（时间正序）。
func (h *LogHub) Recent(n int) []LogLine {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if n <= 0 || n > len(h.buf) {
		n = len(h.buf)
	}
	out := make([]LogLine, n)
	copy(out, h.buf[len(h.buf)-n:])
	return out
}

// Subscribe 订阅新日志。
func (h *LogHub) Subscribe() (<-chan LogLine, func()) {
	ch := make(chan LogLine, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// hubHandler 是一个把日志同时写入 hub 的 slog.Handler。
type hubHandler struct {
	next slog.Handler
	hub  *LogHub
	attr []slog.Attr
}

// NewHubHandler 包装一个已有的 slog.Handler，额外把日志推给面板。
func NewHubHandler(next slog.Handler, hub *LogHub) slog.Handler {
	return &hubHandler{next: next, hub: hub}
}

func (h *hubHandler) Enabled(ctx context.Context, l slog.Level) bool { return true }

func (h *hubHandler) Handle(ctx context.Context, r slog.Record) error {
	line := LogLine{
		Time:  r.Time,
		Level: r.Level.String(),
		Msg:   r.Message,
	}
	if len(h.attr) > 0 || r.NumAttrs() > 0 {
		line.Attrs = make(map[string]string, len(h.attr)+r.NumAttrs())
		for _, a := range h.attr {
			line.Attrs[a.Key] = a.Value.String()
		}
		r.Attrs(func(a slog.Attr) bool {
			line.Attrs[a.Key] = a.Value.String()
			return true
		})
	}
	h.hub.Push(line)
	return h.next.Handle(ctx, r)
}

func (h *hubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &hubHandler{next: h.next.WithAttrs(attrs), hub: h.hub, attr: append([]slog.Attr{}, h.attr...)}
	next.attr = append(next.attr, attrs...)
	return next
}

func (h *hubHandler) WithGroup(name string) slog.Handler {
	return &hubHandler{next: h.next.WithGroup(name), hub: h.hub, attr: h.attr}
}
