package main

import (
	"errors"
	"fmt"
	"testing"
)

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
