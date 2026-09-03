package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// 网关的 SSH 前门 + 中继。
//
// 这里刻意不理解任何 SSH 会话语义：客户端发来的 channel 和 request（pty-req、env、
// exec、shell、subsystem、window-change、signal、exit-status、direct-tcpip……）
// 一律原样转给 codespace 里的 sshd，回包原样送回。所以交互 shell、`ssh cmd`、
// 精确退出码、scp/sftp、-L/-R、agent 转发都是自然可用的，代码也短。

// connectDeadline 是从"收到连接"到"内层 SSH 可用"的总预算。
const connectDeadline = 6 * time.Minute

type gateway struct {
	cfg  *Config
	prov Provider
	log  *log.Logger
	ssh  *ssh.ServerConfig
	// fingerprint 是前门 host key 的指纹，启动时打给用户看：
	// 万一 known_hosts 里有别的东西留下的 [127.0.0.1]:2222，一眼能对上。
	fingerprint string

	mu      sync.Mutex
	ln      net.Listener
	conns   map[*clientConn]struct{}
	closing bool
	nextID  int
	wg      sync.WaitGroup
}

func newGateway(cfg *Config, prov Provider, logger *log.Logger) (*gateway, error) {
	if err := checkLoopback(cfg.listen()); err != nil {
		return nil, err
	}
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	signer, err := loadOrCreateEd25519(filepath.Join(dir, "gateway_ed25519"))
	if err != nil {
		return nil, err
	}
	g := &gateway{cfg: cfg, prov: prov, log: logger, conns: map[*clientConn]struct{}{},
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}
	g.ssh = &ssh.ServerConfig{
		// 只监听回环地址，所以不要求任何凭据：能连到这个端口的人本来就已经登录了
		// 这台机器。用户名（root 或别的）只是个称呼，网关不看。
		NoClientAuth:  true,
		ServerVersion: "SSH-2.0-" + appName,
	}
	g.ssh.AddHostKey(signer)
	return g, nil
}

// checkLoopback 拒绝把免认证的前门开到网络上。
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen 地址 %q 不合法：%w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen 地址 %q 不是回环地址。前门是免认证的，只允许 127.0.0.1 / localhost；"+
		"要对外提供服务请自己在前面加一层（比如 ssh -L 或 tailscale）", addr)
}

func (g *gateway) listen() (net.Addr, error) {
	ln, err := net.Listen("tcp", g.cfg.listen())
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			return nil, fmt.Errorf("%w（可能已经有一个 %s 在跑；或者用 -listen 换个端口）", err, appName)
		}
		return nil, err
	}
	g.mu.Lock()
	g.ln = ln
	g.mu.Unlock()
	return ln.Addr(), nil
}

func (g *gateway) serve(ctx context.Context) error {
	g.mu.Lock()
	ln := g.ln
	g.mu.Unlock()
	if ln == nil {
		if _, err := g.listen(); err != nil {
			return err
		}
		g.mu.Lock()
		ln = g.ln
		g.mu.Unlock()
	}
	go func() {
		<-ctx.Done()
		g.close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			g.mu.Lock()
			closing := g.closing
			g.mu.Unlock()
			if closing {
				g.wg.Wait()
				return nil
			}
			return fmt.Errorf("accept：%w", err)
		}
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			g.handle(ctx, conn)
		}()
	}
}

func (g *gateway) close() {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return
	}
	g.closing = true
	ln := g.ln
	conns := make([]*clientConn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		c.shutdown()
	}
}

func (g *gateway) track(c *clientConn, add bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if add {
		g.nextID++
		c.id = g.nextID
		g.conns[c] = struct{}{}
	} else {
		delete(g.conns, c)
	}
}

// handle 处理一条客户端 TCP 连接：握手，然后按需拉起内层连接。
func (g *gateway) handle(ctx context.Context, raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second)) // 握手超时
	sc, chans, reqs, err := ssh.NewServerConn(raw, g.ssh)
	if err != nil {
		g.log.Printf("客户端 %s 握手失败：%v", raw.RemoteAddr(), err)
		_ = raw.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	connCtx, cancel := context.WithCancel(ctx)
	c := &clientConn{gw: g, sc: sc, ctx: connCtx, cancel: cancel, done: make(chan struct{})}
	g.track(c, true)
	g.log.Printf("[%d] 客户端已连接：%s（%s，用户名 %q 不影响远端登录名）",
		c.id, sc.RemoteAddr(), sc.ClientVersion(), sc.User())

	defer func() {
		cancel()
		close(c.done)
		_ = sc.Close()
		c.closeInner()
		g.track(c, false)
		g.log.Printf("[%d] 客户端已断开；gh 子进程已退出，接下来由 GitHub 自己的 idle 计时停机", c.id)
	}()

	go c.serveGlobalRequests(reqs)
	for nc := range chans {
		go c.serveChannel(nc)
	}
}

// clientConn 是一条客户端连接，以及它对应的那一条内层 SSH 连接。
// 一条客户端连接 = 一个 gh 子进程 = 一条隧道，进程与会话同生共死。
type clientConn struct {
	gw     *gateway
	sc     *ssh.ServerConn
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	id     int

	mu     sync.Mutex
	client *ssh.Client
	tr     *Transport
	err    error
	tried  bool
}

func (c *clientConn) shutdown() {
	c.cancel()
	_ = c.sc.Close()
}

func (c *clientConn) closeInner() {
	c.mu.Lock()
	client, tr := c.client, c.tr
	c.client, c.tr = nil, nil
	c.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	if tr != nil {
		_ = tr.Conn.Close() // 杀掉 gh 子进程 → 停止上报"有人在用"
	}
}

// inner 惰性建立内层连接：第一个 channel 到来时才开始"查/建/开机/连"。
func (c *clientConn) inner(notify Notify) (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tried {
		return c.client, c.err
	}
	c.tried = true
	client, tr, err := c.connect(notify)
	c.client, c.tr, c.err = client, tr, err
	if err != nil {
		return nil, err
	}
	c.gw.log.Printf("[%d] 内层连接已建立：%s", c.id, tr.Desc)
	if every := c.gw.cfg.keepalive(); every > 0 {
		go c.keepalive(client, every)
	}
	go c.watchInner(client, tr)
	for _, typ := range []string{"forwarded-tcpip", "forwarded-streamlocal@openssh.com", "x11", "auth-agent@openssh.com"} {
		in := client.HandleChannelOpen(typ)
		if in == nil {
			continue
		}
		go func(in <-chan ssh.NewChannel) {
			for nc := range in {
				go c.serveReverseChannel(nc)
			}
		}(in)
	}
	return client, nil
}

// connect = Ensure（查/建/开机）+ Dial（gh 隧道）+ SSH 握手，带退避重试：
// 刚创建或刚开机的 codespace 里 sshd 可能要过几秒才接受连接。
func (c *clientConn) connect(notify Notify) (*ssh.Client, *Transport, error) {
	deadline := time.Now().Add(connectDeadline)
	authRetries := 0
	for attempt := 1; ; attempt++ {
		id, err := c.gw.prov.Ensure(c.ctx, notify)
		if err != nil {
			return nil, nil, err
		}
		tr, err := c.gw.prov.Dial(c.ctx, id, notify)
		if err == nil {
			var client *ssh.Client
			client, err = handshake(c.ctx, tr)
			if err == nil {
				return client, tr, nil
			}
			_ = tr.Conn.Close()
		}
		if c.ctx.Err() != nil {
			return nil, nil, c.ctx.Err()
		}
		if inv, ok := c.gw.prov.(invalidator); ok {
			inv.Invalidate() // 刚才那个"已就绪"的结论可能已经不成立了
		}
		if isAuthFailure(err) {
			if h, ok := c.gw.prov.(authFailureReporter); ok {
				h.OnAuthFailure() // 丢掉可能过期的登录名缓存
			}
			// 密钥被拒通常有两种原因，都能自愈：登录名缓存过期（换了 codespace），
			// 或者刚建出来的 codespace 还没把公钥装进 authorized_keys。所以先重试
			// 两次（重新问 gh 拿登录名），别让用户看到一次失败的 ssh。
			authRetries++
			if authRetries <= 2 && time.Now().Before(deadline) {
				notify("codespace 拒绝了密钥，重新问一次登录名再试…")
				select {
				case <-c.ctx.Done():
					return nil, nil, c.ctx.Err()
				case <-time.After(5 * time.Second):
				}
				continue
			}
			return nil, nil, fmt.Errorf("codespace 拒绝了网关的密钥：%w\n"+
				"（自定义镜像要确认装了 sshd；也可以在 config.json 里把 remote_user 写成 codespace 里的真实用户名）", err)
		}
		if errors.Is(err, errStdioUnsupported) || time.Now().After(deadline) {
			return nil, nil, err
		}
		c.gw.log.Printf("[%d] 第 %d 次连接失败：%v", c.id, attempt, err)
		notify(fmt.Sprintf("第 %d 次连接没成（%s），3 秒后重试…", attempt, brief(err)))
		select {
		case <-c.ctx.Done():
			return nil, nil, c.ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// invalidator 是可选的 provider 能力：连接失败之后丢掉缓存的"已就绪"状态，
// 下一次重试会重新去查（可能需要重新开机）。
type invalidator interface{ Invalidate() }

// authFailureReporter 是可选的 provider 能力：密钥被拒时让它丢掉可能过期的缓存。
type authFailureReporter interface{ OnAuthFailure() }

// brief 把多行、带 URL 的底层错误压成一行，给客户端终端看；完整内容进日志。
func brief(err error) string {
	msg := strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])
	if i := strings.Index(msg, ": Get \""); i > 0 {
		msg = msg[:i] + "：网络请求失败"
	}
	if len(msg) > 150 {
		msg = msg[:150] + "…"
	}
	return msg
}

// isBadToken 认出"GitHub 说这个 token 不行"，用来给客户端一句可执行的提示。
func isBadToken(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403)
}

// isAuthFailure 认出"密钥被拒"这种重试也没用的错误。
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain")
}

// handshake 在 provider 给的字节流上完成 SSH 客户端握手。
func handshake(ctx context.Context, tr *Transport) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            tr.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(tr.Signer)},
		HostKeyCallback: tr.HostKey,
	}
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		sc, chans, reqs, err := ssh.NewClientConn(tr.Conn, tr.Desc, cfg)
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{client: ssh.NewClient(sc, chans, reqs)}
	}()
	select {
	case r := <-ch:
		return r.client, r.err
	case <-time.After(90 * time.Second):
		_ = tr.Conn.Close()
		return nil, errors.New("内层 SSH 握手超时")
	case <-ctx.Done():
		_ = tr.Conn.Close()
		return nil, ctx.Err()
	}
}

// keepalive 每分钟往内层连接发一个 keepalive 包。这点流量会经过 gh 的端口转发，
// 于是 gh 会持续调用官方的 NotifyCodespaceOfClientActivity —— 也就是替我们说
// "我还在用，别停"。会话一断，这个 goroutine 结束、子进程被杀，上报随之停止。
func (c *clientConn) keepalive(client *ssh.Client, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			ok, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				c.gw.log.Printf("[%d] keepalive 失败：%v", c.id, err)
				return
			}
			c.gw.log.Printf("[%d] keepalive ok=%v", c.id, ok)
		}
	}
}

// watchInner：codespace 那边断了（被 GitHub 停机、隧道断开）就把客户端也断掉，
// 让用户看到一次普通的 SSH 断线，而不是一个卡死的终端。
func (c *clientConn) watchInner(client *ssh.Client, tr *Transport) {
	err := client.Wait()
	select {
	case <-c.done:
		return
	default:
	}
	diag := ""
	if tr != nil && tr.Diag != nil {
		if d := tr.Diag(); d != "" {
			diag = "；gh 说：" + d
		}
	}
	c.gw.log.Printf("[%d] 内层连接结束：%v%s", c.id, err, diag)
	c.shutdown()
}

func (c *clientConn) serveGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "keepalive@openssh.com":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "tcpip-forward", "cancel-tcpip-forward",
			"streamlocal-forward@openssh.com", "cancel-streamlocal-forward@openssh.com":
			client, err := c.inner(func(string) {})
			if err != nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			ok, payload, err := client.SendRequest(req.Type, req.WantReply, req.Payload)
			if req.WantReply {
				_ = req.Reply(ok && err == nil, payload)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// serveChannel 处理客户端开的 channel。
func (c *clientConn) serveChannel(nc ssh.NewChannel) {
	if nc.ChannelType() != "session" {
		// direct-tcpip 之类：先要有内层连接，再原样开一条同类型的 channel。
		client, err := c.inner(func(string) {})
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			return
		}
		ich, ireqs, err := client.OpenChannel(nc.ChannelType(), nc.ExtraData())
		if err != nil {
			var oce *ssh.OpenChannelError
			if errors.As(err, &oce) {
				_ = nc.Reject(oce.Reason, oce.Message)
			} else {
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			}
			return
		}
		cch, creqs, err := nc.Accept()
		if err != nil {
			_ = ich.Close()
			return
		}
		relay(cch, creqs, ich, ireqs)
		return
	}

	// 会话 channel：先接下来，这样冷启动那几分钟可以往用户终端写进度。
	cch, creqs, err := nc.Accept()
	if err != nil {
		return
	}
	pump := &reqPump{}
	go func() {
		for req := range creqs {
			pump.push(req)
		}
		pump.eof()
	}()

	notify := func(msg string) {
		c.gw.log.Printf("[%d] %s", c.id, msg)
		fmt.Fprintf(cch.Stderr(), "%s: %s\r\n", appName, msg)
	}
	client, err := c.inner(notify)
	if err != nil {
		msg := fmt.Sprintf("连不上 codespace：%s", err)
		if isBadToken(err) {
			msg += fmt.Sprintf("\n%s: 换个 token：在网关那边跑 `%s setup`", appName, appName)
		}
		failSession(cch, pump, msg)
		return
	}
	ich, ireqs, err := client.OpenChannel("session", nil)
	if err != nil {
		failSession(cch, pump, fmt.Sprintf("打开远端会话失败：%s", err))
		return
	}
	pump.attach(ich)
	relayData(cch, ich, ireqs)
}

// serveReverseChannel 是反方向：codespace 主动开 channel（-R 的回连、agent 转发）。
func (c *clientConn) serveReverseChannel(nc ssh.NewChannel) {
	cch, creqs, err := c.sc.OpenChannel(nc.ChannelType(), nc.ExtraData())
	if err != nil {
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) {
			_ = nc.Reject(oce.Reason, oce.Message)
		} else {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}
	ich, ireqs, err := nc.Accept()
	if err != nil {
		_ = cch.Close()
		return
	}
	relay(cch, creqs, ich, ireqs)
}

// ---------- 通用中继 ----------

// relay 双向搬数据 + 双向转发 channel request。
func relay(cch ssh.Channel, creqs <-chan *ssh.Request, ich ssh.Channel, ireqs <-chan *ssh.Request) {
	pump := &reqPump{}
	pump.attach(ich)
	go func() {
		for req := range creqs {
			pump.push(req)
		}
		pump.eof()
	}()
	relayData(cch, ich, ireqs)
}

// relayData 搬 stdin/stdout/stderr，并把内层的 request（exit-status 等）转回客户端。
// 等内层的 request 通道关闭之后才关客户端 channel，否则退出码会丢。
func relayData(cch ssh.Channel, ich ssh.Channel, ireqs <-chan *ssh.Request) {
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		for req := range ireqs {
			forwardRequest(cch, req)
		}
	}()

	go func() {
		_, _ = io.Copy(ich, cch)
		_ = ich.CloseWrite()
	}()

	dataDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(cch, ich)
		dataDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(cch.Stderr(), ich.Stderr())
		dataDone <- struct{}{}
	}()
	<-dataDone
	<-dataDone

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
	}
	_ = cch.Close()
	_ = ich.Close()
}

// failSession 让一条会话以"说得清的错误"结束：先把原因写到客户端 stderr，再回复
// 它挂着的 exec/shell（否则客户端只会拿到一个没有下文的 255），最后给退出码。
func failSession(cch ssh.Channel, pump *reqPump, msg string) {
	fmt.Fprintf(cch.Stderr(), "%s: %s\r\n", appName, msg)
	pump.fail()
	pump.waitStart(3 * time.Second)
	sendExitStatus(cch, 255)
	_ = cch.Close()
}

// reqPump 保证 channel request 按原顺序转发，并且允许在内层 channel 还没建好时
// 先把请求攒着（冷启动期间客户端已经把 pty-req/env 发过来了）。
type reqPump struct {
	mu      sync.Mutex
	dst     ssh.Channel
	pending []*ssh.Request
	closed  bool
	failing bool

	startOnce sync.Once
	started   chan struct{}
}

func (p *reqPump) push(req *ssh.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
		return
	}
	if p.failing {
		p.answerLocally(req)
		return
	}
	if p.dst == nil {
		p.pending = append(p.pending, req)
		return
	}
	forwardRequest(p.dst, req)
}

// fail 切到"本地应答"模式：不再有内层 channel 了，请求由我们自己回。
func (p *reqPump) fail() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failing = true
	if p.started == nil {
		p.started = make(chan struct{})
	}
	for _, req := range p.pending {
		p.answerLocally(req)
	}
	p.pending = nil
}

// answerLocally 必须在持锁时调用。
func (p *reqPump) answerLocally(req *ssh.Request) {
	if req.WantReply {
		_ = req.Reply(true, nil)
	}
	switch req.Type {
	case "exec", "shell", "subsystem":
		started := p.started
		p.startOnce.Do(func() {
			if started != nil {
				close(started)
			}
		})
	}
}

// waitStart 等客户端把 exec/shell 发过来（这样它才会去读 stderr），最多等 d。
func (p *reqPump) waitStart(d time.Duration) {
	p.mu.Lock()
	ch := p.started
	p.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(d):
	}
}

func (p *reqPump) attach(dst ssh.Channel) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dst = dst
	for _, req := range p.pending {
		forwardRequest(dst, req)
	}
	p.pending = nil
}

// eof 表示客户端不会再发 request 了；还没转发出去的直接拒掉，别让客户端干等。
func (p *reqPump) eof() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, req := range p.pending {
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
	}
	p.pending = nil
}

func forwardRequest(dst ssh.Channel, req *ssh.Request) {
	ok, err := dst.SendRequest(req.Type, req.WantReply, req.Payload)
	if req.WantReply {
		_ = req.Reply(ok && err == nil, nil)
	}
}

func sendExitStatus(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

// connectHint 是要打给用户看的那条连接命令。
func connectHint(listen string) string {
	host, port := hostFor(listen)
	return fmt.Sprintf("ssh root@%s -p %s", host, port)
}

// hostFor 从 listen 地址里拆出主机和端口。
func hostFor(listen string) (host, port string) {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1", "2222"
	}
	if h == "" || h == "localhost" {
		h = "127.0.0.1"
	}
	return h, p
}
