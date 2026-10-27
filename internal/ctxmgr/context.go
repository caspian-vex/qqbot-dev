// Package ctxmgr 管理会话上下文窗口。
//
// 目标是在固定 token 预算内，尽可能保留最近、最相关的对话，
// 既不让模型"失忆"，也不让 token 成本失控。
package ctxmgr

import (
	"context"
	"fmt"

	"github.com/vex/qqbot/internal/llm"
	"github.com/vex/qqbot/internal/store"
)

// Manager 负责构建上下文并落库。
type Manager struct {
	store *store.Store
}

// New 创建上下文管理器。
func New(s *store.Store) *Manager {
	return &Manager{store: s}
}

// Build 组装一次请求的消息列表。
//
// 策略：
//  1. 取最近 maxTurns 轮（一轮 = 用户 + 助手，所以取 maxTurns*2 条）
//  2. 从最新往回累加，直到逼近 token 预算为止
//  3. 丢弃更早的整轮，保证 user/assistant 成对出现
//
// 之所以按"整轮"丢弃而不是逐条丢弃：如果上下文以 assistant 开头，
// 部分模型会表现异常（把历史回复当成待续写内容）。
func (m *Manager) Build(ctx context.Context, sessionID string, cfg store.ContextSettings, system, userText string) ([]llm.Message, error) {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 10
	}
	if cfg.TokenBudget <= 0 {
		cfg.TokenBudget = 3000
	}

	limit := cfg.MaxTurns * 2
	history, err := m.store.RecentMessages(sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取会话历史失败: %w", err)
	}

	// 预算里先扣掉固定开销：系统提示、本轮用户消息、预期输出
	budget := cfg.TokenBudget
	budget -= llm.EstimateTokens(system)
	budget -= llm.EstimateTokens(userText)
	budget -= 512 // 预留输出空间
	if budget < 0 {
		budget = 0
	}

	// 从最新往回挑，凑够预算就停
	start := len(history)
	used := 0
	for i := len(history) - 1; i >= 0; i-- {
		t := llm.EstimateTokens(history[i].Content)
		if used+t > budget && start != len(history) {
			break
		}
		used += t
		start = i
	}

	// 修正到轮次边界：如果起始位置是 assistant，往前挪一条，
	// 让上下文从 user 开始。
	if start < len(history) && history[start].Role == "assistant" && start > 0 {
		start--
	}

	msgs := make([]llm.Message, 0, len(history)-start+2)
	msgs = append(msgs, llm.Message{Role: "system", Content: system})
	for _, h := range history[start:] {
		msgs = append(msgs, llm.Message{Role: h.Role, Content: h.Content})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: userText})
	return msgs, nil
}

// Record 把一轮对话写入历史。
func (m *Manager) Record(sessionID, role, content string, tokens int) error {
	return m.store.AppendMessage(sessionID, role, content, tokens)
}

// Reset 清空某会话的上下文。
func (m *Manager) Reset(sessionID string) error {
	return m.store.ClearSession(sessionID)
}

// Stats 返回某会话的消息条数。
func (m *Manager) Stats(sessionID string) (int, error) {
	return m.store.CountMessages(sessionID)
}
