package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// codespacesProvider 是 GitHub Codespaces 这个 provider。
//
// 它只做三件事：找到（或建出）那一个 codespace、把它开起来、然后把连接这一步交给
// 官方的 `gh codespace ssh --stdio`（见 dial.go）。停机完全不管：gh 子进程活着的
// 时候会替我们持续告诉 codespace "有人在用"，进程一死，GitHub 自己的 idle 计时器
// 就开始跑，到点由 GitHub 停机。
type codespacesProvider struct {
	cfg *Config
	api *api
	gh  *ghCLI
	log *log.Logger

	mu     sync.Mutex // 串行化 Ensure：多个客户端同时连不会重复创建/启动
	warmID string
	warmAt time.Time
}

const (
	// ensureTimeout 覆盖"查 → 建 → 开机 → 等就绪"整段（新建一个要好几分钟）。
	ensureTimeout = 20 * time.Minute
	warmFor       = 10 * time.Second // 这段时间内不重复问 GitHub 状态
)

func newCodespacesProvider(ctx context.Context, cfg *Config, logger *log.Logger) (*codespacesProvider, error) {
	token, err := cfg.token(ctx)
	if err != nil {
		return nil, err
	}
	gh, err := newGHCLI(cfg, token)
	if err != nil {
		return nil, err
	}
	return &codespacesProvider{cfg: cfg, api: newAPI(token), gh: gh, log: logger}, nil
}

func (p *codespacesProvider) Name() string { return "github-codespaces" }

// loginFor 给出这个 codespace 该用的登录名：配置里手写的优先，其次是**属于这个
// codespace** 的缓存。换了 codespace 就返回空，让调用方重新问 gh。
func (p *codespacesProvider) loginFor(id string) string {
	if u := p.cfg.RemoteUser; u != "" {
		return u
	}
	if p.cfg.CachedUserFor == id {
		return p.cfg.CachedUser
	}
	return ""
}

// OnAuthFailure：codespace 拒绝了我们的密钥。除了登录名可能过期，刚建出来的
// codespace 也可能还没把公钥装进 authorized_keys。丢掉缓存，下一次重新问 gh。
func (p *codespacesProvider) OnAuthFailure() {
	if p.cfg.CachedUser == "" {
		return
	}
	p.log.Printf("codespace 拒绝了密钥，丢掉缓存的登录名 %q，重新问 gh", p.cfg.CachedUser)
	_ = p.cfg.remember(func(c *Config) { c.CachedUser, c.CachedUserFor = "", "" })
}

// Invalidate 丢掉"刚才已经就绪"的结论。连接失败之后由核心调用。
func (p *codespacesProvider) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warmID, p.warmAt = "", time.Time{}
}

func (p *codespacesProvider) Ensure(ctx context.Context, notify Notify) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.warmID != "" && time.Since(p.warmAt) < warmFor {
		return p.warmID, nil
	}

	ctx, cancel := context.WithTimeout(ctx, ensureTimeout)
	defer cancel()

	notify("正在检查 codespace 状态…")
	cs, err := p.resolve(ctx, notify)
	if err != nil {
		return "", err
	}
	if cs.Name != p.cfg.Codespace {
		if err := p.cfg.remember(func(c *Config) { c.Codespace = cs.Name }); err != nil {
			p.log.Printf("提醒：记住 codespace 名字失败：%v", err)
		}
	}
	if err := p.bringUp(ctx, cs, notify); err != nil {
		return "", err
	}

	p.warmID, p.warmAt = cs.Name, time.Now()
	return cs.Name, nil
}

// bringUp 把 codespace 弄到 Available：
//   - 停着（Shutdown/Archived）就请求开机
//   - 处于过渡状态（Queued/Provisioning/Starting/ShuttingDown…）就等
//   - **等的过程中被停掉也会重新开机** —— 比如 idle 到点了正在 ShuttingDown，
//     那就先等它关完，再开起来（只等不开会一直卡在 Shutdown）
func (p *codespacesProvider) bringUp(ctx context.Context, cs *codespace, notify Notify) error {
	const maxStarts = 3
	begin := time.Now()
	starts, lastStart := 0, time.Time{}
	lastState, lastTalk := "", time.Now()

	for {
		switch {
		case cs.running():
			return nil
		case cs.State == "Failed":
			return fmt.Errorf("codespace %s 进入 Failed 状态，去 %s 看一眼", cs.Name, cs.WebURL)
		case cs.State == "Deleted" || cs.State == "Moved":
			return fmt.Errorf("codespace %s 变成了 %s 状态", cs.Name, cs.State)
		case cs.stopped() && !cs.PendingOperation && time.Since(lastStart) > 30*time.Second:
			if starts >= maxStarts {
				return fmt.Errorf("请求开机 %d 次之后 codespace %s 还是 %s 状态", starts, cs.Name, cs.State)
			}
			starts++
			lastStart = time.Now()
			notify("codespace 是停止状态，正在开机…")
			if err := p.api.startCodespace(ctx, cs.Name); err != nil {
				return fmt.Errorf("启动 codespace %s：%w", cs.Name, err)
			}
		}

		if cs.State != lastState {
			notify("codespace 状态：" + cs.State)
			lastState, lastTalk = cs.State, time.Now()
		} else if time.Since(lastTalk) > 20*time.Second {
			notify(fmt.Sprintf("还在等 codespace 就绪（%s，已用 %s）",
				cs.State, time.Since(begin).Round(time.Second)))
			lastTalk = time.Now()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 codespace %s 就绪：%w", cs.Name, ctx.Err())
		case <-time.After(pollInterval):
		}
		next, err := p.api.getCodespace(ctx, cs.Name)
		if err != nil {
			return err
		}
		cs = next
	}
}

// resolve 找到那一个 codespace，顺序是：
//  1. 配置里记住的名字
//  2. display name 是 csgw 的（配置丢了也能认回来）
//  3. 账号里只有一个 codespace 的话就用它
//  4. 以上都没有 → 新建（来源仓库见 sourceRepo）
func (p *codespacesProvider) resolve(ctx context.Context, notify Notify) (*codespace, error) {
	if name := p.cfg.Codespace; name != "" {
		cs, err := p.api.getCodespace(ctx, name)
		if err == nil {
			return cs, nil
		}
		if !errors.Is(err, errNotFound) {
			return nil, err
		}
		notify(fmt.Sprintf("记住的 codespace %s 已经不在了，重新找一个", name))
	}

	list, err := p.api.listCodespaces(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].DisplayName == displayMarker {
			return &list[i], nil
		}
	}
	if len(list) == 1 {
		notify(fmt.Sprintf("认领账号里已有的 codespace：%s", list[0].Name))
		return &list[0], nil
	}
	if len(list) > 1 {
		// 有多个但没有我们建的那个：不去猜别人的，自己建一个专用的。
		sort.Slice(list, func(i, j int) bool { return list[i].LastUsedAt > list[j].LastUsedAt })
		notify(fmt.Sprintf("账号里有 %d 个 codespace，但没有 %s 专用的，新建一个", len(list), appName))
	}
	return p.create(ctx, notify)
}

func (p *codespacesProvider) create(ctx context.Context, notify Notify) (*codespace, error) {
	repo, err := p.sourceRepo(ctx, notify)
	if err != nil {
		return nil, err
	}
	notify(fmt.Sprintf("正在从 %s 新建 codespace（第一次会久一点）…", repo.FullName))
	cs, err := p.api.createCodespace(ctx, repo.ID, displayMarker)
	if err != nil {
		return nil, fmt.Errorf("从 %s 创建 codespace：%w", repo.FullName, err)
	}
	p.log.Printf("已创建 codespace %s（仓库 %s，状态 %s）", cs.Name, repo.FullName, cs.State)
	return cs, nil
}

// sourceRepo 给出建 codespace 用的仓库：配置里指定的那个，否则用（或建）
// <你的账号>/codespace-box 这个公开仓库。
func (p *codespacesProvider) sourceRepo(ctx context.Context, notify Notify) (*repository, error) {
	if nwo := p.cfg.Repo; nwo != "" {
		repo, err := p.api.getRepo(ctx, nwo)
		if err != nil {
			return nil, fmt.Errorf("查仓库 %s：%w", nwo, err)
		}
		return repo, nil
	}

	login, err := p.api.currentUser(ctx)
	if err != nil {
		return nil, err
	}
	nwo := login + "/" + autoRepoName

	repo, err := p.api.getRepo(ctx, nwo)
	if err == nil {
		_ = p.cfg.remember(func(c *Config) { c.Repo = repo.FullName })
		return repo, nil
	}
	if !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("查仓库 %s：%w", nwo, err)
	}

	notify(fmt.Sprintf("账号里没有可用的 codespace，正在建一个公开仓库 %s 当开发机载体…", nwo))
	repo, err = p.api.createPublicRepo(ctx, autoRepoName)
	if err != nil {
		return nil, fmt.Errorf("创建公开仓库 %s（token 需要 repo 或 public_repo 权限，"+
			"也可以在 config.json 里把 repo 设成已有的仓库）：%w", nwo, err)
	}
	p.log.Printf("已创建公开仓库 %s", repo.FullName)
	_ = p.cfg.remember(func(c *Config) { c.Repo = repo.FullName })
	return repo, nil
}
