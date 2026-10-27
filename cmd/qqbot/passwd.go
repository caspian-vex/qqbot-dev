package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vex/qqbot/internal/config"
	"github.com/vex/qqbot/internal/panel"
	"github.com/vex/qqbot/internal/secret"
	"github.com/vex/qqbot/internal/store"
)

// cmdResetPassword 重置面板管理员密码。
//
// 存在的理由：密码是 argon2id 哈希存的，取不回明文。
// 忘了密码原本只能删库重装，代价是丢掉会话历史和全部配置。
// 这个命令只改 password_hash 一个字段，其余数据原样保留。
//
// 为什么放在二进制里而不是让用户去敲 sqlite3：
// 表结构是 app_settings(id, json)，整份配置序列化在一个 JSON 列里。
// 用 SQL 改就得写 json_set，还依赖宿主机装了 sqlite3——
// 而镜像里已经有这个二进制和 SQLite 驱动，直接调用最省事也最不容易出错。
//
// 用法（qqbotctl 会停容器后调用，避免两个进程同时写）：
//
//	docker run --rm -v /opt/qqbot/data/qqbot:/opt/qqbot/data qqbot:<tag> qqbot reset-password
func cmdResetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	dbPath := fs.String("db", "", "数据库路径（默认 <root>/data/qqbot/qqbot.db）")
	keepTOTP := fs.Bool("keep-totp", false, "保留已有的 TOTP 二次验证配置")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *dbPath
	if path == "" {
		// 优先读引导配置里的 data_dir：它可能不是默认的 <root>/data/qqbot
		// （容器里就配成了 /opt/qqbot/data）。
		if cfg, err := config.Load(config.ConfigPath()); err == nil && cfg.DataDir != "" {
			path = filepath.Join(cfg.DataDir, "qqbot.db")
		} else {
			path = filepath.Join(config.Root(), "data", "qqbot", "qqbot.db")
		}
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("找不到数据库 %s（用 --db 指定路径）", path)
	}

	st, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer st.Close()

	pw, err := secret.GenerateToken(16)
	if err != nil {
		return fmt.Errorf("生成密码失败: %w", err)
	}
	hash, err := panel.HashPassword(pw)
	if err != nil {
		return fmt.Errorf("计算密码哈希失败: %w", err)
	}

	var user string
	var hadTOTP bool
	if _, err := st.UpdateSettings(func(v *store.Settings) error {
		v.Panel.PasswordHash = hash
		if v.Panel.AdminUser == "" {
			v.Panel.AdminUser = "admin"
		}
		user = v.Panel.AdminUser
		hadTOTP = v.Panel.TOTPEnabled
		// 连 TOTP 也一起清掉：会走到"重置密码"这一步的，多半是完全进不去了。
		// 留着 TOTP 等于换个密码还是登不进。
		if !*keepTOTP {
			v.Panel.TOTPSecret = ""
			v.Panel.TOTPEnabled = false
		}
		return nil
	}); err != nil {
		return fmt.Errorf("写入新密码失败: %w", err)
	}

	fmt.Printf("\n================ 密码已重置 ================\n")
	fmt.Printf("  用户名: %s\n", user)
	fmt.Printf("  新密码: %s\n", pw)
	if !*keepTOTP && hadTOTP {
		fmt.Printf("  TOTP  : 已关闭（原配置被清除）\n")
	}
	fmt.Printf("  以上信息只显示这一次，请立即保存。\n")
	fmt.Printf("==============================================\n\n")
	return nil
}
