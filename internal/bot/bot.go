// Package bot 是消息处理引擎：判断要不要回、组装上下文、调用模型、投递回复。
package bot

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vex/qqbot/internal/ctxmgr"
	"github.com/vex/qqbot/internal/llm"
	"github.com/vex/qqbot/internal/onebot"
	"github.com/vex/qqbot/internal/persona"
	"github.com/vex/qqbot/internal/store"
)

// Stats 是运行时统计，面板总览页用。
type Stats struct {
	Received    atomic.Int64
	Replied     atomic.Int64
	Ignored     atomic.Int64
	Dropped     atomic.Int64
	Errors      atomic.Int64
	LastReplyAt atomic.Int64
}

// Snapshot 是 Stats 的可序列化快照。
type Snapshot struct {
	Received    int64     `json:"received"`
	Replied     int64     `json:"replied"`
	Ignored     int64     `json:"ignored"`
	Dropped     int64     `json:"dropped"`
	Errors      int64     `json:"errors"`
	LastReplyAt time.Time `json:"last_reply_at"`
}

// Engine 是消息处理引擎。
type Engine struct {
	store  *store.Store
	client *onebot.Client
	llm    *llm.Client
	ctx    *ctxmgr.Manager
	log    *slog.Logger

	dedupe *dedupe
	queue  *queue
	cool   *cooldown

	selfID  atomic.Int64
	botName atomic.Value // string

	groupMu    sync.Mutex
	groupNames map[int64]string

	stats Stats
}

// New 创建引擎。
func New(st *store.Store, client *onebot.Client, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		store:      st,
		client:     client,
		llm:        llm.New(),
		ctx:        ctxmgr.New(st),
		log:        log.With("component", "bot"),
		dedupe:     newDedupe(5 * time.Minute),
		queue:      newQueue(3, 3),
		cool:       newCooldown(),
		groupNames: make(map[int64]string),
	}
	e.botName.Store("")
	return e
}

// SetClient 注入协议端客户端。允许先建引擎再建客户端，打破循环依赖。
func (e *Engine) SetClient(c *onebot.Client) { e.client = c }

// SetSelf 设置机器人自身的账号信息（登录后调用）。
func (e *Engine) SetSelf(id int64, name string) {
	e.selfID.Store(id)
	e.botName.Store(name)
}

// SelfID 返回机器人 QQ 号。
func (e *Engine) SelfID() int64 { return e.selfID.Load() }

// BotName 返回机器人昵称。
func (e *Engine) BotName() string {
	v, _ := e.botName.Load().(string)
	return v
}

// Stats 返回统计快照。
func (e *Engine) Stats() Snapshot {
	last := e.stats.LastReplyAt.Load()
	var lastAt time.Time
	if last > 0 {
		lastAt = time.Unix(last, 0)
	}
	return Snapshot{
		Received:    e.stats.Received.Load(),
		Replied:     e.stats.Replied.Load(),
		Ignored:     e.stats.Ignored.Load(),
		Dropped:     e.stats.Dropped.Load(),
		Errors:      e.stats.Errors.Load(),
		LastReplyAt: lastAt,
	}
}

// DedupeSize 返回去重缓存的条目数（面板展示用）。
func (e *Engine) DedupeSize() int { return e.dedupe.Size() }

// HandleEvent 处理一个 OneBot 事件。由 onebot.Client 在独立 goroutine 中调用。
func (e *Engine) HandleEvent(ctx context.Context, ev *onebot.Event) {
	switch {
	case ev.IsMessage():
		e.handleMessage(ctx, ev)
	case ev.PostType == "meta_event":
		if ev.SelfID != 0 {
			e.selfID.Store(ev.SelfID)
		}
	}
}

func (e *Engine) handleMessage(ctx context.Context, ev *onebot.Event) {
	e.stats.Received.Add(1)

	// 忽略自己发的消息，否则会和自己的回复无限对话
	if self := e.selfID.Load(); self != 0 && ev.UserID == self {
		e.stats.Ignored.Add(1)
		return
	}

	if e.dedupe.Seen(ev.MessageID) {
		e.log.Debug("重复消息，已跳过", "message_id", ev.MessageID)
		return
	}

	settings, err := e.store.Settings()
	if err != nil {
		e.log.Error("读取设置失败", "err", err)
		e.stats.Errors.Add(1)
		return
	}

	decision := e.decide(ev, settings)
	if !decision.Respond {
		e.stats.Ignored.Add(1)
		e.log.Debug("不满足触发条件", "reason", decision.Reason,
			"group", ev.GroupID, "user", ev.UserID)
		return
	}

	// 同一会话串行，跨会话并发
	key := sessionKey(ev)
	ok := e.queue.Submit(key, func() {
		e.reply(ctx, ev, settings, decision)
	})
	if !ok {
		e.stats.Dropped.Add(1)
		e.log.Warn("会话排队已满，丢弃本次回复", "session", key)
	}
}

// reply 执行一次完整的"取上下文 → 调模型 → 分段投递"。
func (e *Engine) reply(ctx context.Context, ev *onebot.Event, settings *store.Settings, d Decision) {
	userText := strings.TrimSpace(d.Text)
	if userText == "" {
		// 只 @ 了机器人、或者只发了唤醒词，没带正文。
		// 这种情况给个简短回应即可，没必要为一句"在吗"去调模型。
		if d.Direct {
			if _, err := e.sendText(ctx, ev, []string{"在的，怎么了？"}); err == nil {
				e.stats.Replied.Add(1)
				e.stats.LastReplyAt.Store(time.Now().Unix())
			}
			return
		}
		e.log.Debug("消息没有可读文本，跳过")
		e.stats.Ignored.Add(1)
		return
	}

	p, err := e.store.ActivePersona()
	if err != nil {
		e.log.Error("读取人格失败", "err", err)
		e.stats.Errors.Add(1)
		return
	}

	vars := persona.Vars{
		Time:      time.Now().Format("2006-01-02 15:04"),
		BotName:   e.BotName(),
		Nickname:  ev.Sender.DisplayName(),
		GroupName: e.groupName(ctx, ev.GroupID),
	}
	system := persona.Build(persona.Expand(p.SystemPrompt, vars), vars)

	sessionID := store.SessionID(kindOf(ev), targetOf(ev))
	_ = e.store.TouchSession(sessionID, kindOf(ev), targetOf(ev))

	msgs, err := e.ctx.Build(ctx, sessionID, settings.Context, system, userText)
	if err != nil {
		e.log.Error("组装上下文失败", "err", err)
		e.stats.Errors.Add(1)
		return
	}

	cfg := llm.Config{
		BaseURL:     settings.LLM.BaseURL,
		APIKey:      settings.LLM.APIKey,
		Model:       settings.LLM.Model,
		Temperature: settings.LLM.Temperature,
		MaxTokens:   settings.LLM.MaxTokens,
		Thinking:    settings.LLM.Thinking,
		Timeout:     time.Duration(settings.LLM.TimeoutSec) * time.Second,
	}

	res, err := e.llm.Chat(ctx, cfg, msgs)
	if err != nil {
		e.log.Error("调用模型失败", "err", err)
		e.stats.Errors.Add(1)
		e.sendFallback(ctx, ev)
		return
	}

	reply := StripMarkdown(res.Content)
	if reply == "" {
		e.log.Warn("模型返回空文本")
		e.stats.Errors.Add(1)
		return
	}

	// 先落库再发送：即便发送失败，上下文也是连贯的
	promptTokens := res.Usage.PromptTokens
	if promptTokens == 0 {
		for _, m := range msgs {
			promptTokens += llm.EstimateTokens(m.Content)
		}
	}
	completionTokens := res.Usage.CompletionTokens
	if completionTokens == 0 {
		completionTokens = llm.EstimateTokens(reply)
	}

	_ = e.ctx.Record(sessionID, "user", userText, llm.EstimateTokens(userText))
	_ = e.ctx.Record(sessionID, "assistant", reply, completionTokens)
	_ = e.store.RecordUsage(settings.LLM.Model, promptTokens, completionTokens, isPeak(time.Now()))

	opt := SplitOptions{
		MaxSegments: settings.Bot.MaxSegments,
		MaxRunes:    120,
	}
	segs := Split(reply, opt)
	if len(segs) == 0 {
		return
	}

	if err := e.deliver(ctx, ev, segs); err != nil {
		e.log.Error("发送回复失败", "err", err)
		e.stats.Errors.Add(1)
		return
	}

	e.stats.Replied.Add(1)
	e.stats.LastReplyAt.Store(time.Now().Unix())
	e.log.Info("已回复", "group", ev.GroupID, "user", ev.UserID,
		"segments", len(segs), "first_token", res.FirstTokenLatency.Round(time.Millisecond))
}

// sendFallback 在模型调用失败时给一句兜底回复，避免用户干等。
func (e *Engine) sendFallback(ctx context.Context, ev *onebot.Event) {
	// 兜底也失败就不再重试，避免在故障时放大流量
	_, _ = e.sendText(ctx, ev, []string{"我这会儿有点卡住了，稍后再试试？"})
}

// deliver 分段发送，段之间留随机间隔，模拟真人打字的节奏。
func (e *Engine) deliver(ctx context.Context, ev *onebot.Event, segs []string) error {
	_, err := e.sendText(ctx, ev, segs)
	return err
}

// sendText 按段发送文本，第一段附带引用或 @。
func (e *Engine) sendText(ctx context.Context, ev *onebot.Event, segs []string) (int64, error) {
	if len(segs) == 0 {
		return 0, nil
	}

	var lastID int64
	for i, s := range segs {
		parts := make([]onebot.Segment, 0, 3)

		// 只有第一段带引用/@，后续段保持干净
		if i == 0 {
			if ev.IsGroup() {
				if ev.MessageID != 0 {
					parts = append(parts, onebot.Reply(ev.MessageID))
				} else {
					parts = append(parts, onebot.At(ev.UserID), onebot.Text(" "))
				}
			}
		}
		parts = append(parts, onebot.Text(s))

		var (
			id  int64
			err error
		)
		if ev.IsGroup() {
			id, err = e.client.SendGroupMsg(ctx, ev.GroupID, parts)
		} else {
			id, err = e.client.SendPrivateMsg(ctx, ev.UserID, parts)
		}
		if err != nil {
			return lastID, err
		}
		lastID = id

		// 最后一段不等待
		if i < len(segs)-1 {
			delay := 300 + rand.Intn(500)
			select {
			case <-time.After(time.Duration(delay) * time.Millisecond):
			case <-ctx.Done():
				return lastID, ctx.Err()
			}
		}
	}
	return lastID, nil
}

// groupName 懒加载群名，带进程内缓存。
// 群名只用于给模型一点上下文，取不到也不影响主链路。
func (e *Engine) groupName(ctx context.Context, groupID int64) string {
	if groupID == 0 {
		return ""
	}
	e.groupMu.Lock()
	if name, ok := e.groupNames[groupID]; ok {
		e.groupMu.Unlock()
		return name
	}
	e.groupMu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var info struct {
		GroupName string `json:"group_name"`
	}
	if err := e.client.Call(callCtx, "get_group_info", map[string]any{"group_id": groupID}, &info); err != nil {
		// 缓存空值，避免每条消息都去问一次
		e.groupMu.Lock()
		e.groupNames[groupID] = ""
		e.groupMu.Unlock()
		return ""
	}

	e.groupMu.Lock()
	e.groupNames[groupID] = info.GroupName
	e.groupMu.Unlock()
	return info.GroupName
}

// isPeak 判断当前是否处于 DeepSeek 高峰计费时段。
// 高峰：北京时间周一至周五 9:00-12:00、14:00-18:00。
func isPeak(t time.Time) bool {
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}
	h := t.Hour()
	return (h >= 9 && h < 12) || (h >= 14 && h < 18)
}

func sessionKey(ev *onebot.Event) string {
	return store.SessionID(kindOf(ev), targetOf(ev))
}

func kindOf(ev *onebot.Event) string {
	if ev.IsGroup() {
		return "group"
	}
	return "private"
}

func targetOf(ev *onebot.Event) int64 {
	if ev.IsGroup() {
		return ev.GroupID
	}
	return ev.UserID
}

// ErrNotConnected 表示协议端尚未连接。
var ErrNotConnected = errors.New("协议端未连接")
