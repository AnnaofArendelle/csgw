package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

const usageText = `csgw · 把 GitHub Codespace 当普通 SSH 服务器用

用法：
  csgw                启动网关（没配置过就先走一遍向导）
  csgw setup          重新跑配置向导
  csgw ssh-config     打印 ~/.ssh/config 里那一段（-write 写入 / -remove 删除）
  csgw version

启动之后：
  ssh root@codespace

第一次连接会自动找到（或创建）那一个 codespace 并开机，之后一直复用它。
连接期间 gh 会持续告诉 GitHub "有人在用"；断开之后网关什么都不做，
由 GitHub 按你账号里的 idle 设置自动停机。
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "", "start":
		err = cmdStart(ctx, args)
	case "setup":
		err = cmdSetup(ctx)
	case "ssh-config":
		err = cmdSSHConfig(args)
	case "version", "--version":
		fmt.Printf("%s %s\n", appName, version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		err = fmt.Errorf("不认识的命令 %q\n\n%s", cmd, usageText)
	}

	if err != nil {
		if errors.Is(err, errAborted) {
			fmt.Println("\n已取消。")
			return
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
}

func cmdStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	listen := fs.String("listen", "", "监听地址（默认 "+defaultListen+"，只能是回环地址）")
	noVerify := fs.Bool("no-verify", false, "启动时不去问 GitHub 校验 token（离线/脚本场景）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.listenFlag = *listen // 只对本次运行生效，不写进配置文件
	}
	if !cfg.hasToken(ctx) {
		if err := runWizard(ctx, cfg); err != nil {
			return err
		}
	}
	if !*noVerify {
		if err := checkTokenBeforeServing(ctx, cfg); err != nil {
			return err
		}
	}

	logger := log.New(os.Stderr, "", log.Ltime)
	prov, err := newCodespacesProvider(ctx, cfg, logger)
	if err != nil {
		return err
	}
	gw, err := newGateway(cfg, prov, logger)
	if err != nil {
		return err
	}
	addr, err := gw.listen()
	if err != nil {
		return err
	}

	target := cfg.Codespace
	if target == "" {
		target = "第一次连接时自动查找/创建"
	}
	fmt.Printf("%s%s%s 监听 %s · provider %s · 目标 %s\n",
		cBright, appName, cReset, addr, prov.Name(), target)
	fmt.Printf("连接：%sssh root@%s%s    （Ctrl-C 停止网关；停机交给 GitHub 的 idle 设置）\n\n",
		cBright, hostAlias, cReset)

	return gw.serve(ctx)
}

// checkTokenBeforeServing 在开始监听之前先确认 token 还能用。
//
// 光有一个 token 字符串不等于它有效：过期、被撤销、权限不对，网关照样能起来，
// 然后每一次 ssh 都失败，而向导又因为"配置里有 token"不会弹出来。所以：
//   - GitHub 明确拒绝（401/403）→ 有终端就直接进向导重新填；没终端就报错退出，
//     让 systemd 之类别装作一切正常
//   - 只是连不上 GitHub → 打一行提醒照常启动（不能因为网络抖就不干活）
//   - token 有效但 scope 里没有 codespace → 提醒（这个坑最常见）
func checkTokenBeforeServing(ctx context.Context, cfg *Config) error {
	token, err := cfg.token(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	info, err := newAPI(token).whoami(ctx)
	switch {
	case err == nil:
		fmt.Printf("token 有效：@%s", info.Login)
		if info.Scopes != "" {
			fmt.Printf("（权限：%s）", info.Scopes)
		}
		fmt.Println()
		if info.Scopes != "" && !hasScope(info.Scopes, "codespace") {
			fmt.Fprintf(os.Stderr,
				"%s: 提醒：这个 token 没有 codespace 权限，连接时一定会失败。"+
					"去 https://github.com/settings/tokens 补上，或者 `%s setup` 换一个\n", appName, appName)
		}
		return nil

	case tokenRejected(err):
		fmt.Fprintf(os.Stderr, "%s: GitHub 拒绝了现在这个 token：%v\n", appName, err)
		if !isTTY() {
			return fmt.Errorf("换一个 token 再启动：`%s setup`（或者设置 $GITHUB_TOKEN）", appName)
		}
		fmt.Fprintf(os.Stderr, "%s: 进配置向导换一个…\n\n", appName)
		if err := runWizard(ctx, cfg); err != nil {
			return err
		}
		return nil

	default:
		fmt.Fprintf(os.Stderr, "%s: 提醒：暂时连不上 GitHub（%v），先照常启动；"+
			"真正 ssh 的时候会再试\n", appName, brief(err))
		return nil
	}
}

func cmdSetup(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := runWizard(ctx, cfg); err != nil {
		return err
	}
	fmt.Printf("  用 `%s` 启动网关。\n", appName)
	return nil
}

func cmdSSHConfig(args []string) error {
	fs := flag.NewFlagSet("ssh-config", flag.ContinueOnError)
	write := fs.Bool("write", false, "写入 ~/.ssh/config")
	remove := fs.Bool("remove", false, "从 ~/.ssh/config 删除")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch {
	case *remove:
		msg, err := removeSSHConfig()
		if err != nil {
			return err
		}
		fmt.Println(msg)
	case *write:
		msg, err := installSSHConfig(cfg.listen())
		if err != nil {
			return err
		}
		fmt.Println(msg)
	default:
		fmt.Print(sshConfigBlock(cfg.listen()))
		fmt.Println("\n加上 -write 直接写入 ~/.ssh/config。")
	}
	return nil
}
