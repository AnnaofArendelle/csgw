#!/bin/sh
# 编译。两种情况都照顾：
#
#   1. 有网络（或者本机 go 够新）：直接用 `go`，正常下载依赖。
#      Termux 里就是这条路：pkg install golang git，然后 ./build.sh
#   2. 写这份代码的那台机器：/usr/local/go 是 1.22，而 x/crypto v0.46.0 要求 >= 1.24，
#      而且连不上 proxy.golang.org。模块缓存里已经有解压好的 go1.25.5，
#      于是直接用它并把网络关掉。
set -eu

MODCACHE=$(go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
TOOLCHAIN="$MODCACHE/golang.org/toolchain@v0.0.1-go1.25.5.linux-amd64/bin"

if [ -x "$TOOLCHAIN/go" ] && [ -d "$MODCACHE/golang.org/x/crypto@v0.46.0" ]; then
	GO="$TOOLCHAIN/go"
	export GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOPRIVATE='*'
	echo "（离线模式：用模块缓存里的 go1.25.5）"
else
	GO=${GO:-go}
fi

"$GO" mod tidy
"$GO" vet ./...
"$GO" build -trimpath -o csgw .
echo "编译完成：$(pwd)/csgw"
