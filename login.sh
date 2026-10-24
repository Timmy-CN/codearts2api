#!/usr/bin/env bash
# login.sh — CodeArts Agent（华为云账号）登录：浏览器 OAuth2 + PKCE → auths/codearts-{user_id}.json
#
# 用法（-print-only 与 -auth-dir 可任意顺序组合）:
#   ./login.sh                          本机有浏览器：自动打开浏览器，回调 + ticket 轮询双通道
#   ./login.sh -print-only              服务器：打印登录链接，任意机器浏览器打开，ticket 轮询下发
#   ./login.sh -auth-dir <dir>          指定凭证输出目录（默认 ./auths）
set -euo pipefail
cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
    echo "错误：未找到 go 命令，请先安装 Go（https://go.dev/dl/）" >&2
    exit 1
fi

AUTH_DIR="./auths"
EXTRA=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -print-only)
            EXTRA+=("-print-only")
            ;;
        -auth-dir)
            [[ $# -ge 2 && -n "${2:-}" && "${2:0:1}" != "-" ]] || {
                echo "错误：-auth-dir 需要一个目录参数（用法：-auth-dir <dir>）" >&2
                exit 1
            }
            AUTH_DIR="$2"
            EXTRA+=("-auth-dir" "$AUTH_DIR")
            shift
            ;;
        *)
            echo "错误：未知参数 $1（支持 -print-only 与 -auth-dir <dir>）" >&2
            exit 1
            ;;
    esac
    shift
done

mkdir -p "$AUTH_DIR"

# 每次都重建：go build 有缓存，代价极小；只在二进制缺失时才建会跑旧代码。
BIN=./bin/codearts2api-login
echo "build login binary ..."
mkdir -p ./bin
go build -o "$BIN" ./cmd/login

exec "$BIN" "${EXTRA[@]}"
