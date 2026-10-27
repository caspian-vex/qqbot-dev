// Package onebot 实现 OneBot v11 的 WebSocket 客户端。
//
// 只依赖协议本身，不感知对端是 NapCat 还是别的协议端，
// 这样将来换协议端时上层框架零改动。
package onebot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Event 是 OneBot v11 上报事件的超集。
// 只声明框架真正会用到的字段，其余保留在 Raw 里备用。
type Event struct {
	Time     int64  `json:"time"`
	SelfID   int64  `json:"self_id"`
	PostType string `json:"post_type"`

	// message 类事件
	MessageType string          `json:"message_type"`
	SubType     string          `json:"sub_type"`
	MessageID   int64           `json:"message_id"`
	UserID      int64           `json:"user_id"`
	GroupID     int64           `json:"group_id"`
	RawMessage  string          `json:"raw_message"`
	Message     json.RawMessage `json:"message"`
	Font        int             `json:"font"`
	Sender      Sender          `json:"sender"`

	// meta_event 类事件
	MetaEventType string         `json:"meta_event_type"`
	Interval      int64          `json:"interval"`
	Status        map[string]any `json:"status"`

	// notice 类事件
	NoticeType string `json:"notice_type"`
	OperatorID int64  `json:"operator_id"`

	Raw map[string]any `json:"-"`
}

// Sender 是消息发送者信息。
type Sender struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Card     string `json:"card"`
	Role     string `json:"role"`
}

// DisplayName 返回群名片优先、昵称兜底的展示名。
func (s Sender) DisplayName() string {
	if s.Card != "" {
		return s.Card
	}
	if s.Nickname != "" {
		return s.Nickname
	}
	return strconv.FormatInt(s.UserID, 10)
}

// Segment 是一条消息段（OneBot v11 的 array 格式）。
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// IsGroup 判断是否为群消息事件。
func (e *Event) IsGroup() bool { return e.PostType == "message" && e.MessageType == "group" }

// IsPrivate 判断是否为私聊消息事件。
func (e *Event) IsPrivate() bool { return e.PostType == "message" && e.MessageType == "private" }

// IsMessage 判断是否为消息事件（群或私聊）。
func (e *Event) IsMessage() bool { return e.PostType == "message" }

// IsHeartbeat 判断是否为心跳事件。
func (e *Event) IsHeartbeat() bool {
	return e.PostType == "meta_event" && e.MetaEventType == "heartbeat"
}

// Segments 把消息解析成消息段切片。
// 协议端可能配置成 array 或 string（CQ 码）格式，这里两种都兼容。
func (e *Event) Segments() ([]Segment, error) {
	if len(e.Message) == 0 {
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(e.Message))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var segs []Segment
		if err := json.Unmarshal(e.Message, &segs); err != nil {
			return nil, fmt.Errorf("解析消息段数组失败: %w", err)
		}
		return segs, nil
	}
	var s string
	if err := json.Unmarshal(e.Message, &s); err != nil {
		return nil, fmt.Errorf("解析消息字符串失败: %w", err)
	}
	return ParseCQ(s), nil
}

// PlainText 抽取消息中的纯文本部分，@ 与图片等非文本段会被忽略。
func (e *Event) PlainText() string {
	// 少数协议端只给 raw_message 不给 message，这种情况下才回退。
	if len(e.Message) == 0 {
		return strings.TrimSpace(e.RawMessage)
	}
	segs, err := e.Segments()
	if err != nil {
		// 解析失败时退回到 raw_message，至少不让消息完全丢失
		return strings.TrimSpace(e.RawMessage)
	}
	var b strings.Builder
	for _, s := range segs {
		if s.Type == "text" {
			if t, ok := s.Data["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	// 解析成功但没有任何文本段（只有 @、图片、表情），就是"没有可读正文"。
	//
	// 这里绝不能回退到 raw_message：NapCat 的 raw_message 是 CQ 码字符串，
	// 一条纯 @ 的消息回退后会变成 "[CQ:at,qq=123456]"，
	// 于是模型会收到一句"用户说：[CQ:at,qq=123456]"并认真回答它。
	return strings.TrimSpace(b.String())
}

// AtTargets 返回消息中 @ 到的 QQ 号列表。
func (e *Event) AtTargets() []int64 {
	segs, err := e.Segments()
	if err != nil {
		return nil
	}
	var out []int64
	for _, s := range segs {
		if s.Type != "at" {
			continue
		}
		switch v := s.Data["qq"].(type) {
		case string:
			if v == "all" {
				continue
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				out = append(out, n)
			}
		case float64:
			out = append(out, int64(v))
		}
	}
	return out
}

// Mentions 判断消息是否 @ 了指定 QQ 号。
func (e *Event) Mentions(qq int64) bool {
	for _, t := range e.AtTargets() {
		if t == qq {
			return true
		}
	}
	return false
}

// HasImage 判断消息里是否包含图片。
func (e *Event) HasImage() bool {
	segs, err := e.Segments()
	if err != nil {
		return false
	}
	for _, s := range segs {
		if s.Type == "image" {
			return true
		}
	}
	return false
}

// ParseCQ 把 CQ 码字符串解析成消息段。
// 只处理框架需要的子集：text / at / image / face / reply。
func ParseCQ(s string) []Segment {
	var segs []Segment
	for len(s) > 0 {
		idx := strings.Index(s, "[CQ:")
		if idx < 0 {
			if s != "" {
				segs = append(segs, Segment{Type: "text", Data: map[string]any{"text": unescapeCQ(s)}})
			}
			break
		}
		if idx > 0 {
			segs = append(segs, Segment{Type: "text", Data: map[string]any{"text": unescapeCQ(s[:idx])}})
		}
		end := strings.Index(s[idx:], "]")
		if end < 0 {
			segs = append(segs, Segment{Type: "text", Data: map[string]any{"text": s[idx:]}})
			break
		}
		body := s[idx+4 : idx+end] // 去掉 "[CQ:" 前缀
		segs = append(segs, parseCQBody(body))
		s = s[idx+end+1:]
	}
	return segs
}

func parseCQBody(body string) Segment {
	typ, rest, _ := strings.Cut(body, ",")
	data := map[string]any{}
	for _, kv := range strings.Split(rest, ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		data[k] = unescapeCQ(v)
	}
	return Segment{Type: typ, Data: data}
}

var cqEscaper = strings.NewReplacer(
	"&#91;", "[",
	"&#93;", "]",
	"&#44;", ",",
	"&amp;", "&",
)

func unescapeCQ(s string) string { return cqEscaper.Replace(s) }
