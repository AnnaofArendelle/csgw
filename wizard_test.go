package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHConfigBlockIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".ssh", "config")

	if _, err := installSSHConfig("127.0.0.1:2222"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "Host "+hostAlias) {
		t.Fatalf("没写进去：%s", first)
	}

	if _, err := installSSHConfig("127.0.0.1:2223"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if strings.Count(string(second), sshBegin) != 1 {
		t.Fatalf("重复写入了：\n%s", second)
	}
	if !strings.Contains(string(second), "Port 2223") {
		t.Fatalf("没更新端口：\n%s", second)
	}

	if _, err := removeSSHConfig(); err != nil {
		t.Fatal(err)
	}
	third, _ := os.ReadFile(path)
	if strings.Contains(string(third), sshBegin) {
		t.Fatalf("没删干净：\n%s", third)
	}
}

// 上一版项目写的段落定义了同一个 Host，必须被清掉，否则会盖住我们的。
func TestInstallRemovesLegacyBlockAndKeepsOthers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "config")
	existing := legacyBegin + "\nHost codespace\n    Port 2222\n" + legacyEnd + "\n" +
		"\nHost myserver\n    HostName example.com\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	msg, err := installSSHConfig("127.0.0.1:2222")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "上一版") {
		t.Fatalf("没提到清理旧段落：%s", msg)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), legacyBegin) {
		t.Fatalf("旧段落还在：\n%s", got)
	}
	if !strings.Contains(string(got), "Host myserver") {
		t.Fatalf("误伤了用户自己的条目：\n%s", got)
	}
}

// 用户自己写的 Host codespace 不能被覆盖。
func TestRefusesToClobberForeignHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "config")
	if err := os.WriteFile(path, []byte("Host codespace\n    HostName mine.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installSSHConfig("127.0.0.1:2222"); err == nil {
		t.Fatal("应该拒绝")
	}
}

// -listen 这种一次性参数不能被 remember() 写进配置文件（真踩过这个坑：
// 我用 -listen 2223 做实验，结果把用户的默认端口改成了 2223）。
func TestListenFlagIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Token = "t"
	cfg.listenFlag = "127.0.0.1:2223"
	if got := cfg.listen(); got != "127.0.0.1:2223" {
		t.Fatalf("本次运行应该用覆盖值，实际 %q", got)
	}
	if err := cfg.remember(func(c *Config) { c.Codespace = "cs-1" }); err != nil {
		t.Fatal(err)
	}
	again, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if again.Listen != "" {
		t.Fatalf("配置文件里不该出现 listen，实际 %q", again.Listen)
	}
	if again.listen() != defaultListen {
		t.Fatalf("重启后应该回到默认 %s，实际 %s", defaultListen, again.listen())
	}
	if again.Codespace != "cs-1" {
		t.Fatalf("该记住的东西没记住：%q", again.Codespace)
	}
}

// 网络不通不能被当成"token 是错的"：那样用户白填一遍还得重新去 GitHub 复制。
func TestTokenRejectedOnlyWhenGitHubSaysSo(t *testing.T) {
	githubSaidNo := &apiError{Status: 401, Method: "GET", Path: "/user", Message: "Bad credentials"}
	if !tokenRejected(githubSaidNo) {
		t.Fatal("401 应该算 token 被拒")
	}
	if !tokenRejected(fmt.Errorf("包一层：%w", githubSaidNo)) {
		t.Fatal("包过一层的 401 也应该算")
	}
	netErr := fmt.Errorf("请求 GET /user：%w", errors.New("net/http: TLS handshake timeout"))
	if tokenRejected(netErr) {
		t.Fatal("网络错误不该算 token 被拒")
	}
}
