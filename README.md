# csgw

把 GitHub Codespace 当成一台按需开机的普通 SSH 服务器用：

```bash
csgw                  # 启动网关（第一次会问一个 token）
ssh root@codespace    # 完事
```

用不着知道 Codespaces、`gh`、Dev Tunnels 的存在。一条 `ssh` 背后：

```
ssh root@codespace
  → 网关前门（只监听 127.0.0.1，免认证）
  → 找到那一个 codespace（记住的名字 → display name=csgw → 账号里唯一的那个 → 都没有就新建）
  → 停止状态就开机，等到 Available
  → gh codespace ssh --stdio 打开官方隧道
  → 通道原样中继：你的终端就是 codespace 里的终端
断开
  → 杀掉 gh 子进程 → 停止上报"有人在用" → GitHub 按你账号里的 idle 设置自动停机
```

## 按需计费是怎么做到的

不是自己发明的保活机制，用的是 GitHub CLI 里已有的官方通道：

- `gh codespace ssh` 活着的时候，它会周期性调用
  `Codespaces.Grpc.CodespaceHostService.v1/NotifyCodespaceOfClientActivity`
  （client id `gh_cli`），也就是**替我们告诉 codespace"有人在用，别停"**。
- 网关让这个子进程**和 SSH 会话同生共死**：会话建立时启动，最后一个会话断开时杀掉。
- 断开之后网关**什么都不做**：不调 `/stop`、不发保活、不伪造活跃。
  停机时间完全按你 GitHub 账号（github.com/settings/codespaces）里的 idle 设置走。
- 会话期间网关每 60 秒往内层连接发一个 `keepalive@openssh.com`。这点流量会经过
  gh 的端口转发，让上面那个活跃度上报一直有内容可报，**空闲挂着的终端不会被判定成没人用**。

所以计费 = 实际用时 + 一次 idle 窗口。想更省就把账号里的 idle 时间调小（GitHub 允许最低 5 分钟）。

## 需要什么

- **GitHub token**：要 `codespace` 权限；如果让它自动建仓库，还要 `repo`（或 `public_repo`）。
  查找顺序：`config.json` → `$GITHUB_TOKEN` / `$GH_TOKEN` → `gh auth token`。
- **GitHub CLI (`gh`)**：只有"建立连接"这一步用它。网关不重新实现 Dev Tunnels，也不碰
  Codespaces 内部 RPC。`--stdio` 是 `gh codespace ssh --config` 生成的 ProxyCommand
  用的同一个机制（gh ≥ 2.30 都有）。
- Go 1.24+（只依赖 `golang.org/x/crypto`、`golang.org/x/term`）。

## 编译

```bash
./build.sh          # 产出 ./csgw
```

这台机器上系统 go 是 1.22 而 `x/crypto` 要 1.24，`build.sh` 会自动用模块缓存里
解压好的 go1.25.5 工具链，并且全程离线（`GOPROXY=off`）。

## 第一次跑

`csgw` 没有配置时进入上下键界面（↑↓ 选择、Enter 确认、Esc 返回、Ctrl-C 退出）：

```
============================================
  csgw · Codespace SSH 网关
  配置文件：~/.config/csgw/config.json　gh：/usr/bin/gh
============================================

 ★ 填写 GitHub Token
      粘贴一次，之后就不用管了
  ☆ 用环境变量里的 token
  ☆ 用 gh 已登录的账号
  ☆ 高级设置
  ☆ 保存并启动
```

token 输入不回显，当场拿去 `GET /user` 验证。保存时会把这段幂等地写进 `~/.ssh/config`：

```
Host codespace
    HostName 127.0.0.1
    Port 2222
    User root
    StrictHostKeyChecking accept-new
    UserKnownHostsFile ~/.ssh/known_hosts.csgw
    ControlMaster auto
    ControlPath ~/.ssh/csgw-%r@%h-%p
    ControlPersist 10s
    ServerAliveInterval 60
```

`ControlMaster` 让第二、第三个终端秒开并复用同一条隧道；最后一个终端退出 10 秒后隧道才断。
`root` 只是个称呼：网关用 `gh codespace ssh --config` 报告的真实登录名（`vscode`/`codespace`…）
进 codespace，所以 `ssh 任意名字@codespace` 都能连，进去以后是那个真实用户（要 root 就 `sudo -i`）。

## 第一个 codespace 从哪来

账号里一个 codespace 都没有时，网关会在你账号下建一个**公开**仓库 `codespace-box`
（带一个初始提交），然后从它建 codespace，display name 记作 `csgw`。
机器规格、地区、idle 时间全部不传，用 GitHub 的默认值。

- 仓库是公开的，它只是"建 codespace 的载体"——别往里 commit 私密东西。
- 想用自己的仓库：向导里"高级设置 → 来源仓库"，或直接改 `config.json` 的 `repo`。
- codespace 被删掉再连，会按同样的规则重新建一个。

## 命令

```
csgw                启动（没配置过就先走向导）
csgw -listen 127.0.0.1:2223   换端口（只影响本次，不写进配置文件）
csgw -no-verify     启动时不问 GitHub 校验 token（离线/脚本场景）
csgw setup          重新配置
csgw ssh-config     打印那段 ssh 配置（-write 写入 / -remove 删除）
csgw version
```

### token 填错 / 过期 / 权限不对会怎样

"配置里有一个 token"不等于它能用，所以启动时会先问一次 GitHub（`GET /user`，一次往返）：

| 情况 | 行为 |
|---|---|
| 有效 | 打印 `token 有效：@你的账号（权限：…）`，正常启动 |
| 有效但没有 `codespace` 权限 | 照常启动，但明确警告"连接时一定会失败"（这个坑最常见） |
| GitHub 明确拒绝（401/403） | **有终端就直接进向导让你换一个**；没终端（systemd/脚本）就报错退出 1，不会装作一切正常 |
| 连不上 GitHub | 只打一行提醒，照常启动 —— 网络抖不该让网关罢工，真正 ssh 的时候会再试 |
| 不想启动时联网 | 加 `-no-verify` |

没有 token 时：非交互环境报错退出（并告诉你跑 `csgw setup`），交互环境进向导；
向导里不填 token 就选"保存并启动"会被拦住，Esc 退出则不留任何半成品配置文件。
运行期间 token 被撤销的话，错误会打到 ssh 客户端的终端上，并附一句"跑 `csgw setup` 换一个"。

配置和密钥都在 `~/.config/csgw/`（0700）：`config.json`(0600)、`gateway_ed25519`（前门 host key，
固定不换，所以不会报 host key 变了；真丢了就删掉 `~/.ssh/known_hosts.csgw`）、`codespace_ed25519(.pub)`（登录 codespace 用，由 gh 注册进去）、
`known_codespaces`（记住的远端 host key）。

## 代码结构

```
main.go          命令行入口
tui.go           上下键菜单 / 掩码输入
wizard.go        首次配置向导 + ~/.ssh/config 段落维护
config.go        配置与状态（就一个 json 文件）
provider.go      Provider 接口（20 行，给以后接别的云开发环境留的口）
api.go           GitHub REST：/user、list、get、create、start、轮询
codespaces.go    Provider 实现：找到/新建/开机那一个 codespace
dial.go          gh --stdio 隧道、密钥、远端 host key
proxy.go         SSH 前门 + 通用 channel 中继 + keepalive
```

**为什么 `proxy.go` 不解析 SSH 语义**：客户端的 channel 和 request（`pty-req`、`env`、
`exec`、`shell`、`subsystem`、`window-change`、`signal`、`exit-status`、`direct-tcpip`…）
一律原样转发，回包原样送回。所以交互 shell、`ssh cmd`、精确退出码、`scp`/`sftp`、
`-L`/`-R`、agent 转发都是自然可用的，代码也短。

## 安全

- 前门只监听回环地址且免认证：能连到这个端口的人本来就已经登录了这台机器。
  监听地址不是回环地址时**拒绝启动**，不会把 codespace 静默暴露到网络上。
- token 只写在 0600 的配置文件里；给 gh 的时候先清掉继承来的 GitHub 凭据，
  出错信息里的 token 会被替换成 `***`。
- 所有外部命令都是结构化 argv，没有任何 `sh -c`。
- 网关→codespace 这一跳的 host key 首次信任并记住（`known_codespaces`），
  变了会报错并告诉你删哪一行（codespace 重建过就属于正常）。

## 测试

```bash
go test ./...          # 17 个测试，约 0.3 秒
go test -race ./...
```

跑的是真实代码路径：真 SSH 服务端 + 真 SSH 客户端（`x/crypto/ssh` 和系统 `ssh` 二进制）+
真 HTTP + 真 REST 客户端；只有 GitHub 那一侧被替换（`httptest` 假 API、一个扮演
codespace sshd 的进程内 SSH 服务端）。覆盖：没有 codespace 时建仓库+建 codespace、
停止状态开机一次、记住的名字失效后认领账号里唯一那个、token 无效的报错不泄漏 token、
exec 输出/退出码/stderr 穿透、pty-req/window-change/subsystem 原样转发、
同一条连接多会话复用一条隧道、拉不起 codespace 时客户端看到人话、
真实 OpenSSH 客户端取到退出码 7、非回环监听拒绝启动、`~/.ssh/config` 段落幂等+不误伤。

## 实战验证结果（2026-09-04，真 token / 真 codespace）

跑通了。账号 @AnnaofArendelle，`gh` 2.99.0，全程只用一条 `ssh root@codespace`：

| 验证项 | 结果 |
|---|---|
| 认领账号里已有的 codespace | ✓ `codespace-q4j9qqw5rwrc677v`（Shutdown）→ 自动开机 → 连上 |
| 冷启动全过程可见 | ✓ 终端里依次出现 检查状态 → 认领 → 开机 → 状态 Queued → 打开隧道 |
| 进去之后 | ✓ `whoami`=vscode，`hostname`=codespaces-bebb72，`CODESPACES=true` |
| 要 root | ✓ `sudo -n whoami` → root（免密 sudo） |
| 退出码 | ✓ `ssh root@codespace 'exit 7'` → 7 |
| stderr 分离 | ✓ stdout/stderr 各走各的 |
| scp 上传 / 下载 | ✓ 双向都通 |
| 一条隧道多会话 | ✓ ControlMaster 复用，4 个会话共用一个 gh 子进程 |
| 断开即停止上报 | ✓ 日志：`客户端已断开；gh 子进程已退出` |
| 新建 codespace 这条路 | ✓ 从 `AnnaofArendelle/codespace-box` 建出 `csgw-756r99g6rjvfw6rr`（display=csgw），随后删除 |
| 网络抖动下的重试 | ✓ 第一次连接时本机到 api.github.com / tunnels 反复 TLS 超时，重试 16 次后连上（约 4 分钟） |
| 把云端 codespace 删掉再连 | ✓ 自动从 `codespace-box` 重建并连上 |
| 重建后登录名变了 | ✓ 旧镜像是 `vscode`、默认镜像是 `codespace`；密钥被拒后自动重新问 gh，**同一次 ssh 内**自愈 |
| 等待期间被 idle 停机 | ✓ `ShuttingDown → Shutdown` 会等它关完再开机（不会卡在 Shutdown） |

三个已知事实：

- **`last_used_at` 不是活跃度指标**：它只在 codespace 被启动时更新，会话期间不动（加上 gh 自己用的
  `?internal=true&refresh=true` 也一样）。判断"有没有被算作在用"只能看它到底什么时候被停机。
- 实战里第一次连接失败最多的一步是 `gh codespace ssh --config`（探远端登录名）。所以探到的
  登录名会缓存进 `config.json` 的 `cached_user`，并且**绑定到具体 codespace**
  （`cached_user_for`）；换了 codespace 一定重新问，密钥被拒时也会清掉重问。
- **idle 时间来自你 GitHub 账号的设置**，网关一个字都不传。这个账号目前是 5 分钟：
  实测断开约 4–5 分钟后 codespace 自动 `Shutdown`。想更省/更耐用就去
  github.com/settings/codespaces 改，网关不用动。

### idle 归属：已验证（这是整个工具的核心承诺）

这个 codespace 的 `idle_timeout_minutes=5`（来自账号设置）。挂一条**完全空闲**的
SSH 会话（远端只有一个 `sleep`，没有任何输入输出）：

```
[01:42:16] 起会话（12 分钟完全空闲），idle_timeout=5 分钟
[01:44:16] 空闲第 2 分钟：Available
[01:46:17] 空闲第 4 分钟：Available
[01:48:17] 空闲第 6 分钟：Available      ← 已经超过 5 分钟 idle，没被停
[01:52:23] 空闲第 10 分钟：Available
[01:54:38] 已断开
[01:56:39] 断开后 2 分钟：Available
[01:58:41] 断开后 4 分钟：Available
[02:00:59] 断开后 6 分钟：Shutdown      ← GitHub 自己停的，网关没插手
```

结论：会话开着就不会被 idle 停掉（gh 的 `NotifyCodespaceOfClientActivity` 生效），
断开之后由 GitHub 按账号里的 idle 设置自己停机。按需计费闭环成立。

如果哪天发现"挂着的会话被停机了"，补救办法是把 `keepalive_seconds`（config.json）调小，
或者在会话里跑个真实活动。
