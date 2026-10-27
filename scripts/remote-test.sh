#!/usr/bin/env bash
# 把源码同步到服务器并在那边跑测试。
#
# 为什么不在本地跑：本地是 Windows，没有 gcc，`go test -race` 直接不可用；
# 而竞态检测恰恰是这类并发代码最该跑的一项。
#
# 用法：
#   export QQBOT_SSH_HOST=root@1.2.3.4
#   export QQBOT_SSH_KEY=~/.ssh/id_ed25519       # 可选，默认就是这个
#   ./scripts/remote-test.sh                     # 全部
#   ./scripts/remote-test.sh quick               # 跳过 -race
#
# 只传源码：bin/dist/.git/数据目录都在排除列表里，
# 服务器上不需要它们，传了只是浪费带宽和磁盘。
set -euo pipefail

HOST="${QQBOT_SSH_HOST:-}"
KEY="${QQBOT_SSH_KEY:-$HOME/.ssh/id_ed25519}"
REMOTE_DIR="${QQBOT_REMOTE_DIR:-/root/qqbot-src}"
MODE="${1:-full}"

if [ -z "$HOST" ]; then
  echo "缺少 QQBOT_SSH_HOST，例如：export QQBOT_SSH_HOST=root@1.2.3.4" >&2
  exit 1
fi

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR -o ConnectTimeout=15)
[ -f "$KEY" ] && SSH_OPTS+=(-i "$KEY")

cd "$(dirname "$0")/.."

echo "=== 同步到 $HOST:$REMOTE_DIR ==="
tar czf - \
  --exclude=./.git \
  --exclude=./bin \
  --exclude=./dist \
  --exclude=./.devroot \
  --exclude='*.exe' \
  --exclude='*.db' --exclude='*.db-shm' --exclude='*.db-wal' \
  . | ssh "${SSH_OPTS[@]}" "$HOST" \
    "rm -rf '$REMOTE_DIR' && mkdir -p '$REMOTE_DIR' && tar xzf - -C '$REMOTE_DIR'"

echo "=== 在服务器上跑测试 ==="
ssh "${SSH_OPTS[@]}" "$HOST" "cd '$REMOTE_DIR' && chmod +x scripts/run-tests.sh && ./scripts/run-tests.sh '$MODE'"
