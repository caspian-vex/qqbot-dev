package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vex/qqbot/internal/secret"
)

// 加密字段的存储前缀，用于区分明文与密文，便于平滑升级。
const encPrefix = "enc:"

// LLMSettings 是模型接入配置。
type LLMSettings struct {
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	// Thinking 为 false 时显式关闭思考模式。
	// DeepSeek V4.1 默认开启思考模式，机器人场景必须关，否则每条消息都多几秒延迟。
	Thinking   bool `json:"thinking"`
	TimeoutSec int  `json:"timeout_sec"`
}

// BotSettings 是触发与投递配置。
type BotSettings struct {
	WakeWords        []string `json:"wake_words"`
	PassiveRate      float64  `json:"passive_rate"`
	GroupCooldownSec int      `json:"group_cooldown_sec"`
	MaxConcurrent    int      `json:"max_concurrent"`
	MaxSegments      int      `json:"max_segments"`
	SegDelayMinMS    int      `json:"seg_delay_min_ms"`
	SegDelayMaxMS    int      `json:"seg_delay_max_ms"`
	WhitelistGroups  []int64  `json:"whitelist_groups"`
	BlacklistUsers   []int64  `json:"blacklist_users"`
	QuoteReply       bool     `json:"quote_reply"`
}

// ContextSettings 是上下文窗口配置。
type ContextSettings struct {
	MaxTurns    int `json:"max_turns"`
	TokenBudget int `json:"token_budget"`
}

// PanelSettings 是面板凭据配置。
type PanelSettings struct {
	AdminUser    string `json:"admin_user"`
	PasswordHash string `json:"password_hash"`
	TOTPSecret   string `json:"totp_secret"`
	TOTPEnabled  bool   `json:"totp_enabled"`
}

// Settings 是全部运行期设置，整体以一个 JSON 文档存放。
type Settings struct {
	LLM     LLMSettings     `json:"llm"`
	Bot     BotSettings     `json:"bot"`
	Context ContextSettings `json:"context"`
	Panel   PanelSettings   `json:"panel"`
}

// DefaultSettings 返回出厂默认值。
// 默认值刻意保守：被动接话概率低、上下文预算小、思考模式关闭。
func DefaultSettings() *Settings {
	return &Settings{
		LLM: LLMSettings{
			BaseURL:     "https://api.deepseek.com",
			Model:       "deepseek-flash",
			Temperature: 1.0,
			MaxTokens:   512,
			Thinking:    false,
			TimeoutSec:  60,
		},
		Bot: BotSettings{
			WakeWords:        []string{},
			PassiveRate:      0.15,
			GroupCooldownSec: 30,
			MaxConcurrent:    3,
			MaxSegments:      4,
			SegDelayMinMS:    300,
			SegDelayMaxMS:    800,
			WhitelistGroups:  []int64{},
			BlacklistUsers:   []int64{},
			QuoteReply:       true,
		},
		Context: ContextSettings{
			MaxTurns:    10,
			TokenBudget: 3000,
		},
		Panel: PanelSettings{
			AdminUser: "admin",
		},
	}
}

// normalize 把 nil 切片收敛成空切片。
//
// 原因：JSON 里 null 与 [] 反序列化后都是 len 0，但序列化出去一个是 null、
// 一个是 []。前端拿到 null 直接 .map 会抛异常，所以对外统一成 []。
func (v *Settings) normalize() {
	if v.Bot.WakeWords == nil {
		v.Bot.WakeWords = []string{}
	}
	if v.Bot.WhitelistGroups == nil {
		v.Bot.WhitelistGroups = []int64{}
	}
	if v.Bot.BlacklistUsers == nil {
		v.Bot.BlacklistUsers = []int64{}
	}
}

// SetBox 设置用于加密敏感字段的加解密器。
func (s *Store) SetBox(b *secret.Box) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.box = b
}

// Settings 返回设置的快照（含解密后的敏感字段）。
func (s *Store) Settings() (*Settings, error) {
	s.mu.RLock()
	if s.settings != nil {
		cp := *s.settings
		s.mu.RUnlock()
		return &cp, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings != nil {
		cp := *s.settings
		return &cp, nil
	}

	var raw string
	err := s.db.QueryRow(`SELECT json FROM app_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		def := DefaultSettings()
		s.settings = def
		cp := *def
		return &cp, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取设置失败: %w", err)
	}

	var v Settings
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("解析设置失败: %w", err)
	}
	if err := s.decryptSensitive(&v); err != nil {
		return nil, err
	}
	v.normalize()
	s.settings = &v
	cp := v
	return &cp, nil
}

// SaveSettings 覆盖保存设置。
func (s *Store) SaveSettings(v *Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(v)
}

// UpdateSettings 在事务语义下修改设置：传入的函数改完即持久化。
// 任一环节失败都不会留下半写状态。
func (s *Store) UpdateSettings(fn func(*Settings) error) (*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.settingsLocked()
	if err != nil {
		return nil, err
	}
	next := *cur
	// 深拷贝切片，避免调用方修改到缓存。
	// 用 append([]T{}, ...) 而不是 append([]T(nil), ...)：
	// 后者在源为空时会返回 nil，序列化出去就变成 null。
	next.Bot.WakeWords = append([]string{}, cur.Bot.WakeWords...)
	next.Bot.WhitelistGroups = append([]int64{}, cur.Bot.WhitelistGroups...)
	next.Bot.BlacklistUsers = append([]int64{}, cur.Bot.BlacklistUsers...)

	if err := fn(&next); err != nil {
		return nil, err
	}
	if err := s.saveLocked(&next); err != nil {
		return nil, err
	}
	cp := next
	return &cp, nil
}

// settingsLocked 要求调用方已持有写锁。
func (s *Store) settingsLocked() (*Settings, error) {
	if s.settings != nil {
		cp := *s.settings
		return &cp, nil
	}
	var raw string
	err := s.db.QueryRow(`SELECT json FROM app_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		def := DefaultSettings()
		s.settings = def
		cp := *def
		return &cp, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取设置失败: %w", err)
	}
	var v Settings
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("解析设置失败: %w", err)
	}
	if err := s.decryptSensitive(&v); err != nil {
		return nil, err
	}
	v.normalize()
	s.settings = &v
	cp := v
	return &cp, nil
}

func (s *Store) saveLocked(v *Settings) error {
	// 所有写路径都经过这里，归一化放在这一处就不会漏
	v.normalize()
	enc, err := s.encryptSensitive(v)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(enc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO app_settings (id, json) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET json = excluded.json`, string(blob))
	if err != nil {
		return fmt.Errorf("写入设置失败: %w", err)
	}
	s.settings = v
	return nil
}

func (s *Store) encryptSensitive(v *Settings) (*Settings, error) {
	out := *v
	if s.box == nil {
		return &out, nil
	}
	if out.LLM.APIKey != "" && !strings.HasPrefix(out.LLM.APIKey, encPrefix) {
		c, err := s.box.Encrypt(out.LLM.APIKey)
		if err != nil {
			return nil, err
		}
		out.LLM.APIKey = encPrefix + c
	}
	if out.Panel.TOTPSecret != "" && !strings.HasPrefix(out.Panel.TOTPSecret, encPrefix) {
		c, err := s.box.Encrypt(out.Panel.TOTPSecret)
		if err != nil {
			return nil, err
		}
		out.Panel.TOTPSecret = encPrefix + c
	}
	return &out, nil
}

func (s *Store) decryptSensitive(v *Settings) error {
	if s.box == nil {
		return nil
	}
	if strings.HasPrefix(v.LLM.APIKey, encPrefix) {
		p, err := s.box.Decrypt(strings.TrimPrefix(v.LLM.APIKey, encPrefix))
		if err != nil {
			return fmt.Errorf("解密 LLM API Key 失败: %w", err)
		}
		v.LLM.APIKey = p
	}
	if strings.HasPrefix(v.Panel.TOTPSecret, encPrefix) {
		p, err := s.box.Decrypt(strings.TrimPrefix(v.Panel.TOTPSecret, encPrefix))
		if err != nil {
			return fmt.Errorf("解密 TOTP 密钥失败: %w", err)
		}
		v.Panel.TOTPSecret = p
	}
	return nil
}
