// Package persona 负责把用户写的人格提示词包装成最终的系统提示。
package persona

import (
	"strings"
)

// FormatRules 是强制追加的输出约束。
//
// 为什么必须追加：QQ 不渲染 Markdown。模型默认会输出 **加粗**、## 标题、表格、
// 有序列表，在 QQ 里就是一堆乱码符号，可读性极差。这段规则用户不可编辑，
// 避免误删后输出质量断崖式下跌。
const FormatRules = `输出要求：
- 纯文本回复，不要使用 Markdown 语法（不要用 **、#、反引号、表格、分隔线）
- 不要用列表符号，需要列举时用「、」或分号连成一句话
- 默认 1-3 句，总长度不超过 120 字；只有用户明确要求详细展开时才加长
- 不要复述用户说过的话，不要说"作为一个AI""我理解你的意思"之类的套话
- 不要输出表情包占位符或颜文字以外的特殊符号`

// Vars 是可在人格提示词里使用的变量。
type Vars struct {
	Time      string
	GroupName string
	Nickname  string
	BotName   string
}

// Build 拼装最终的系统提示词。
//
// 结构：用户人格 → 上下文变量 → 格式约束。
// 变量放在中间而不是塞进用户人格里，是为了让用户改人格时不用关心变量语法。
func Build(systemPrompt string, v Vars) string {
	var b strings.Builder

	b.WriteString(strings.TrimSpace(systemPrompt))
	b.WriteString("\n\n")

	var lines []string
	if v.BotName != "" {
		lines = append(lines, "- 你的名字："+v.BotName)
	}
	if v.Nickname != "" {
		lines = append(lines, "- 对方称呼："+v.Nickname)
	}
	if v.GroupName != "" {
		lines = append(lines, "- 当前群聊："+v.GroupName)
	}
	if v.Time != "" {
		lines = append(lines, "- 当前时间："+v.Time)
	}
	if len(lines) > 0 {
		b.WriteString("上下文：\n")
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n\n")
	}

	b.WriteString(FormatRules)
	return b.String()
}

// Expand 把 {{var}} 形式的占位符替换成实际值。
// 仅支持 4 个已知变量，未知占位符原样保留，避免误替换用户内容。
func Expand(s string, v Vars) string {
	replacer := strings.NewReplacer(
		"{{time}}", v.Time,
		"{{group_name}}", v.GroupName,
		"{{nickname}}", v.Nickname,
		"{{bot_name}}", v.BotName,
	)
	return replacer.Replace(s)
}
