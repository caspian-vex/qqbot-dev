package bot

import (
	"math/rand"
	"strings"
	"time"

	"github.com/vex/qqbot/internal/onebot"
	"github.com/vex/qqbot/internal/store"
)

// Decision 是一次触发判断的结果。
type Decision struct {
	// Respond 表示是否需要回复。
	Respond bool
	// Reason 记录判断依据，便于在日志里排查"为什么没回"。
	Reason string
	// Text 是去掉 @ 和唤醒词之后的用户正文。
	Text string
	// Direct 表示这条消息是明确冲着机器人来的（@、唤醒词或私聊）。
	// 被动接话的 Direct 为 false，会被冷却规则限制。
	Direct bool
}

// 直接对话的最小间隔，防止有人连续 @ 刷屏。
const directCooldown = 3 * time.Second

// decide 判断是否回复这条消息。
//
// 判断顺序刻意从"最确定不该回"到"最确定该回"：
// 黑名单 → 私聊 → 群白名单 → @/唤醒词 → 被动概率。
// 这个顺序保证白名单和黑名单永远优先于概率，不会出现
// "在黑名单里但随机命中被回复"这种荒谬情况。
func (e *Engine) decide(ev *onebot.Event, s *store.Settings) Decision {
	if isBlacklisted(ev.UserID, s.Bot.BlacklistUsers) {
		return Decision{Reason: "用户在黑名单"}
	}

	if ev.IsPrivate() {
		if !e.cool.Allow("u:"+itoa(ev.UserID), directCooldown) {
			return Decision{Reason: "私聊冷却中"}
		}
		return Decision{
			Respond: true,
			Reason:  "私聊",
			Text:    ev.PlainText(),
			Direct:  true,
		}
	}

	if !ev.IsGroup() {
		return Decision{Reason: "非消息事件"}
	}

	// 群白名单非空时，只处理白名单内的群
	if len(s.Bot.WhitelistGroups) > 0 && !containsInt64(s.Bot.WhitelistGroups, ev.GroupID) {
		return Decision{Reason: "群不在白名单"}
	}

	self := e.selfID.Load()
	text := ev.PlainText()

	// @ 机器人
	if self != 0 && ev.Mentions(self) {
		if !e.cool.Allow("u:"+itoa(ev.UserID), directCooldown) {
			return Decision{Reason: "用户冷却中"}
		}
		return Decision{
			Respond: true,
			Reason:  "被@",
			Text:    stripWakeWords(text, s.Bot.WakeWords),
			Direct:  true,
		}
	}

	// 唤醒词
	if hit, rest := matchWakeWord(text, s.Bot.WakeWords); hit {
		if !e.cool.Allow("u:"+itoa(ev.UserID), directCooldown) {
			return Decision{Reason: "用户冷却中"}
		}
		return Decision{
			Respond: true,
			Reason:  "命中唤醒词",
			Text:    rest,
			Direct:  true,
		}
	}

	// 被动接话：按概率 + 群冷却
	if s.Bot.PassiveRate <= 0 {
		return Decision{Reason: "被动接话已关闭"}
	}
	if strings.TrimSpace(text) == "" {
		return Decision{Reason: "无可读文本"}
	}
	if rand.Float64() >= s.Bot.PassiveRate {
		return Decision{Reason: "未命中被动概率"}
	}
	window := time.Duration(s.Bot.GroupCooldownSec) * time.Second
	if !e.cool.Allow("g:"+itoa(ev.GroupID), window) {
		return Decision{Reason: "群冷却中"}
	}

	return Decision{
		Respond: true,
		Reason:  "被动接话",
		Text:    text,
		Direct:  false,
	}
}

// matchWakeWord 检查文本是否以某个唤醒词开头（允许前面有空白）。
func matchWakeWord(text string, words []string) (bool, string) {
	t := strings.TrimSpace(text)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if strings.HasPrefix(t, w) {
			return true, strings.TrimSpace(strings.TrimPrefix(t, w))
		}
	}
	return false, text
}

// stripWakeWords 去掉文本中出现的唤醒词。
func stripWakeWords(text string, words []string) string {
	out := strings.TrimSpace(text)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		out = strings.ReplaceAll(out, w, "")
	}
	return strings.TrimSpace(out)
}

func isBlacklisted(userID int64, list []int64) bool {
	return containsInt64(list, userID)
}

func containsInt64(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// itoa 避免为了拼一个字符串引入 strconv 的导入噪音。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
