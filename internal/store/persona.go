package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultPersonaName 是初始人格的名字。
const DefaultPersonaName = "default"

// DefaultSystemPrompt 是用户指定的初始人格。
// 框架会在它后面自动追加 QQ 纯文本格式约束（见 internal/persona）。
const DefaultSystemPrompt = "You are a helpful assistant"

// Persona 是一套人格设定。
type Persona struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	SystemPrompt string    `json:"system_prompt"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ActivePersona 返回当前启用的人格；没有任何人格时自动创建默认人格。
func (s *Store) ActivePersona() (*Persona, error) {
	p, err := s.scanPersona(
		`SELECT id, name, system_prompt, active, created_at, updated_at
		   FROM personas WHERE active = 1 ORDER BY id LIMIT 1`)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 首次运行：落一个默认人格
	if _, err := s.db.Exec(
		`INSERT INTO personas (name, system_prompt, active, created_at, updated_at)
		 VALUES (?, ?, 1, ?, ?)`,
		DefaultPersonaName, DefaultSystemPrompt, now(), now()); err != nil {
		return nil, fmt.Errorf("创建默认人格失败: %w", err)
	}
	return s.scanPersona(
		`SELECT id, name, system_prompt, active, created_at, updated_at
		   FROM personas WHERE name = ?`, DefaultPersonaName)
}

// ListPersonas 返回全部人格。
func (s *Store) ListPersonas() ([]Persona, error) {
	rows, err := s.db.Query(
		`SELECT id, name, system_prompt, active, created_at, updated_at
		   FROM personas ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Persona
	for rows.Next() {
		var p Persona
		var active int
		var ca, ua int64
		if err := rows.Scan(&p.ID, &p.Name, &p.SystemPrompt, &active, &ca, &ua); err != nil {
			return nil, err
		}
		p.Active = active == 1
		p.CreatedAt = time.Unix(ca, 0)
		p.UpdatedAt = time.Unix(ua, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPersona 按名字创建或更新人格。
func (s *Store) UpsertPersona(name, systemPrompt string) (*Persona, error) {
	if name == "" {
		return nil, errors.New("人格名不能为空")
	}
	_, err := s.db.Exec(
		`INSERT INTO personas (name, system_prompt, active, created_at, updated_at)
		 VALUES (?, ?, 0, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET system_prompt = excluded.system_prompt,
		                                 updated_at = excluded.updated_at`,
		name, systemPrompt, now(), now())
	if err != nil {
		return nil, fmt.Errorf("保存人格失败: %w", err)
	}
	return s.scanPersona(
		`SELECT id, name, system_prompt, active, created_at, updated_at
		   FROM personas WHERE name = ?`, name)
}

// ActivatePersona 切换启用的人格（同一个事务内取消其它人格）。
func (s *Store) ActivatePersona(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE personas SET active = 0`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE personas SET active = 1, updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) scanPersona(q string, args ...any) (*Persona, error) {
	var p Persona
	var active int
	var ca, ua int64
	err := s.db.QueryRow(q, args...).Scan(&p.ID, &p.Name, &p.SystemPrompt, &active, &ca, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Active = active == 1
	p.CreatedAt = time.Unix(ca, 0)
	p.UpdatedAt = time.Unix(ua, 0)
	return &p, nil
}
