// Package config 负责加载 qqbot 的引导配置与密钥。
//
// 设计上刻意把配置分成两层：
//   - 引导配置（etc/qqbot.toml）：路径、监听地址、协议端连接串这类部署期确定的项，
//     由安装脚本生成，人工可改但面板不写。
//   - 运行期设置（SQLite settings 表）：LLM、人格、触发规则这类高频调整的项，
//     面板可写，支持热更新。
//
// 分开的原因：如果面板也写 TOML，注释和人工改动会被冲掉，且并发写有竞态。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// defaultRoot 是默认安装根目录。本地开发可用 QQBOT_ROOT 覆盖。
const defaultRoot = "/opt/qqbot"

// Config 是引导配置。
type Config struct {
	Root    string       `toml:"-"`
	DataDir string       `toml:"data_dir"`
	Log     LogConfig    `toml:"log"`
	OneBot  OneBotConfig `toml:"onebot"`
	Panel   PanelConfig  `toml:"panel"`
}

// LogConfig 控制日志行为。默认 warn，避免低配云盘被日志写爆。
type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// OneBotConfig 描述协议端（NapCat）的连接信息。
type OneBotConfig struct {
	URL         string `toml:"url"`
	AccessToken string `toml:"access_token"`
}

// PanelConfig 描述 Web 面板的监听参数。
type PanelConfig struct {
	Listen string `toml:"listen"`
}

// Default 返回一份可直接运行的默认配置。
func Default() *Config {
	root := Root()
	return &Config{
		Root:    root,
		DataDir: filepath.Join(root, "data", "qqbot"),
		Log: LogConfig{
			Level:  "warn",
			Format: "text",
		},
		OneBot: OneBotConfig{
			URL: "ws://127.0.0.1:3001",
		},
		Panel: PanelConfig{
			Listen: "127.0.0.1:8080",
		},
	}
}

// Root 返回安装根目录，优先取 QQBOT_ROOT 环境变量。
func Root() string {
	if v := strings.TrimSpace(os.Getenv("QQBOT_ROOT")); v != "" {
		return v
	}
	return defaultRoot
}

// ConfigPath 返回引导配置文件路径。
func ConfigPath() string {
	return filepath.Join(Root(), "etc", "qqbot.toml")
}

// SecretsPath 返回密钥文件路径。
func SecretsPath() string {
	return filepath.Join(Root(), "etc", "secrets.env")
}

// Load 从 path 读取配置。文件不存在时返回默认配置并给出提示错误。
func Load(path string) (*Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("配置文件不存在: %s（先运行 qqbot install）", path)
		}
		return nil, err
	}
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		// 不阻断启动，只提示：升级后旧配置里多出的键不应导致无法运行
		fmt.Fprintf(os.Stderr, "警告: %s 中有未知配置项 %v，已忽略\n", path, undecoded)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	if c.DataDir == "" {
		c.DataDir = filepath.Join(c.Root, "data", "qqbot")
	}
	if c.Log.Level == "" {
		c.Log.Level = "warn"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.OneBot.URL == "" {
		return errors.New("onebot.url 不能为空")
	}
	if c.Panel.Listen == "" {
		c.Panel.Listen = "127.0.0.1:8080"
	}
	return nil
}

// Save 把配置写回 path（安装脚本使用）。
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

// LoadSecrets 读取 etc/secrets.env（KEY=VALUE 每行一条），并与进程环境合并。
// 进程环境优先，便于用 docker -e 覆盖。
func LoadSecrets(path string) (map[string]string, error) {
	secrets := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		secrets[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	for _, k := range []string{"QQBOT_SECRET_KEY", "QQBOT_ONEBOT_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			secrets[k] = v
		}
	}
	return secrets, nil
}

// EnvInt 读取整型环境变量，失败时返回 def。
func EnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
