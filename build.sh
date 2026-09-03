#!/bin/sh
# 离线编译。
#
# 这台机器上 /usr/local/go 是 1.22，而 golang.org/x/crypto v0.46.0 的 go.mod 要求
# >= 1.24，正常情况下 go 会去下载工具链——但这里连不上 proxy.golang.org。
# 模块缓存里已经有解压好的 go1.25.5，直接用它，并且把网络访问关掉。
set -eu

MODCACHE=$(go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
TOOLCHAIN="$MODCACHE/golang.org/toolchain@v0.0.1-go1.25.5.linux-amd64/bin"
if [ -x "$TOOLCHAIN/go" ]; then
	GO="$TOOLCHAIN/go"
else
	GO=go   # 有网络 / 本机 go 够新时的正常路径
fi

export GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOPRIVATE='*'

"$GO" mod tidy
"$GO" vet ./...
"$GO" build -trimpath -o csgw .
echo "编译完成：$(pwd)/csgw"
