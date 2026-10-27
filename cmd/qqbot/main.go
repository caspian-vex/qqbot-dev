package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vex/qqbot/internal/bot"
	"github.com/vex/qqbot/internal/config"
	"github.com/vex/qqbot/internal/onebot"
	"github.com/vex/qqbot/internal/panel"
	"github.com/vex/qqbot/internal/secret"
	"github.com/vex/qqbot/internal/store"
	"github.com/vex/qqbot/internal/version"
	"github.com/vex/qqbot/web"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	cmd := "help"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "version", "-v", "--version":
		fmt.Println(version.String())
		return nil
	case "run":
		return cmdRun(args)
	case "status":
		return cmdStatus(args)
	case "doctor":
		return cmdDoctor(args)
	case "reset-password":
		return cmdResetPassword(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("未知子命令: %s", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `qqbot — 轻量 QQ 机器人

用法:
  qqbot <子命令> [参数]

子命令:
  run             前台运行机器人（容器入口）
  status          查看运行状态
  doctor          环境自检
  reset-password  重置面板管理员密码（忘记密码时用）
  version         打印版本
  help            显示本帮助

配置文件: %s
数据目录: %s
`, config.ConfigPath(), filepath.Join(config.Root(), "data", "qqbot"))
}

// cmdRun 是容器入口：装配全部组件并前台运行。
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", config.ConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// 日志：面板要展示实时日志，所以先建 hub，再让 logger 双写
	hub := panel.NewLogHub(500)
	logger := newLogger(cfg.Log, hub)
	slog.SetDefault(logger)

	secrets, err := config.LoadSecrets(config.SecretsPath())
	if err != nil {
		return fmt.Errorf("读取密钥失败: %w", err)
	}

	box, err := buildBox(secrets, logger)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(cfg.DataDir, "qqbot.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetBox(box)

	if err := ensureAdminPassword(st, logger); err != nil {
		return err
	}

	token := cfg.OneBot.AccessToken
	if v := secrets["QQBOT_ONEBOT_TOKEN"]; v != "" {
		token = v
	}

	// 先建引擎再建客户端，然后互相注入，避免循环依赖
	engine := bot.New(st, nil, logger)
	client := onebot.New(cfg.OneBot.URL, token, logger, nil)
	engine.SetClient(client)
	client.SetHandler(engine.HandleEvent)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go client.Run(ctx)
	go fetchLoginInfo(ctx, client, engine, logger)

	panelSrv := panel.New(panel.Options{
		Store:   st,
		Engine:  engine,
		Client:  client,
		Log:     logger,
		Hub:     hub,
		Assets:  web.Assets(),
		Listen:  cfg.Panel.Listen,
		Version: version.Version,
	})

	logger.Info("qqbot 启动", "version", version.Version,
		"onebot", cfg.OneBot.URL, "panel", cfg.Panel.Listen)

	errCh := make(chan error, 1)
	go func() {
		if err := panelSrv.Start(ctx, cfg.Panel.Listen); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，正在关闭")
		// 给面板一点时间优雅关闭
		time.Sleep(200 * time.Millisecond)
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("面板服务异常退出: %w", err)
		}
		return nil
	}
}

// newLogger 构造同时写文件/标准输出与面板 hub 的日志器。
func newLogger(cfg config.LogConfig, hub *panel.LogHub) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var base slog.Handler
	if cfg.Format == "json" {
		base = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		base = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(panel.NewHubHandler(base, hub))
}

// buildBox 从密钥文件构造加解密器；缺失时给出可执行的修复提示。
func buildBox(secrets map[string]string, log *slog.Logger) (*secret.Box, error) {
	key := secrets["QQBOT_SECRET_KEY"]
	if key == "" {
		return nil, fmt.Errorf(
			"缺少 QQBOT_SECRET_KEY。\n"+
				"  该密钥用于加密存储模型 API Key，不能丢（丢了需要重新录入 Key）。\n"+
				"  修复：在 %s 中加入一行 QQBOT_SECRET_KEY=<64位十六进制>\n"+
				"  生成：qqbot install 会自动生成，或执行 openssl rand -hex 32",
			config.SecretsPath())
	}
	box, err := secret.New(key)
	if err != nil {
		return nil, fmt.Errorf("QQBOT_SECRET_KEY 无效: %w", err)
	}
	log.Debug("密钥已加载")
	return box, nil
}

// ensureAdminPassword 首次运行时生成管理员密码并打印一次。
func ensureAdminPassword(st *store.Store, log *slog.Logger) error {
	settings, err := st.Settings()
	if err != nil {
		return err
	}
	if settings.Panel.PasswordHash != "" {
		return nil
	}

	pw, err := secret.GenerateToken(16)
	if err != nil {
		return err
	}
	hash, err := panel.HashPassword(pw)
	if err != nil {
		return err
	}
	if _, err := st.UpdateSettings(func(v *store.Settings) error {
		v.Panel.PasswordHash = hash
		if v.Panel.AdminUser == "" {
			v.Panel.AdminUser = "admin"
		}
		return nil
	}); err != nil {
		return err
	}

	// 只在首次生成时打印一次，之后无法再查看
	fmt.Fprintf(os.Stdout, "\n================ 面板初始凭据 ================\n")
	fmt.Fprintf(os.Stdout, "  用户名: %s\n", settings.Panel.AdminUser)
	fmt.Fprintf(os.Stdout, "  密码  : %s\n", pw)
	fmt.Fprintf(os.Stdout, "  以上信息只显示这一次，请立即保存。\n")
	fmt.Fprintf(os.Stdout, "==============================================\n\n")

	log.Warn("已生成面板初始密码，请登录后立即修改")
	return nil
}

// fetchLoginInfo 在连接建立后拉取一次登录信息，用于获取机器人 QQ 号与昵称。
func fetchLoginInfo(ctx context.Context, client *onebot.Client, engine *bot.Engine, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !client.Status().Connected {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			info, err := client.GetLoginInfo(callCtx)
			cancel()
			if err != nil {
				log.Debug("获取登录信息失败", "err", err)
				continue
			}
			engine.SetSelf(info.UserID, info.Nickname)
			log.Info("已登录", "uin", info.UserID, "nickname", info.Nickname)
			return
		}
	}
}
