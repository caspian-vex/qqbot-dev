package bot

import (
	"strings"
	"unicode/utf8"
)

// SplitOptions 控制回复文本的分段行为。
type SplitOptions struct {
	// MaxSegments 是单条回复最多切成几段，超出部分合并到最后一段并截断。
	// 这个上限是防刷屏的关键：模型偶尔会输出长篇大论，
	// 一次发十条消息比不发更糟。
	MaxSegments int
	// MaxRunes 是单段的字符上限。
	MaxRunes int
}

// DefaultSplit 返回默认分段参数。
func DefaultSplit() SplitOptions {
	return SplitOptions{MaxSegments: 4, MaxRunes: 120}
}

// sentenceEnders 是句子结束符。中英文都覆盖，因为模型可能中英混排。
const sentenceEnders = "。！？!?…\n"

// Split 把模型输出切成若干段，尽量在句子边界断开。
//
// 为什么不在逗号处断：QQ 消息是独立气泡，在逗号处断开会读起来很别扭，
// 像被人硬生生截断。整句发送才像人在说话。
func Split(text string, opt SplitOptions) []string {
	if opt.MaxSegments <= 0 {
		opt.MaxSegments = 4
	}
	if opt.MaxRunes <= 0 {
		opt.MaxRunes = 120
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// 先按句末标点切成句子，保留标点与原有空白。
	// 注意这里不能 TrimSpace 后再拼接，否则 "Hello! How" 会变成 "Hello!How"，
	// 英文里就成了病句。
	var sentences []string
	var cur strings.Builder
	for _, r := range text {
		cur.WriteRune(r)
		if strings.ContainsRune(sentenceEnders, r) {
			if strings.TrimSpace(cur.String()) != "" {
				sentences = append(sentences, cur.String())
			}
			cur.Reset()
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		sentences = append(sentences, cur.String())
	}

	// 把句子合并成不超过 MaxRunes 的段
	var segs []string
	var buf strings.Builder
	bufLen := 0
	for _, s := range sentences {
		n := utf8.RuneCountInString(strings.TrimSpace(s))
		if bufLen > 0 && bufLen+n > opt.MaxRunes {
			segs = append(segs, strings.TrimSpace(buf.String()))
			buf.Reset()
			bufLen = 0
		}
		buf.WriteString(s)
		bufLen += n
	}
	if bufLen > 0 {
		segs = append(segs, strings.TrimSpace(buf.String()))
	}

	if len(segs) == 0 {
		return nil
	}

	// 超出段数上限：把后面的合并进最后一段并截断
	if len(segs) > opt.MaxSegments {
		head := segs[:opt.MaxSegments-1]
		tail := strings.Join(segs[opt.MaxSegments-1:], "")
		head = append(head, truncate(tail, opt.MaxRunes))
		segs = head
	}

	// 过滤空段
	out := segs[:0]
	for _, s := range segs {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// truncate 按字符数截断，并补省略号。
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// StripMarkdown 兜底清理模型可能漏出的 Markdown 记号。
//
// 即使系统提示里明确要求纯文本，模型仍有概率输出 ** 或 #。
// 这里做一层防御，保证发到 QQ 的文字是干净的。
func StripMarkdown(s string) string {
	if s == "" {
		return s
	}
	replacements := []struct{ from, to string }{
		{"**", ""},
		{"__", ""},
		{"```", ""},
		{"`", ""},
		{"### ", ""},
		{"## ", ""},
		{"# ", ""},
		{"- ", ""},
		{"* ", ""},
	}
	out := s
	for _, r := range replacements {
		out = strings.ReplaceAll(out, r.from, r.to)
	}
	// 折叠多余空行
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}
