package main

import (
	"context"
	"net"

	"golang.org/x/crypto/ssh"
)

// Notify 把一句人话进度发给正在等待的 SSH 客户端（写到它的 stderr）。
type Notify func(string)

// Provider 是一种能提供"可以 ssh 进去的远程开发环境"的后端。
//
// GitHub Codespaces 是目前唯一的实现（codespaces.go + dial.go）。要接别的云开发
// 环境，只需要新写一个文件实现这三个方法：核心（proxy.go）不认识任何一家云。
type Provider interface {
	// Name 用于日志。
	Name() string

	// Ensure 返回一个已经就绪（开机、可连）的环境 id，需要时创建并启动它。
	// 慢操作期间通过 notify 汇报进度。
	Ensure(ctx context.Context, notify Notify) (string, error)

	// Dial 打开一条到该环境内 sshd 的字节流，并给出在这条流上登录所需的凭据。
	// 关掉 Transport.Conn 就应该释放这条流背后的一切（子进程、隧道……）。
	Dial(ctx context.Context, id string, notify Notify) (*Transport, error)
}

// Transport 是"一条通到某个 sshd 的裸字节流 + 登录凭据"。
// SSH 握手、会话中继、keepalive 都由核心统一处理，provider 不用碰。
type Transport struct {
	Conn    net.Conn            // 裸 SSH 字节流（对 Codespaces 来说是 gh 子进程的管道）
	User    string              // 远端登录名
	Signer  ssh.Signer          // 用来登录的私钥
	HostKey ssh.HostKeyCallback // 校验远端 host key
	Desc    string              // 日志里的一句描述
	// Diag 返回底层通道的诊断信息（对 Codespaces 来说是 gh 的 stderr 尾部），
	// 连接意外结束时打进日志用。可以为 nil。
	Diag func() string
}
