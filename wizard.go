package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// 首次配置向导：只有 token 是必答项，其它都有默认值。

func runWizard(ctx context.Context, cfg *Config) error {
	if !isTTY() {
		return errors.New("首次配置需要交互式终端：先运行一次 `csgw setup`，或者设置 $GITHUB_TOKEN")
	}
	ghPath, ghErr := findGH(cfg.GHPath)

	for {
		sub := fmt.Sprintf("配置文件：%s", cfg.path0())
		items := []item{
			{Label: "填写 GitHub Token", Detail: tokenDetail(cfg)},
			{Label: "用环境变量里的 token", Detail: envTokenDetail()},
			{Label: "用 gh 已登录的账号", Detail: ghAuthDetail(ctx, cfg)},
			{Label: "高级设置", Detail: advancedDetail(cfg)},
			{Label: "保存并启动", Detail: "写入配置，装好 ssh 别名，然后开始监听"},
		}
		if ghErr != nil {
			sub += "\n  " + cRed + "gh 没找到：" + ghErr.Error() + cReset
		} else {
			sub += fmt.Sprintf("　gh：%s", ghPath)
		}

		choice, err := menu("csgw · Codespace SSH 网关", sub, items, 0)
		if errors.Is(err, errBack) || errors.Is(err, errAborted) {
			return errAborted
		}
		if err != nil {
			return err
		}

		switch choice {
		case 0:
			header("填写 GitHub Token", "需要 codespace 权限；自动建仓库还需要 repo/public_repo\n  新建：https://github.com/settings/tokens")
			tok, err := password("粘贴 token（不回显）")
			if err != nil || tok == "" {
				continue
			}
			if verifyToken(ctx, tok) {
				cfg.Token = tok
				if err := cfg.save(); err != nil {
					failLine("保存失败：%v", err)
				} else {
					okLine("已写入 %s（权限 0600）", cfg.path0())
				}
			}
			pause()
		case 1:
			header("环境变量", "")
			if t := firstEnvToken(); t != "" {
				if verifyToken(ctx, t) {
					cfg.Token = ""
					_ = cfg.save()
					okLine("将直接使用环境变量里的 token（不写进配置文件）")
				}
			} else {
				failLine("$GITHUB_TOKEN / $GH_TOKEN 都是空的")
			}
			pause()
		case 2:
			header("gh 已登录的账号", "")
			cfg.Token = ""
			if t, err := cfg.token(ctx); err == nil && t != "" {
				if verifyToken(ctx, t) {
					_ = cfg.save()
					okLine("将使用 gh 的登录凭据")
				}
			} else {
				failLine("gh 还没登录：先跑 `gh auth login`，或者回上一步粘贴 token")
			}
			pause()
		case 3:
			if err := advanced(cfg); err != nil && !errors.Is(err, errBack) {
				return err
			}
		case 4:
			if !cfg.hasToken(ctx) {
				header("还差一个 token", "")
				failLine("没有可用的 token，先填一个")
				pause()
				continue
			}
			return finish(cfg)
		}
	}
}

func tokenDetail(cfg *Config) string {
	if strings.TrimSpace(cfg.Token) != "" {
		return "配置文件里已经有一个（重新填会覆盖）"
	}
	return "粘贴一次，之后就不用管了"
}

func envTokenDetail() string {
	if t := firstEnvToken(); t != "" {
		return "检测到 " + maskToken(t)
	}
	return "当前没有设置"
}

func ghAuthDetail(ctx context.Context, cfg *Config) string {
	tmp := &Config{GHPath: cfg.GHPath}
	if t, err := tmp.token(ctx); err == nil && t != "" {
		return "gh 已登录，可以直接用"
	}
	return "gh 当前未登录"
}

func advancedDetail(cfg *Config) string {
	repo := cfg.Repo
	if repo == "" {
		repo = "自动建一个公开仓库 " + autoRepoName
	}
	return fmt.Sprintf("监听 %s · 来源仓库 %s", cfg.listen(), repo)
}

func firstEnvToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func maskToken(t string) string {
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "…" + t[len(t)-4:]
}

// tokenRejected 区分"GitHub 明确说这个 token 不行"和"根本没连上 GitHub"。
// 后者不该让用户白填一遍（这台机器的网络就经常抖）。
func tokenRejected(err error) bool {
	var ae *apiError
	return errors.As(err, &ae)
}

// verifyToken 当场拿 token 去问 GitHub，别等到第一次 ssh 才发现填错了。
func verifyToken(ctx context.Context, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a := newAPI(token)
	login, err := a.currentUser(ctx)
	if err != nil {
		if tokenRejected(err) {
			failLine("token 用不了：%v", err)
			return false
		}
		failLine("没连上 GitHub：%v", err)
		fmt.Printf("  %s这看着是网络问题，不一定是 token 的错%s\n", cDim, cReset)
		return confirm("先把这个 token 存下来，等网络好了再说？", true)
	}
	okLine("token 有效：@%s", login)
	list, err := a.listCodespaces(ctx)
	if err != nil {
		fmt.Printf("  %s（列 codespace 时出错，可能是 token 缺少 codespace 权限：%v）%s\n", cDim, err, cReset)
		return true
	}
	switch len(list) {
	case 0:
		fmt.Printf("  %s账号里还没有 codespace —— 第一次 ssh 时自动建一个%s\n", cDim, cReset)
	default:
		fmt.Printf("  %s账号里有 %d 个 codespace%s\n", cDim, len(list), cReset)
	}
	return true
}

func advanced(cfg *Config) error {
	for {
		items := []item{
			{Label: "监听地址", Detail: cfg.listen() + "（只能是回环地址）"},
			{Label: "来源仓库", Detail: repoOrAuto(cfg)},
			{Label: "codespace 里的登录名", Detail: userOrAuto(cfg)},
			{Label: "返回", Detail: ""},
		}
		choice, err := menu("高级设置", "都可以留空，留空就是上面括号里的默认行为", items, 0)
		if err != nil {
			return err
		}
		switch choice {
		case 0:
			header("监听地址", "免认证的前门只允许 127.0.0.1 / localhost")
			v, _ := line("listen", cfg.listen())
			if err := checkLoopback(v); err != nil {
				failLine("%v", err)
				pause()
				continue
			}
			cfg.Listen = v
			_ = cfg.save()
		case 1:
			header("来源仓库", "第一次没有 codespace 时从哪个仓库创建。\n  留空 = 自动在你账号下建一个公开仓库 "+autoRepoName+" 当开发机载体")
			v, _ := line("owner/name（留空=自动）", cfg.Repo)
			cfg.Repo = strings.TrimSpace(v)
			_ = cfg.save()
		case 2:
			header("codespace 里的登录名", "留空 = 每次问 gh（`gh codespace ssh --config` 报什么就用什么）")
			v, _ := line("remote_user（留空=自动）", cfg.RemoteUser)
			cfg.RemoteUser = strings.TrimSpace(v)
			_ = cfg.save()
		default:
			return nil
		}
	}
}

func repoOrAuto(cfg *Config) string {
	if cfg.Repo == "" {
		return "自动建公开仓库 " + autoRepoName
	}
	return cfg.Repo
}

func userOrAuto(cfg *Config) string {
	if cfg.RemoteUser == "" {
		return "自动（问 gh）"
	}
	return cfg.RemoteUser
}

func finish(cfg *Config) error {
	header("配置完成", cfg.path0())
	okLine("%s", cfg.path0())
	fmt.Printf("\n  现在开始监听 %s，另开一个终端就能用：\n\n      %s%s%s\n\n",
		cfg.listen(), cBright, connectHint(cfg.listen()), cReset)
	return nil
}
