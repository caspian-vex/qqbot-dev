// Package mock 提供本地联调用的假协议端与假模型服务。
//
// 存在的理由：开发机不装 Docker，也不该拿真 QQ 号去试错。
// 但"没在真链路上跑过"的代码等于没写——触发判定、去重、冷却、分段、
// 上下文累积、失败兜底、断线重连这些行为，只有端到端跑一遍才能确认。
//
// 这里实现的 mock 严格按 OneBot v11 的报文格式收发，
// 所以被验证的是真实代码路径（onebot.Client + bot.Engine），
// 而不是被 mock 替换过的桩。
package mock

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vex/qqbot/internal/onebot"
)

// ErrNoPeer 表示当前没有任何客户端连着 mock 协议端。
var ErrNoPeer = errors.New("没有已连接的客户端")

// Group 是模拟的群。
type Group struct {
	GroupID     int64  `json:"group_id"`
	GroupName   string `json:"group_name"`
	MemberCount int    `json:"member_count"`
}

// Friend 是模拟的好友。
type Friend struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Remark   string `json:"remark"`
}

// OneBotOptions 是 mock 协议端的启动参数。
type OneBotOptions struct {
	// Addr 是监听地址。留空则监听 127.0.0.1 的随机端口（测试用）。
	Addr     string
	SelfID   int64
	Nickname string
	// Token 非空时校验 Authorization: Bearer <Token>，用于验证鉴权分支。
	Token   string
	Groups  []Group
	Friends []Friend
}

// Sent 是机器人发出去的一条消息，测试用它断言投递结果。
type Sent struct {
	Seq      int64            `json:"seq"`
	Action   string           `json:"action"`
	GroupID  int64            `json:"group_id,omitempty"`
	UserID   int64            `json:"user_id,omitempty"`
	Segments []onebot.Segment `json:"segments"`
	// Text 是所有 text 段拼接后的纯文本。
	Text string `json:"text"`
	// ReplyTo 是 reply 段引用的消息 ID，0 表示没有引用。
	ReplyTo int64 `json:"reply_to,omitempty"`
	// AtUser 是 at 段的 QQ 号，0 表示没有 @。
	AtUser int64     `json:"at_user,omitempty"`
	At     time.Time `json:"at"`
}

// Call 是客户端调用过的一个动作。
type Call struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
	At     time.Time       `json:"at"`
}

type peer struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (p *peer) send(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.write(payload)
}

func (p *peer) write(payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return p.ws.WriteMessage(websocket.TextMessage, payload)
}

// OneBotServer 是一个最小可用的 OneBot v11 WebSocket 服务端。
type OneBotServer struct {
	opts     OneBotOptions
	upgrader websocket.Upgrader
	httpSrv  *http.Server
	addr     string

	mu    sync.Mutex
	peers map[*peer]struct{}
	sent  []Sent
	calls []Call

	seq        atomic.Int64
	msgID      atomic.Int64
	connTotal  atomic.Int64
	rejectSend atomic.Bool
}

// NewOneBot 启动 mock 协议端，监听 127.0.0.1 的随机端口。
func NewOneBot(opts OneBotOptions) (*OneBotServer, error) {
	if opts.SelfID == 0 {
		opts.SelfID = 10001
	}
	if opts.Nickname == "" {
		opts.Nickname = "小助手"
	}

	s := &OneBotServer{
		opts:  opts,
		peers: make(map[*peer]struct{}),
	}
	s.msgID.Store(1000)
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// 本地联调，不校验来源
		CheckOrigin: func(*http.Request) bool { return true },
	}

	ln, err := net.Listen("tcp", listenAddr(opts.Addr))
	if err != nil {
		return nil, err
	}
	s.addr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWS)
	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.httpSrv.Serve(ln) }()

	return s, nil
}

// URL 返回客户端应连接的 WebSocket 地址。
func (s *OneBotServer) URL() string { return "ws://" + s.addr + "/" }

// Addr 返回监听地址。
func (s *OneBotServer) Addr() string { return s.addr }

// Close 关闭所有连接与服务。
func (s *OneBotServer) Close() error {
	s.mu.Lock()
	peers := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.peers = make(map[*peer]struct{})
	s.mu.Unlock()

	for _, p := range peers {
		_ = p.ws.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}

func (s *OneBotServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.opts.Token != "" {
		if strings.TrimSpace(r.Header.Get("Authorization")) != "Bearer "+s.opts.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	p := &peer{ws: ws}

	s.mu.Lock()
	s.peers[p] = struct{}{}
	s.mu.Unlock()
	s.connTotal.Add(1)

	defer func() {
		s.mu.Lock()
		delete(s.peers, p)
		s.mu.Unlock()
		_ = ws.Close()
	}()

	// 真实协议端连上后会先推生命周期事件，这里保持一致，
	// 顺便验证客户端对 meta_event 的处理不会出错。
	_ = p.send(map[string]any{
		"time":            time.Now().Unix(),
		"self_id":         s.opts.SelfID,
		"post_type":       "meta_event",
		"meta_event_type": "lifecycle",
		"sub_type":        "connect",
	})

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		s.handleIncoming(p, data)
	}
}

func (s *OneBotServer) handleIncoming(p *peer, data []byte) {
	var req struct {
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
		Echo   string          `json:"echo"`
	}
	if err := json.Unmarshal(data, &req); err != nil || req.Action == "" {
		// 不是动作请求，忽略
		return
	}

	s.mu.Lock()
	s.calls = append(s.calls, Call{Action: req.Action, Params: req.Params, At: time.Now()})
	s.mu.Unlock()

	s.reply(p, req.Action, req.Params, req.Echo)
}

func okResponse(echo string, data any) map[string]any {
	return map[string]any{"status": "ok", "retcode": 0, "data": data, "echo": echo}
}

func failResponse(echo string, code int, msg string) map[string]any {
	return map[string]any{
		"status":  "failed",
		"retcode": code,
		"data":    nil,
		"message": msg,
		"wording": msg,
		"echo":    echo,
	}
}

func (s *OneBotServer) reply(p *peer, action string, params json.RawMessage, echo string) {
	switch action {
	case "get_login_info":
		_ = p.send(okResponse(echo, map[string]any{
			"user_id":  s.opts.SelfID,
			"nickname": s.opts.Nickname,
		}))

	case "get_status":
		_ = p.send(okResponse(echo, map[string]any{"online": true, "good": true}))

	case "get_version_info":
		_ = p.send(okResponse(echo, map[string]any{
			"app_name":         "mock-onebot",
			"app_version":      "1.0.0",
			"protocol_version": "v11",
		}))

	case "get_group_list":
		groups := s.opts.Groups
		if groups == nil {
			groups = []Group{}
		}
		_ = p.send(okResponse(echo, groups))

	case "get_friend_list":
		friends := s.opts.Friends
		if friends == nil {
			friends = []Friend{}
		}
		_ = p.send(okResponse(echo, friends))

	case "get_group_info":
		var q struct {
			GroupID int64 `json:"group_id"`
		}
		_ = json.Unmarshal(params, &q)
		for _, g := range s.opts.Groups {
			if g.GroupID == q.GroupID {
				_ = p.send(okResponse(echo, g))
				return
			}
		}
		// 未登记的群也给个名字，方便随手造数据联调
		_ = p.send(okResponse(echo, Group{
			GroupID:   q.GroupID,
			GroupName: "群" + strconv.FormatInt(q.GroupID, 10),
		}))

	case "send_group_msg", "send_private_msg":
		s.handleSend(p, action, params, echo)

	default:
		_ = p.send(failResponse(echo, 1404, "mock 未实现的动作: "+action))
	}
}

func (s *OneBotServer) handleSend(p *peer, action string, params json.RawMessage, echo string) {
	if s.rejectSend.Load() {
		_ = p.send(failResponse(echo, 100, "mock 主动拒绝发送"))
		return
	}

	var q struct {
		GroupID int64           `json:"group_id"`
		UserID  int64           `json:"user_id"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(params, &q); err != nil {
		_ = p.send(failResponse(echo, 1400, "参数解析失败: "+err.Error()))
		return
	}

	segs := DecodeSegments(q.Message)
	item := Sent{
		Seq:      s.seq.Add(1),
		Action:   action,
		GroupID:  q.GroupID,
		UserID:   q.UserID,
		Segments: segs,
		Text:     TextOf(segs),
		At:       time.Now(),
	}
	for _, sg := range segs {
		switch sg.Type {
		case "reply":
			item.ReplyTo = ToInt64(sg.Data["id"])
		case "at":
			item.AtUser = ToInt64(sg.Data["qq"])
		}
	}

	s.mu.Lock()
	s.sent = append(s.sent, item)
	s.mu.Unlock()

	_ = p.send(okResponse(echo, map[string]any{"message_id": s.msgID.Add(1)}))
}

// InjectMessage 是构造一条消息事件的参数。
type InjectMessage struct {
	GroupID   int64
	UserID    int64
	Nickname  string
	Card      string
	Text      string
	AtSelf    bool
	AtAll     bool
	Image     bool
	Private   bool
	MessageID int64 // 0 表示自动分配
}

// InjectMessage 构造并推送一条 message 事件。
// 返回这条事件实际使用的 message_id，便于测试重复投递。
func (s *OneBotServer) InjectMessage(m InjectMessage) (int64, error) {
	if m.UserID == 0 {
		m.UserID = 20001
	}
	if m.Nickname == "" {
		m.Nickname = "用户" + strconv.FormatInt(m.UserID, 10)
	}

	segs := make([]onebot.Segment, 0, 4)
	if m.AtSelf {
		segs = append(segs, onebot.Segment{
			Type: "at",
			Data: map[string]any{"qq": strconv.FormatInt(s.opts.SelfID, 10)},
		})
	}
	if m.AtAll {
		segs = append(segs, onebot.Segment{
			Type: "at",
			Data: map[string]any{"qq": "all"},
		})
	}
	if m.Image {
		segs = append(segs, onebot.Segment{
			Type: "image",
			Data: map[string]any{"file": "mock.jpg", "url": "https://example.invalid/mock.jpg"},
		})
	}
	if m.Text != "" {
		segs = append(segs, onebot.Segment{
			Type: "text",
			Data: map[string]any{"text": m.Text},
		})
	}

	id := m.MessageID
	if id == 0 {
		id = s.msgID.Add(1)
	}

	messageType := "group"
	if m.Private {
		messageType = "private"
	}

	ev := map[string]any{
		"time":         time.Now().Unix(),
		"self_id":      s.opts.SelfID,
		"post_type":    "message",
		"message_type": messageType,
		"sub_type":     "normal",
		"message_id":   id,
		"user_id":      m.UserID,
		"message":      segs,
		"raw_message":  RawCQ(segs),
		"font":         0,
		"sender": map[string]any{
			"user_id":  m.UserID,
			"nickname": m.Nickname,
			"card":     m.Card,
			"role":     "member",
		},
	}
	if !m.Private {
		ev["group_id"] = m.GroupID
	}

	if err := s.Broadcast(ev); err != nil {
		return id, err
	}
	return id, nil
}

// Broadcast 把任意事件推给所有已连接客户端。
func (s *OneBotServer) Broadcast(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}

	s.mu.Lock()
	peers := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()

	if len(peers) == 0 {
		return ErrNoPeer
	}

	var firstErr error
	for _, p := range peers {
		if err := p.write(payload); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Sent 返回机器人发出的全部消息快照。
func (s *OneBotServer) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent(nil), s.sent...)
}

// Calls 返回客户端调用过的动作快照。
func (s *OneBotServer) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Connected 返回当前连接数。
func (s *OneBotServer) Connected() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// ConnTotal 返回累计接受过的连接数（含重连）。
func (s *OneBotServer) ConnTotal() int64 { return s.connTotal.Load() }

// Reset 清空已发送记录与调用记录，便于一个测试里做多段断言。
func (s *OneBotServer) Reset() {
	s.mu.Lock()
	s.sent = nil
	s.calls = nil
	s.mu.Unlock()
}

// SetRejectSend 让后续 send_* 动作返回失败，用于验证发送失败分支。
func (s *OneBotServer) SetRejectSend(v bool) { s.rejectSend.Store(v) }

// DropPeers 强行断开所有连接，用于验证客户端的重连与退避。
func (s *OneBotServer) DropPeers() {
	s.mu.Lock()
	peers := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()

	for _, p := range peers {
		_ = p.ws.Close()
	}
}

// WaitSent 阻塞等待已发送消息数达到 n 条。
func (s *OneBotServer) WaitSent(ctx context.Context, n int) ([]Sent, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := s.Sent(); len(got) >= n {
			return got, nil
		}
		select {
		case <-ctx.Done():
			return s.Sent(), ctx.Err()
		case <-ticker.C:
		}
	}
}

// WaitConnected 阻塞等待至少 n 个客户端连上来。
func (s *OneBotServer) WaitConnected(ctx context.Context, n int) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.Connected() >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// DecodeSegments 把 message 字段解成消息段。
// 兼容 array 与 CQ 码字符串两种格式，和真实协议端行为一致。
func DecodeSegments(raw json.RawMessage) []onebot.Segment {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return nil
	}
	if t[0] == '[' {
		var segs []onebot.Segment
		if err := json.Unmarshal(raw, &segs); err != nil {
			return nil
		}
		return segs
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return nil
	}
	return onebot.ParseCQ(str)
}

// TextOf 拼接所有 text 段的文本。
func TextOf(segs []onebot.Segment) string {
	var b strings.Builder
	for _, sg := range segs {
		if sg.Type != "text" {
			continue
		}
		if t, ok := sg.Data["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

// RawCQ 把消息段还原成 CQ 码字符串，用于填充 raw_message。
func RawCQ(segs []onebot.Segment) string {
	var b strings.Builder
	for _, sg := range segs {
		switch sg.Type {
		case "text":
			if t, ok := sg.Data["text"].(string); ok {
				b.WriteString(t)
			}
		case "at":
			b.WriteString("[CQ:at,qq=" + formatAny(sg.Data["qq"]) + "]")
		case "image":
			b.WriteString("[CQ:image,file=" + formatAny(sg.Data["file"]) + "]")
		case "reply":
			b.WriteString("[CQ:reply,id=" + formatAny(sg.Data["id"]) + "]")
		default:
			b.WriteString("[CQ:" + sg.Type + "]")
		}
	}
	return b.String()
}

// ToInt64 把 OneBot 里既可能是数字、也可能是字符串的字段转成 int64。
func ToInt64(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}

func formatAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return ""
	}
}

// listenAddr 统一"留空则随机端口"的行为，避免两处重复。
func listenAddr(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return "127.0.0.1:0"
	}
	return addr
}
