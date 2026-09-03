package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeCodespace 扮演 codespace 里的那个 sshd：真实的 SSH 服务端，exec 真的跑
// /bin/sh，并记下收到的 channel request，用来验证中继是"原样转发"的。
type fakeCodespace struct {
	signer ssh.Signer

	mu   sync.Mutex
	seen []string
	user string
}

func newFakeCodespace(t *testing.T) *fakeCodespace {
	t.Helper()
	signer, err := loadOrCreateEd25519(t.TempDir() + "/host_key")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeCodespace{signer: signer}
}

func (f *fakeCodespace) record(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, what)
}

func (f *fakeCodespace) sawRequest(what string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.seen {
		if s == what {
			return true
		}
	}
	return false
}

func (f *fakeCodespace) serve(conn net.Conn) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.mu.Lock()
			f.user = c.User()
			f.mu.Unlock()
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(f.signer)
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "只支持 session")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go f.session(ch, chReqs)
	}
}

func (f *fakeCodespace) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		f.record(req.Type)
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			ssh.Unmarshal(req.Payload, &payload)
			req.Reply(true, nil)
			f.run(ch, payload.Command)
			return
		case "shell":
			req.Reply(true, nil)
			io.WriteString(ch, "fake-codespace-shell\n")
			sendExitStatus(ch, 0)
			ch.Close()
			return
		case "subsystem":
			req.Reply(true, nil)
			io.WriteString(ch, "fake-subsystem\n")
			sendExitStatus(ch, 0)
			ch.Close()
			return
		default:
			if req.WantReply {
				req.Reply(true, nil)
			}
		}
	}
}

func (f *fakeCodespace) run(ch ssh.Channel, command string) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = 255
		}
	}
	sendExitStatus(ch, uint32(code))
	ch.Close()
}

// stubProvider 只替掉"去 GitHub 那一侧"：Ensure 不调 API，Dial 给出一条 net.Pipe
// 通到上面那个假 sshd。前门、握手、中继、keepalive 全是真实代码。
type stubProvider struct {
	cs        *fakeCodespace
	clientKey ssh.Signer
	ensureErr error

	mu      sync.Mutex
	ensured int
	dialed  int
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Ensure(ctx context.Context, notify Notify) (string, error) {
	p.mu.Lock()
	p.ensured++
	p.mu.Unlock()
	if p.ensureErr != nil {
		return "", p.ensureErr
	}
	notify("stub 环境已就绪")
	return "stub-codespace", nil
}

func (p *stubProvider) Dial(ctx context.Context, id string, notify Notify) (*Transport, error) {
	p.mu.Lock()
	p.dialed++
	p.mu.Unlock()
	mine, theirs, err := tcpPipe()
	if err != nil {
		return nil, err
	}
	go p.cs.serve(theirs)
	return &Transport{
		Conn:    mine,
		User:    "vscode",
		Signer:  p.clientKey,
		HostKey: ssh.InsecureIgnoreHostKey(),
		Desc:    "stub transport",
	}, nil
}

// tcpPipe 给出一对已连接的 net.Conn。生产环境里这一对是 gh 子进程的管道；
// 测试里用真的 TCP，因为 net.Pipe 完全不带缓冲，SSH 握手会互相等死。
func tcpPipe() (net.Conn, net.Conn, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer ln.Close()
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	got := <-ch
	if got.err != nil {
		client.Close()
		return nil, nil, got.err
	}
	return client, got.conn, nil
}

func startGateway(t *testing.T, prov Provider) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &Config{Listen: "127.0.0.1:0", path: t.TempDir() + "/config.json"}
	gw, err := newGateway(cfg, prov, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := gw.listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); gw.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return addr.String()
}

func testStub(t *testing.T) (*stubProvider, string) {
	t.Helper()
	key, err := loadOrCreateEd25519(t.TempDir() + "/client_key")
	if err != nil {
		t.Fatal(err)
	}
	prov := &stubProvider{cs: newFakeCodespace(t), clientKey: key}
	return prov, startGateway(t, prov)
}

func dialGateway(t *testing.T, addr string) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "root", // 前门不看用户名
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("连网关失败：%v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// 最基本的一条：exec 的输出、退出码、stderr 都要原样穿过网关。
func TestExecThroughGateway(t *testing.T) {
	prov, addr := testStub(t)
	client := dialGateway(t, addr)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("echo hello-codespace")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello-codespace" {
		t.Fatalf("输出不对：%q", out)
	}
	if prov.cs.user != "vscode" {
		t.Fatalf("内层登录名应该是 provider 给的 vscode，实际 %q", prov.cs.user)
	}
	if prov.ensured != 1 || prov.dialed != 1 {
		t.Fatalf("Ensure/Dial 各应该只发生一次：%d/%d", prov.ensured, prov.dialed)
	}
}

func TestExitStatusAndStderr(t *testing.T) {
	_, addr := testStub(t)
	client := dialGateway(t, addr)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("echo oops >&2; exit 7")
	var ee *ssh.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("期望拿到退出码，实际 %v", err)
	}
	if ee.ExitStatus() != 7 {
		t.Fatalf("退出码 = %d，应该是 7", ee.ExitStatus())
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Fatalf("stderr 没穿过来：%q", stderr.String())
	}
}

// pty-req / window-change / subsystem 这些我们完全没有解析的请求，必须原样到达远端。
func TestRequestsAreForwardedVerbatim(t *testing.T) {
	prov, addr := testStub(t)
	client := dialGateway(t, addr)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		t.Fatalf("pty-req: %v", err)
	}
	if err := sess.Setenv("CSGW_TEST", "1"); err != nil {
		t.Logf("env 请求被拒绝（可以接受）：%v", err)
	}
	if err := sess.WindowChange(40, 120); err != nil {
		t.Fatalf("window-change: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	sess.Wait()

	for _, want := range []string{"pty-req", "window-change", "shell"} {
		if !prov.cs.sawRequest(want) {
			t.Fatalf("远端没收到 %s；实际收到 %v", want, prov.cs.seen)
		}
	}

	sess2, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess2.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("subsystem: %v", err)
	}
	sess2.Wait()
	if !prov.cs.sawRequest("subsystem") {
		t.Fatalf("subsystem 没转发过去：%v", prov.cs.seen)
	}
}

// 一条客户端连接上的多个会话复用同一条隧道（一个 gh 子进程）。
func TestSessionsShareOneTunnel(t *testing.T) {
	prov, addr := testStub(t)
	client := dialGateway(t, addr)

	for i := 0; i < 3; i++ {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sess.Output("true"); err != nil {
			t.Fatal(err)
		}
	}
	if prov.dialed != 1 {
		t.Fatalf("同一条连接应该只拨一次，实际 %d 次", prov.dialed)
	}
}

// 拉不起 codespace 时，客户端要看到人话，而不是一个卡住的终端。
func TestEnsureFailureReachesClient(t *testing.T) {
	key, err := loadOrCreateEd25519(t.TempDir() + "/k")
	if err != nil {
		t.Fatal(err)
	}
	prov := &stubProvider{
		cs:        newFakeCodespace(t),
		clientKey: key,
		ensureErr: errors.New("token 无效或已过期"),
	}
	addr := startGateway(t, prov)
	client := dialGateway(t, addr)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("echo never")
	if err == nil {
		t.Fatal("期望失败")
	}
	if !strings.Contains(stderr.String(), "token 无效") {
		t.Fatalf("客户端没看到原因：%q", stderr.String())
	}
}

// 用系统自带的 openssh 客户端跑一遍完整链路。
func TestRealOpenSSHClient(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("本机没有 ssh 客户端")
	}
	_, addr := testStub(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-p", port,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		"root@" + host,
	}

	out, err := exec.Command(sshBin, append(args, "echo real-ssh-ok")...).Output()
	if err != nil {
		t.Fatalf("ssh 执行失败：%v", err)
	}
	if !strings.Contains(string(out), "real-ssh-ok") {
		t.Fatalf("输出不对：%q", out)
	}

	err = exec.Command(sshBin, append(args, "exit 7")...).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("系统 ssh 没拿到退出码 7：%v", err)
	}
	fmt.Fprintln(io.Discard, "ok")
}

// 前门只允许回环地址：免认证的东西不能开到网络上。
func TestRefusesNonLoopbackListen(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &Config{Listen: "0.0.0.0:2222", path: t.TempDir() + "/config.json"}
	if _, err := newGateway(cfg, &stubProvider{}, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("应该拒绝启动")
	}
}
