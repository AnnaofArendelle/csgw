package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	appName = "csgw"
	// defaultListen 只监听本机：能连到这个端口的人本来就已经登录了这台机器。
	defaultListen = "127.0.0.1:2222"
	// hostAlias 是写进 ~/.ssh/config 的别名，也就是 `ssh root@codespace` 里的那个名字。
	hostAlias = "codespace"
	// autoRepoName 是账号里一个 codespace 都没有时自动创建的公开仓库名字。
	autoRepoName = "codespace-box"
	// displayMarker 是我们建的 codespace 的 display name，用于配置丢了之后重新认领它。
	displayMarker = "csgw"
)

// Config 既是配置也是需要记住的状态，就一个文件，权限 0600。
type Config struct {
	Token      string `json:"token,omitempty"`       // 留空则回退到 $GITHUB_TOKEN / $GH_TOKEN / gh auth token
	Listen     string `json:"listen,omitempty"`      // 默认 127.0.0.1:2222，只允许回环地址
	Repo       string `json:"repo,omitempty"`        // owner/name：建 codespace 的来源仓库；留空=自动建一个公开仓库
	Codespace  string `json:"codespace,omitempty"`   // 记住的 codespace 名字，第一次连上后写入，之后一直复用
	RemoteUser string `json:"remote_user,omitempty"` // 手动指定 codespace 里的登录名；留空=问 gh 并缓存
	// CachedUser 是上一次问 gh 得到的登录名。网络抖动时这个缓存能省掉一次
	// `gh codespace ssh --config` 往返；密钥被拒时会自动清掉重新问。
	CachedUser string `json:"cached_user,omitempty"`
	GHPath     string `json:"gh_path,omitempty"`     // gh 可执行文件路径；留空=自动找
	// KeepaliveSeconds 是会话期间往内层连接发 keepalive 的间隔（0=关掉，负数=用默认 60）。
	// 这点流量的作用是让 gh 持续上报"有人在用"。
	KeepaliveSeconds int `json:"keepalive_seconds,omitempty"`

	path string
	// listenFlag 是 -listen 传进来的一次性覆盖值。它故意不是导出字段：
	// remember() 会把整个结构体写回文件，命令行参数不能被这样持久化。
	listenFlag   string
	keepaliveSet bool
	mu           sync.Mutex
}

// defaultKeepalive：比 GitHub 允许的最小 idle（5 分钟）小一个数量级就够。
const defaultKeepalive = 60 * time.Second

// configDir 是配置、密钥、host key 全部待的地方（0700）。
func configDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, appName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func loadConfig() (*Config, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "config.json")
	cfg := &Config{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析 %s：%w", path, err)
	}
	cfg.keepaliveSet = bytes.Contains(raw, []byte("keepalive_seconds"))
	cfg.path = path
	return cfg, nil
}

// save 原子写入，权限 0600（里面有 token）。
func (c *Config) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// remember 记下一个字段并立刻落盘（记住 codespace 名字用的）。
func (c *Config) remember(set func(*Config)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	set(c)
	return c.saveLocked()
}

// keepalive 返回内层 keepalive 间隔，0 表示关掉。
func (c *Config) keepalive() time.Duration {
	switch {
	case c.KeepaliveSeconds < 0:
		return defaultKeepalive
	case c.KeepaliveSeconds == 0 && !c.keepaliveSet:
		return defaultKeepalive
	case c.KeepaliveSeconds == 0:
		return 0
	default:
		return time.Duration(c.KeepaliveSeconds) * time.Second
	}
}

func (c *Config) listen() string {
	if c.listenFlag != "" {
		return c.listenFlag
	}
	if c.Listen != "" {
		return c.Listen
	}
	return defaultListen
}

func (c *Config) path0() string { return c.path }

// token 按顺序找：配置文件 → 环境变量 → gh 已登录的账号。
func (c *Config) token(ctx context.Context) (string, error) {
	if t := strings.TrimSpace(c.Token); t != "" {
		return t, nil
	}
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if t := strings.TrimSpace(os.Getenv(name)); t != "" {
			return t, nil
		}
	}
	if gh, err := findGH(c.GHPath); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, gh, "auth", "token").Output()
		if t := strings.TrimSpace(string(out)); err == nil && t != "" {
			return t, nil
		}
	}
	return "", errors.New("没有可用的 GitHub token：运行 `csgw setup` 填一个，或设置 $GITHUB_TOKEN")
}

// hasToken 报告是否已经有 token（决定要不要走首次配置向导）。
func (c *Config) hasToken(ctx context.Context) bool {
	t, err := c.token(ctx)
	return err == nil && t != ""
}
