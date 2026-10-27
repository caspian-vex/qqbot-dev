package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	dialTimeout    = 10 * time.Second
	writeTimeout   = 10 * time.Second
	readTimeout    = 60 * time.Second
	pingInterval   = 25 * time.Second
	callTimeout    = 20 * time.Second
	minBackoff     = 1 * time.Second
	maxBackoff     = 30 * time.Second
	stableConnTime = 30 * time.Second
)

// Status 是对外暴露的连接状态，面板用它渲染总览页。
type Status struct {
	Connected   bool      `json:"connected"`
	SelfID      int64     `json:"self_id"`
	Nickname    string    `json:"nickname"`
	ConnectedAt time.Time `json:"connected_at"`
	LastEventAt time.Time `json:"last_event_at"`
	Reconnects  int       `json:"reconnects"`
	LastError   string    `json:"last_error"`
}

// Response 是 OneBot 动作调用的响应。
type Response struct {
	Status  string          `json:"status"`
	RetCode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	Echo    string          `json:"echo"`
}

type actionRequest struct {
	Action string `json:"action"`
	Params any    `json:"params"`
	Echo   string `json:"echo,omitempty"`
}

// Client 是 OneBot v11 的 WebSocket 客户端。
// 同一连接既收事件也发动作，用 echo 字段关联请求与响应。
type Client struct {
	url   string
	token string
	log   *slog.Logger

	handler func(context.Context, *Event)

	writeMu sync.Mutex
	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan *Response
	echoSeq atomic.Uint64

	statusMu sync.RWMutex
	status   Status
}

// New 创建客户端。handler 可为 nil，之后用 SetHandler 注入。
func New(url, token string, log *slog.Logger, handler func(context.Context, *Event)) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		url:     url,
		token:   token,
		log:     log.With("component", "onebot"),
		handler: handler,
		pending: make(map[string]chan *Response),
	}
}

// SetHandler 设置事件处理函数。允许构造后再注入，避免与上层循环依赖。
func (c *Client) SetHandler(h func(context.Context, *Event)) { c.handler = h }

// Status 返回当前连接状态的快照。
func (c *Client) Status() Status {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

func (c *Client) setStatus(fn func(*Status)) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	fn(&c.status)
}

// Run 持续维持连接，直到 ctx 取消。
// 断线后按指数退避重连；连接稳定超过 30 秒才重置退避，避免"连上即断"时高频重试。
func (c *Client) Run(ctx context.Context) {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		startedAt := time.Now()
		err := c.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.Warn("连接断开", "err", err, "retry_in", backoff)
			c.setStatus(func(s *Status) {
				s.Connected = false
				s.LastError = err.Error()
				s.Reconnects++
			})
		}
		if time.Since(startedAt) > stableConnTime {
			backoff = minBackoff
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (c *Client) serve(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: dialTimeout,
		Proxy:            http.ProxyFromEnvironment,
	}
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}

	conn, resp, err := dialer.DialContext(ctx, c.url, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("握手失败 HTTP %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("拨号失败: %w", err)
	}
	defer conn.Close()

	c.mu.Lock()
	c.conn = conn
	c.pending = make(map[string]chan *Response)
	c.mu.Unlock()

	c.setStatus(func(s *Status) {
		s.Connected = true
		s.ConnectedAt = time.Now()
		s.LastError = ""
	})
	c.log.Info("已连接协议端", "url", c.url)

	// 连接关闭时唤醒所有等待中的调用，避免调用方卡到超时
	defer c.failPending()

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go c.pingLoop(pingCtx, conn)

	for {
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return err
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return fmt.Errorf("协议端主动关闭连接")
			}
			return err
		}
		c.dispatch(ctx, data)
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.write(conn, websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) write(conn *websocket.Conn, msgType int, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(msgType, payload)
}

// dispatch 区分「动作响应」与「事件上报」。
// 响应带 echo 且能匹配到等待中的调用；其余一律当事件。
func (c *Client) dispatch(ctx context.Context, data []byte) {
	var probe struct {
		Echo     string `json:"echo"`
		PostType string `json:"post_type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		c.log.Debug("收到无法解析的报文", "err", err)
		return
	}

	if probe.Echo != "" && probe.PostType == "" {
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			c.log.Debug("解析响应失败", "err", err)
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.Echo]
		if ok {
			delete(c.pending, resp.Echo)
		}
		c.mu.Unlock()
		if ok {
			ch <- &resp
		}
		return
	}

	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		c.log.Debug("解析事件失败", "err", err)
		return
	}
	_ = json.Unmarshal(data, &ev.Raw)

	c.setStatus(func(s *Status) {
		s.LastEventAt = time.Now()
		if ev.SelfID != 0 {
			s.SelfID = ev.SelfID
		}
	})

	if h := c.handler; h != nil {
		go h(ctx, &ev)
	}
}

// Call 调用一个 OneBot 动作，并把 data 反序列化到 out（out 可为 nil）。
func (c *Client) Call(ctx context.Context, action string, params any, out any) error {
	if params == nil {
		params = map[string]any{}
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("尚未连接到协议端")
	}

	echo := fmt.Sprintf("q%d", c.echoSeq.Add(1))
	ch := make(chan *Response, 1)

	c.mu.Lock()
	c.pending[echo] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, echo)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(actionRequest{Action: action, Params: params, Echo: echo})
	if err != nil {
		return fmt.Errorf("序列化动作请求失败: %w", err)
	}
	if err := c.write(conn, websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("发送动作请求失败: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return fmt.Errorf("调用 %s 超时: %w", action, waitCtx.Err())
	case resp := <-ch:
		if resp == nil {
			return fmt.Errorf("调用 %s 时连接中断", action)
		}
		if resp.RetCode != 0 {
			msg := resp.Wording
			if msg == "" {
				msg = resp.Message
			}
			return fmt.Errorf("调用 %s 失败 retcode=%d: %s", action, resp.RetCode, msg)
		}
		if out != nil && len(resp.Data) > 0 {
			if err := json.Unmarshal(resp.Data, out); err != nil {
				return fmt.Errorf("解析 %s 返回值失败: %w", action, err)
			}
		}
		return nil
	}
}

// failPending 在连接断开时唤醒所有等待者。
func (c *Client) failPending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan *Response)
	c.conn = nil
	c.mu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- nil:
		default:
		}
	}
}
