#!/usr/bin/env bash
# 在服务器上跑完整测试（build / vet / test / test-race）。
#
# 本地 Windows 没有 gcc，`-race` 跑不了；服务器是 Debian，装上 gcc 就能跑。
# 竞态检测对这类"多 goroutine + 共享状态"的服务端代码价值很高，
# 所以这条链路应该常态化，而不是偶尔手工跑一次。
#
# 用法（在服务器上直接执行，或由 scripts/remote-test.sh 远程调用）：
#   ./scripts/run-tests.sh            # 全部跑
#   ./scripts/run-tests.sh quick      # 跳过 -race
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

MODE="${1:-full}"

# 国内直连 proxy.golang.org 基本不通，用镜像
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"
# -race 需要 cgo，必须显式开启（项目默认按 CGO_ENABLED=0 交付二进制）
export CGO_ENABLED=1

rc_total=0

step() {
  local name="$1"
  shift
  echo
  echo "=== $name ==="
  local start
  start=$(date +%s)
  "$@"
  local rc=$?
  local cost=$(( $(date +%s) - start ))
  echo "--- $name rc=$rc (${cost}s)"
  [ $rc -ne 0 ] && rc_total=1
  return 0
}

echo "=== 环境 ==="
go version
gcc --version 2>/dev/null | head -1 || echo "gcc: 缺失（-race 将不可用）"
echo "cpu=$(nproc)  mem=$(free -m | awk '/^Mem:/{print $2"MB"}')"

step "go build" go build ./...
step "go vet"   go vet ./...
step "go test"  go test ./... -count=1

if [ "$MODE" != "quick" ]; then
  if command -v gcc >/dev/null 2>&1; then
    step "go test -race" go test -race ./... -count=1
  else
    echo
    echo "跳过 -race：没有 gcc"
  fi
fi

echo
if [ $rc_total -eq 0 ]; then
  echo "全部通过"
else
  echo "有步骤失败，见上面的 rc"
fi
exit $rc_total
