package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/vex/qqbot/internal/config"
)

// cmdStatus 打印配置概览与数据目录状态。
// 它刻意不连数据库、不连协议端——运维排查时，
// "服务起不来"往往连日志都看不到，这个命令必须绝对可靠。
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", config.ConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf("版本      : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("安装根目录: %s\n", config.Root())

	fmt.Printf("配置文件  : %s", *configPath)
	if _, err := os.Stat(*configPath); err != nil {
		fmt.Printf("  [不存在]\n")
	} else {
		fmt.Printf("  [存在]\n")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("\n配置加载失败: %v\n", err)
		return nil
	}

	fmt.Printf("数据目录  : %s", cfg.DataDir)
	if st, err := os.Stat(cfg.DataDir); err == nil && st.IsDir() {
		fmt.Printf("  [存在]\n")
	} else {
		fmt.Printf("  [不存在]\n")
	}

	fmt.Printf("协议端    : %s\n", cfg.OneBot.URL)
	fmt.Printf("面板监听  : %s\n", cfg.Panel.Listen)
	fmt.Printf("日志级别  : %s\n", cfg.Log.Level)

	secretsPath := config.SecretsPath()
	secrets, err := config.LoadSecrets(secretsPath)
	if err != nil {
		fmt.Printf("密钥文件  : 读取失败 %v\n", err)
	} else {
		fmt.Printf("密钥文件  : %s", secretsPath)
		if len(secrets) == 0 {
			fmt.Printf("  [空]\n")
		} else {
			fmt.Printf("  [%d 项]\n", len(secrets))
		}
		if secrets["QQBOT_SECRET_KEY"] == "" {
			fmt.Printf("  ⚠ 缺少 QQBOT_SECRET_KEY，服务将无法启动\n")
		}
		if secrets["QQBOT_ONEBOT_TOKEN"] == "" {
			fmt.Printf("  ⚠ 缺少 QQBOT_ONEBOT_TOKEN，协议端连接可能被拒绝\n")
		}
	}

	dbPath := filepath.Join(cfg.DataDir, "qqbot.db")
	if st, err := os.Stat(dbPath); err == nil {
		fmt.Printf("数据库    : %s  (%.1f KB)\n", dbPath, float64(st.Size())/1024)
	} else {
		fmt.Printf("数据库    : %s  [尚未创建]\n", dbPath)
	}
	return nil
}

// cmdDoctor 做环境自检，输出可直接照着修的问题清单。
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var problems int
	ok := func(format string, a ...any) { fmt.Printf("  ✓ "+format+"\n", a...) }
	warn := func(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) }
	fail := func(format string, a ...any) {
		problems++
		fmt.Printf("  ✗ "+format+"\n", a...)
	}

	fmt.Println("环境")
	fmt.Printf("  · %s/%s，Go %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())

	fmt.Println("配置")
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		fail("%v", err)
	} else {
		ok("配置可解析")
	}

	fmt.Println("密钥")
	secrets, _ := config.LoadSecrets(config.SecretsPath())
	switch k := secrets["QQBOT_SECRET_KEY"]; {
	case k == "":
		fail("缺少 QQBOT_SECRET_KEY（生成：openssl rand -hex 32）")
	case len(k) != 64:
		fail("QQBOT_SECRET_KEY 长度应为 64 位十六进制，当前 %d 位", len(k))
	default:
		ok("QQBOT_SECRET_KEY 格式正确")
	}
	if secrets["QQBOT_ONEBOT_TOKEN"] == "" {
		warn("缺少 QQBOT_ONEBOT_TOKEN")
	} else {
		ok("QQBOT_ONEBOT_TOKEN 已配置")
	}

	fmt.Println("目录")
	if cfg != nil {
		if st, err := os.Stat(cfg.DataDir); err == nil && st.IsDir() {
			ok("数据目录存在")
			if err := checkWritable(cfg.DataDir); err != nil {
				fail("数据目录不可写：%v", err)
			} else {
				ok("数据目录可写")
			}
			if gb, err := diskFreeGB(cfg.DataDir); err == nil {
				if gb < 2 {
					warn("磁盘剩余仅 %.1f GB，建议至少保留 2 GB", gb)
				} else {
					ok("磁盘剩余 %.1f GB", gb)
				}
			}
		} else {
			warn("数据目录不存在（首次启动会自动创建）")
		}
	}

	fmt.Println()
	if problems > 0 {
		fmt.Printf("发现 %d 个必须修复的问题\n", problems)
		return fmt.Errorf("自检未通过")
	}
	fmt.Println("自检通过")
	return nil
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
