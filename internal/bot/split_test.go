package bot

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	opt := SplitOptions{MaxSegments: 4, MaxRunes: 120}

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "空字符串",
			in:   "",
			want: nil,
		},
		{
			name: "只有空白",
			in:   "   \n\t ",
			want: nil,
		},
		{
			name: "单句不加句号",
			in:   "今天天气不错",
			want: []string{"今天天气不错"},
		},
		{
			name: "两句断开",
			in:   "今天天气不错。要出去走走吗？",
			want: []string{"今天天气不错。要出去走走吗？"},
		},
		{
			name: "超长拆段",
			in:   strings.Repeat("这是一句比较长的话。", 20),
			want: nil, // 只校验数量，下面单独断言
		},
		{
			name: "英文感叹号也算句末",
			in:   "Hello there! How are you?",
			want: []string{"Hello there! How are you?"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Split(tt.in, opt)
			if tt.want == nil && tt.name != "超长拆段" {
				if len(got) != 0 {
					t.Fatalf("期望空结果，实际 %q", got)
				}
				return
			}
			if tt.name == "超长拆段" {
				if len(got) > opt.MaxSegments {
					t.Fatalf("段数 %d 超过上限 %d", len(got), opt.MaxSegments)
				}
				for i, s := range got {
					if n := len([]rune(s)); n > opt.MaxRunes+1 {
						t.Errorf("第 %d 段长度 %d 超过上限 %d", i, n, opt.MaxRunes)
					}
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("段数不符：期望 %d，实际 %d（%q）", len(tt.want), len(got), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("第 %d 段：期望 %q，实际 %q", i, tt.want[i], got[i])
				}
			}
		})
	}
}

func TestSplitRespectsMaxSegments(t *testing.T) {
	// 每句 10 字，MaxRunes=10 时每段一句，共 10 句 → 应被截到 4 段
	in := strings.Repeat("一二三四五六七八九十。", 10)
	got := Split(in, SplitOptions{MaxSegments: 4, MaxRunes: 10})
	if len(got) != 4 {
		t.Fatalf("期望 4 段，实际 %d 段", len(got))
	}
}

func TestStripMarkdown(t *testing.T) {
	tests := []struct{ in, want string }{
		{"**加粗**文本", "加粗文本"},
		{"## 标题\n正文", "标题\n正文"},
		{"`代码`", "代码"},
		{"- 列表项", "列表项"},
		{"普通文本", "普通文本"},
	}
	for _, tt := range tests {
		if got := StripMarkdown(tt.in); got != tt.want {
			t.Errorf("StripMarkdown(%q) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestMatchWakeWord(t *testing.T) {
	words := []string{"小助手", "bot"}

	if hit, rest := matchWakeWord("小助手你好", words); !hit || rest != "你好" {
		t.Errorf("中文唤醒词匹配失败: hit=%v rest=%q", hit, rest)
	}
	if hit, rest := matchWakeWord("bot 在吗", words); !hit || rest != "在吗" {
		t.Errorf("英文唤醒词匹配失败: hit=%v rest=%q", hit, rest)
	}
	if hit, _ := matchWakeWord("你好小助手", words); hit {
		t.Error("唤醒词只应匹配开头，不应匹配中间")
	}
	if hit, _ := matchWakeWord("随便说点什么", words); hit {
		t.Error("不应误命中")
	}
}

func TestContainsInt64(t *testing.T) {
	list := []int64{1, 2, 3}
	if !containsInt64(list, 2) {
		t.Error("应命中 2")
	}
	if containsInt64(list, 4) {
		t.Error("不应命中 4")
	}
	if containsInt64(nil, 1) {
		t.Error("空列表不应命中")
	}
}
