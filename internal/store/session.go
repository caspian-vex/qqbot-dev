package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Message 是一条会话消息。
type Message struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Tokens    int       `json:"tokens"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionInfo 是会话列表的一项。
type SessionInfo struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	TargetID  int64     `json:"target_id"`
	Messages  int       `json:"messages"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionID 拼接会话标识。群与私聊分开，避免同号冲突。
func SessionID(kind string, targetID int64) string {
	return fmt.Sprintf("%s:%d", kind, targetID)
}

// TouchSession 更新会话的最后活跃时间（不存在则创建）。
func (s *Store) TouchSession(sessionID, kind string, targetID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, kind, target_id, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET updated_at = excluded.updated_at`,
		sessionID, kind, targetID, now())
	return err
}

// AppendMessage 追加一条消息。
func (s *Store) AppendMessage(sessionID, role, content string, tokens int) error {
	_, err := s.db.Exec(
		`INSERT INTO messages (session_id, role, content, tokens, created_at) VALUES (?, ?, ?, ?, ?)`,
		sessionID, role, content, tokens, now())
	if err != nil {
		return fmt.Errorf("写入消息失败: %w", err)
	}
	return nil
}

// RecentMessages 按时间正序返回最近 limit 条消息。
func (s *Store) RecentMessages(sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, tokens, created_at
		   FROM messages WHERE session_id = ? ORDER BY id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取消息失败: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var ts int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Tokens, &ts); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(ts, 0)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转为时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CountMessages 返回某会话的消息条数。
func (s *Store) CountMessages(sessionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

// ClearSession 清空某会话的消息，但保留会话记录（面板里仍可见）。
func (s *Store) ClearSession(sessionID string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("清空会话失败: %w", err)
	}
	_, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now(), sessionID)
	return err
}

// DeleteSession 删除会话及其消息。
func (s *Store) DeleteSession(sessionID string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

// ListSessions 返回会话列表，按最近活跃排序。
func (s *Store) ListSessions() ([]SessionInfo, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.kind, s.target_id, s.updated_at,
		       (SELECT COUNT(1) FROM messages m WHERE m.session_id = s.id) AS cnt
		  FROM sessions s ORDER BY s.updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("读取会话列表失败: %w", err)
	}
	defer rows.Close()

	var out []SessionInfo
	for rows.Next() {
		var it SessionInfo
		var ts int64
		if err := rows.Scan(&it.ID, &it.Kind, &it.TargetID, &ts, &it.Messages); err != nil {
			return nil, err
		}
		it.UpdatedAt = time.Unix(ts, 0)
		out = append(out, it)
	}
	return out, rows.Err()
}

// PruneMessages 删除超过保留期的消息，防止数据库无限增长。
// 低配机器上这一点很重要：messages 表是最容易撑爆磁盘的地方。
func (s *Store) PruneMessages(retain time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retain).Unix()
	res, err := s.db.Exec(`DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ErrSessionNotFound 供上层判断。
var ErrSessionNotFound = errors.New("会话不存在")

// SessionExists 判断会话是否存在。
func (s *Store) SessionExists(sessionID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
