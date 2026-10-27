package store

import (
	"time"
)

// UsageSummary 是某一天的用量汇总。
type UsageSummary struct {
	Day              string  `json:"day"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int64   `json:"requests"`
	PeakRequests     int64   `json:"peak_requests"`
	CostCNY          float64 `json:"cost_cny"`
}

// RecordUsage 记录一次模型调用的 token 消耗。
func (s *Store) RecordUsage(model string, promptTokens, completionTokens int, peak bool) error {
	day := time.Now().Format("2006-01-02")
	p := 0
	if peak {
		p = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO usage_log (day, model, prompt_tokens, completion_tokens, peak, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		day, model, promptTokens, completionTokens, p, now())
	return err
}

// UsageForDay 汇总指定日期的用量，并按 DeepSeek Flash 峰谷价估算费用。
// 空闲：输入 1 元 / 输出 4 元 每百万 token；高峰翻倍。
func (s *Store) UsageForDay(day string) (*UsageSummary, error) {
	rows, err := s.db.Query(
		`SELECT prompt_tokens, completion_tokens, peak FROM usage_log WHERE day = ?`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sum := &UsageSummary{Day: day}
	for rows.Next() {
		var pt, ct, peak int64
		if err := rows.Scan(&pt, &ct, &peak); err != nil {
			return nil, err
		}
		sum.PromptTokens += pt
		sum.CompletionTokens += ct
		sum.Requests++
		if peak == 1 {
			sum.PeakRequests++
		}
		// 分时段计价：高峰单价是空闲的两倍
		mult := 1.0
		if peak == 1 {
			mult = 2.0
		}
		sum.CostCNY += (float64(pt)*1.0 + float64(ct)*4.0) / 1_000_000 * mult
	}
	return sum, rows.Err()
}

// RecentUsage 返回最近 n 天的用量汇总（按日期升序）。
func (s *Store) RecentUsage(n int) ([]UsageSummary, error) {
	if n <= 0 {
		n = 7
	}
	rows, err := s.db.Query(
		`SELECT day FROM usage_log GROUP BY day ORDER BY day DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		days = append(days, d)
	}
	rows.Close()

	out := make([]UsageSummary, 0, len(days))
	// 倒序收集，最后反转成升序
	for i := len(days) - 1; i >= 0; i-- {
		sum, err := s.UsageForDay(days[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *sum)
	}
	return out, nil
}

// AuditEntry 是一条操作审计记录。
type AuditEntry struct {
	ID        int64     `json:"id"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Remote    string    `json:"remote"`
	CreatedAt time.Time `json:"created_at"`
}

// RecordAudit 写入一条审计记录。
func (s *Store) RecordAudit(action, detail, remote string) {
	_, _ = s.db.Exec(
		`INSERT INTO audit_log (action, detail, remote, created_at) VALUES (?, ?, ?, ?)`,
		action, detail, remote, now())
}

// ListAudit 返回最近的审计记录。
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, action, detail, remote, created_at FROM audit_log
		  ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&e.ID, &e.Action, &e.Detail, &e.Remote, &ts); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
