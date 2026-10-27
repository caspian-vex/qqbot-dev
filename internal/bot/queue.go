package bot

import (
	"sync"
	"time"
)

// dedupe 做消息去重。
//
// 为什么需要：协议端在断线重连、或者多个客户端同时订阅时，
// 同一条消息可能被投递多次。不去重的话机器人会重复回复同一句话，
// 在群里非常尴尬。
type dedupe struct {
	mu   sync.Mutex
	seen map[int64]time.Time
	ttl  time.Duration
	last time.Time
}

func newDedupe(ttl time.Duration) *dedupe {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &dedupe{
		seen: make(map[int64]time.Time),
		ttl:  ttl,
		last: time.Now(),
	}
}

// Seen 返回 true 表示这条消息之前处理过。
// 第一次见到会登记并返回 false。
func (d *dedupe) Seen(messageID int64) bool {
	if messageID == 0 {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if _, ok := d.seen[messageID]; ok {
		return true
	}
	d.seen[messageID] = now

	// 顺带做一次惰性清理，避免 map 无限增长。
	// 每 5 分钟最多清理一次，成本可控。
	if now.Sub(d.last) > 5*time.Minute {
		d.last = now
		for id, t := range d.seen {
			if now.Sub(t) > d.ttl {
				delete(d.seen, id)
			}
		}
	}
	return false
}

// Size 返回当前缓存条数（供面板展示）。
func (d *dedupe) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// queue 提供"同一会话串行、跨会话并发"的执行模型。
//
// 为什么同一会话必须串行：上下文是按消息顺序追加的，
// 如果两条消息并发处理，写入历史的顺序会错乱，
// 模型看到的对话就成了"回答在提问之前"。
type queue struct {
	sem      chan struct{}
	mu       sync.Mutex
	depth    map[string]int
	maxDepth int
}

func newQueue(maxConcurrent, maxDepth int) *queue {
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}
	if maxDepth <= 0 {
		maxDepth = 3
	}
	return &queue{
		sem:      make(chan struct{}, maxConcurrent),
		depth:    make(map[string]int),
		maxDepth: maxDepth,
	}
}

// Submit 提交一个任务。返回 false 表示该会话排队已满，任务被丢弃。
//
// 丢弃而不是无限排队，是为了在消息洪峰时保护这台 1 核机器：
// 排队越久，回复越滞后，用户早就聊到别的话题了，
// 迟到的回复比不回复更糟。
func (q *queue) Submit(key string, fn func()) bool {
	q.mu.Lock()
	if q.depth[key] >= q.maxDepth {
		q.mu.Unlock()
		return false
	}
	q.depth[key]++
	q.mu.Unlock()

	go func() {
		defer func() {
			q.mu.Lock()
			q.depth[key]--
			if q.depth[key] <= 0 {
				delete(q.depth, key)
			}
			q.mu.Unlock()
		}()

		q.sem <- struct{}{}
		defer func() { <-q.sem }()
		fn()
	}()
	return true
}

// Depth 返回某会话当前排队数。
func (q *queue) Depth(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth[key]
}

// cooldown 记录每个会话最近一次回复的时间，用于限流。
type cooldown struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newCooldown() *cooldown {
	return &cooldown{last: make(map[string]time.Time)}
}

// Allow 判断是否已过冷却期。pass 为 true 时会顺带记录当前时间。
func (c *cooldown) Allow(key string, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if t, ok := c.last[key]; ok && now.Sub(t) < window {
		return false
	}
	c.last[key] = now
	return true
}

// Peek 只查询不更新。
func (c *cooldown) Peek(key string, window time.Duration) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.last[key]
	if !ok {
		return 0, true
	}
	remain := window - time.Since(t)
	if remain <= 0 {
		return 0, true
	}
	return remain, false
}
