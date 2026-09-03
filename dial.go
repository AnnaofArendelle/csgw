package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// 连接这一步完全复用官方 GitHub CLI：
//
//	gh codespace ssh -c <name> --stdio -- -i <我们的私钥>
//
// gh 负责 Dev Tunnels、把公钥通过官方 RPC(StartRemoteServer) 注册进 codespace、
// 并且在它活着的这段时间里持续调用 NotifyCodespaceOfClientActivity 告诉 codespace
// "有人在用"。我们只是在它的 stdin/stdout 这条管道上说 SSH。
//
// 所以：子进程的生命周期 == SSH 会话的生命周期。会话断开 → 我们杀掉子进程 →
// 活跃度上报停止 → GitHub 按你账号里配置的 idle 时间自动停机。

var errStdioUnsupported = errors.New("这个 gh 版本不支持 --stdio")

// ---------- gh 命令行封装 ----------

type ghCLI struct {
	path  string
	token string
}

var ghCandidates = []string{
	"/usr/bin/gh", "/usr/local/bin/gh", "/opt/homebrew/bin/gh",
	"/home/linuxbrew/.linuxbrew/bin/gh", "/snap/bin/gh",
}

func findGH(configured string) (string, error) {
	if configured != "" {
		if filepath.IsAbs(configured) {
			if st, err := os.Stat(configured); err == nil && !st.IsDir() {
				return configured, nil
			}
			return "", fmt.Errorf("gh_path %q 不是一个可执行文件", configured)
		}
		return exec.LookPath(configured)
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p, nil
	}
	for _, c := range ghCandidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("找不到 GitHub CLI（gh）：去 https://cli.github.com 装一个，或在 config.json 里设 gh_path")
}

func newGHCLI(cfg *Config, token string) (*ghCLI, error) {
	path, err := findGH(cfg.GHPath)
	if err != nil {
		return nil, err
	}
	return &ghCLI{path: path, token: token}, nil
}

// command 构造一次 gh 调用。参数一律是独立的 argv，不经过任何 shell。
func (g *ghCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.path, args...)
	env := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GH_TOKEN="), strings.HasPrefix(kv, "GITHUB_TOKEN="),
			strings.HasPrefix(kv, "GH_ENTERPRISE_TOKEN="), strings.HasPrefix(kv, "GH_HOST="):
			continue // 只用我们自己的 token，不受继承来的凭据干扰
		}
		env = append(env, kv)
	}
	env = append(env,
		"GH_TOKEN="+g.token,
		"GITHUB_TOKEN="+g.token,
		"NO_COLOR=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
	)
	cmd.Env = env
	return cmd
}

// scrub 把 token 从要给人看的文本里抹掉。
func (g *ghCLI) scrub(s string) string {
	if len(g.token) >= 8 {
		s = strings.ReplaceAll(s, g.token, "***")
	}
	return s
}

// remoteUser 问 gh 这个 codespace 里该用哪个登录名。`gh codespace ssh --config`
// 是官方给 OpenSSH 用的配置输出，里面的 User 行就是答案。
func (g *ghCLI) remoteUser(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := g.command(ctx, "codespace", "ssh", "-c", name, "--config")
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh codespace ssh --config 失败：%s", g.scrub(strings.TrimSpace(stderr.String()+" "+err.Error())))
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		field, rest, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if ok && strings.EqualFold(field, "user") {
			if u := strings.TrimSpace(rest); u != "" {
				return u, nil
			}
		}
	}
	return "", fmt.Errorf("gh 没有报告 codespace %s 的登录名", name)
}

// ---------- Dial ----------

func (p *codespacesProvider) Dial(ctx context.Context, id string, notify Notify) (*Transport, error) {
	user := p.cfg.RemoteUser
	if user == "" {
		user = p.cfg.CachedUser
	}
	if user == "" {
		var err error
		if user, err = p.gh.remoteUser(ctx, id); err != nil {
			return nil, err
		}
		if err := p.cfg.remember(func(c *Config) { c.CachedUser = user }); err != nil {
			p.log.Printf("提醒：记住远端登录名失败：%v", err)
		}
	}
	signer, keyPath, err := ensureInnerKey()
	if err != nil {
		return nil, err
	}
	hostKeys, err := newHostKeyStore()
	if err != nil {
		return nil, err
	}

	notify("正在打开到 codespace 的隧道（gh codespace ssh --stdio）…")

	// 子进程要活过 Dial 的 ctx：ctx 只约束"建立连接"这一段，会话本身由
	// Transport.Conn 的关闭来结束。
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := p.gh.command(procCtx, "codespace", "ssh", "-c", id, "--stdio", "--", "-i", keyPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动 gh codespace ssh：%w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	conn := &pipeConn{
		r:      stdout,
		w:      stdin,
		name:   id,
		exited: exited,
		stderr: stderr,
		scrub:  p.gh.scrub,
		stop: func() {
			cancel()
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			}
		},
	}
	return &Transport{
		Conn:    conn,
		User:    user,
		Signer:  signer,
		HostKey: hostKeys.callback(id),
		Desc:    fmt.Sprintf("gh codespace ssh --stdio (%s@%s)", user, id),
		Diag:    func() string { return strings.TrimSpace(p.gh.scrub(stderr.String())) },
	}, nil
}

// ---------- 把子进程的管道当成一条 net.Conn ----------

type pipeConn struct {
	r      io.ReadCloser
	w      io.WriteCloser
	name   string
	exited chan error
	stderr *tailBuffer
	scrub  func(string) string
	stop   func()
	once   sync.Once
}

type pipeAddr struct{ name string }

func (a pipeAddr) Network() string { return "gh-stdio" }
func (a pipeAddr) String() string  { return a.name }

func (c *pipeConn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if err != nil && n == 0 {
		if detail := c.childError(); detail != nil {
			return n, detail
		}
	}
	return n, err
}

func (c *pipeConn) Write(b []byte) (int, error) { return c.w.Write(b) }

// childError 在子进程已经退出时，把它的 stderr 变成一个说得清的错误。
func (c *pipeConn) childError() error {
	select {
	case err := <-c.exited:
		c.exited <- err // 让后续调用也能看到
		detail := strings.TrimSpace(c.scrub(c.stderr.String()))
		if strings.Contains(detail, "unknown flag") && strings.Contains(detail, "stdio") {
			return fmt.Errorf("%w：%s", errStdioUnsupported, detail)
		}
		if detail == "" {
			return fmt.Errorf("gh codespace ssh 退出了：%v", err)
		}
		return fmt.Errorf("gh codespace ssh 退出了：%s", detail)
	default:
		return nil
	}
}

func (c *pipeConn) Close() error {
	c.once.Do(func() {
		_ = c.w.Close()
		_ = c.r.Close()
		if c.stop != nil {
			c.stop()
		}
	})
	return nil
}

func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr{appName} }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr{c.name} }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

// tailBuffer 只留最后 8KB，避免一个话多的子进程把内存吃掉。
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBufferMax = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBufferMax {
		t.buf = t.buf[len(t.buf)-tailBufferMax:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ---------- 网关 → codespace 用的密钥 ----------

// loadOrCreateEd25519 加载（没有就生成）一把持久化的 ed25519 密钥。
// 前门 host key 和网关→codespace 的密钥都用它，所以重启不会换钥匙。
func loadOrCreateEd25519(path string) (ssh.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("解析 %s：%w", path, err)
		}
		return signer, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, appName)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}

// ensureInnerKey 是网关登录 codespace 用的密钥。公钥文件是给 gh 读的：gh 会把它
// 通过官方 RPC(StartRemoteServer) 注册进 codespace 的 authorized_keys。
func ensureInnerKey() (ssh.Signer, string, error) {
	dir, err := configDir()
	if err != nil {
		return nil, "", err
	}
	priv := filepath.Join(dir, "codespace_ed25519")
	signer, err := loadOrCreateEd25519(priv)
	if err != nil {
		return nil, "", err
	}
	pub := priv + ".pub"
	line := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if existing, err := os.ReadFile(pub); err != nil || string(existing) != string(line) {
		if err := os.WriteFile(pub, line, 0o600); err != nil {
			return nil, "", err
		}
	}
	return signer, priv, nil
}

// ---------- codespace 的 host key：首次信任并记住 ----------

type hostKeyStore struct {
	path string
	mu   sync.Mutex
}

func newHostKeyStore() (*hostKeyStore, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	return &hostKeyStore{path: filepath.Join(dir, "known_codespaces")}, nil
}

func (s *hostKeyStore) callback(name string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		want := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))

		raw, err := os.ReadFile(s.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, ln := range strings.Split(string(raw), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			who, keyPart, ok := strings.Cut(ln, " ")
			if !ok || who != name {
				continue
			}
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(keyPart)), []byte(want)) == 1 {
				return nil
			}
			return fmt.Errorf("codespace %s 的 host key 变了（重建过就属于正常）："+
				"删掉 %s 里这一行再重连。当前指纹 %s", name, s.path, ssh.FingerprintSHA256(key))
		}

		f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "%s %s\n", name, want)
		return err
	}
}
