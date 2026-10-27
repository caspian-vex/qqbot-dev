package onebot

import (
	"encoding/json"
	"testing"
)

func TestParseCQ(t *testing.T) {
	segs := ParseCQ("你好[CQ:at,qq=123456]看看这个[CQ:image,file=abc.jpg]")
	if len(segs) != 4 {
		t.Fatalf("期望 4 段，实际 %d 段: %+v", len(segs), segs)
	}
	if segs[0].Type != "text" || segs[0].Data["text"] != "你好" {
		t.Errorf("第 0 段错误: %+v", segs[0])
	}
	if segs[1].Type != "at" || segs[1].Data["qq"] != "123456" {
		t.Errorf("第 1 段错误: %+v", segs[1])
	}
	if segs[2].Type != "text" || segs[2].Data["text"] != "看看这个" {
		t.Errorf("第 2 段错误: %+v", segs[2])
	}
	if segs[3].Type != "image" || segs[3].Data["file"] != "abc.jpg" {
		t.Errorf("第 3 段错误: %+v", segs[3])
	}
}

func TestParseCQEscapes(t *testing.T) {
	segs := ParseCQ("a&#91;b&#93;&#44;c")
	if len(segs) != 1 {
		t.Fatalf("期望 1 段，实际 %d 段", len(segs))
	}
	if got := segs[0].Data["text"]; got != "a[b],c" {
		t.Errorf("转义还原错误: %q", got)
	}
}

func TestEventPlainTextArray(t *testing.T) {
	raw := `[{"type":"at","data":{"qq":"999"}},{"type":"text","data":{"text":" 你好呀 "}}]`
	ev := &Event{Message: json.RawMessage(raw)}
	if got := ev.PlainText(); got != "你好呀" {
		t.Errorf("期望 %q，实际 %q", "你好呀", got)
	}
}

func TestEventPlainTextString(t *testing.T) {
	// string 格式：字段本身是一个 JSON 字符串，内容是 CQ 码
	raw, _ := json.Marshal("[CQ:at,qq=999] 你好呀")
	ev := &Event{Message: json.RawMessage(raw)}
	if got := ev.PlainText(); got != "你好呀" {
		t.Errorf("期望 %q，实际 %q", "你好呀", got)
	}
}

func TestEventPlainTextOnlyNonTextSegments(t *testing.T) {
	// 回归测试：一条只 @ 了机器人、没有任何文字的消息。
	// NapCat 的 raw_message 是 CQ 码字符串，如果 PlainText 在"无文本段"时
	// 回退到 raw_message，模型就会收到 "[CQ:at,qq=999]" 并认真回答它。
	raw := `[{"type":"at","data":{"qq":"999"}}]`
	ev := &Event{
		Message:    json.RawMessage(raw),
		RawMessage: "[CQ:at,qq=999]",
	}
	if got := ev.PlainText(); got != "" {
		t.Errorf("纯 @ 消息不应有正文，实际 %q", got)
	}

	// 只有图片同理
	img := `[{"type":"image","data":{"file":"a.jpg"}}]`
	ev2 := &Event{Message: json.RawMessage(img), RawMessage: "[CQ:image,file=a.jpg]"}
	if got := ev2.PlainText(); got != "" {
		t.Errorf("纯图片消息不应有正文，实际 %q", got)
	}
}

func TestEventPlainTextFallsBackWhenMessageMissing(t *testing.T) {
	// 少数协议端只给 raw_message 不给 message，这时才该回退
	ev := &Event{RawMessage: "只有 raw_message"}
	if got := ev.PlainText(); got != "只有 raw_message" {
		t.Errorf("缺少 message 字段时应回退到 raw_message，实际 %q", got)
	}
}

func TestEventAtTargets(t *testing.T) {
	raw := `[{"type":"at","data":{"qq":"111"}},{"type":"at","data":{"qq":"all"}},{"type":"at","data":{"qq":"222"}}]`
	ev := &Event{Message: json.RawMessage(raw)}
	targets := ev.AtTargets()
	if len(targets) != 2 {
		t.Fatalf("期望 2 个目标，实际 %v", targets)
	}
	if targets[0] != 111 || targets[1] != 222 {
		t.Errorf("目标解析错误: %v", targets)
	}
	if !ev.Mentions(111) {
		t.Error("应识别出 @111")
	}
	if ev.Mentions(333) {
		t.Error("不应识别出 @333")
	}
}

func TestEventKind(t *testing.T) {
	group := &Event{PostType: "message", MessageType: "group"}
	if !group.IsGroup() || group.IsPrivate() || !group.IsMessage() {
		t.Error("群消息判定错误")
	}
	private := &Event{PostType: "message", MessageType: "private"}
	if !private.IsPrivate() || private.IsGroup() {
		t.Error("私聊消息判定错误")
	}
	notice := &Event{PostType: "notice"}
	if notice.IsMessage() || notice.IsGroup() {
		t.Error("通知事件不应被当成消息")
	}
}

func TestSenderDisplayName(t *testing.T) {
	if got := (Sender{Card: "群名片", Nickname: "昵称"}).DisplayName(); got != "群名片" {
		t.Errorf("应优先群名片，实际 %q", got)
	}
	if got := (Sender{Nickname: "昵称"}).DisplayName(); got != "昵称" {
		t.Errorf("无群名片应用昵称，实际 %q", got)
	}
	if got := (Sender{UserID: 123}).DisplayName(); got != "123" {
		t.Errorf("都没有应回退到 QQ 号，实际 %q", got)
	}
}
