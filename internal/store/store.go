// Package store 封装 SQLite 持久化。
//
// 用 modernc.org/sqlite（纯 Go 实现）而非 mattn/go-sqlite3，
// 目的是 CGO_ENABLED=0 就能交叉编译出 linux/amd64 与 arm64，
// 开发机是 Windows 也不需要 C 工具链。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vex/qqbot/internal/secret"

	_ "modernc.org/sqlite"
)

// Store 是数据库句柄。
type Store struct {
	db   *sql.DB
	path string
	box  *secret.Box

	mu       sync.RWMutex
	settings *Settings
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	// WAL + NORMAL：兼顾并发读与写入吞吐，同时避免每条消息 fsync。
	// 低配云盘的 IOPS 很低，这一项对稳定性影响很大。
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 单机单进程，连接数不必大；限制住可以省内存。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS app_settings (
			id   INTEGER PRIMARY KEY CHECK (id = 1),
			json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS personas (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			system_prompt TEXT NOT NULL,
			active        INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			kind       TEXT NOT NULL,
			target_id  INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			tokens     INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id)`,
		`CREATE TABLE IF NOT EXISTS usage_log (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			day               TEXT NOT NULL,
			model             TEXT NOT NULL,
			prompt_tokens     INTEGER NOT NULL,
			completion_tokens INTEGER NOT NULL,
			peak              INTEGER NOT NULL,
			created_at        INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_day ON usage_log(day)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			action     TEXT NOT NULL,
			detail     TEXT NOT NULL,
			remote     TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("迁移失败: %w\nSQL: %s", err, q)
		}
	}
	return nil
}

// now 返回 Unix 秒。
func now() int64 { return time.Now().Unix() }

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")
