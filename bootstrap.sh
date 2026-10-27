#!/usr/bin/env bash
#
# qqbot 一键部署入口。
#
# 把三件事收成一步：装 git → clone 仓库 → 跑 install.sh
#
#   curl -fsSL -o /tmp/qqbot-boot.sh \
#     https://raw.githubusercontent.com/caspian-vex/qqbot-dev/main/bootstrap.sh \
#     && sudo bash /tmp/qqbot-boot.sh
#
# 参数会原样传给 install.sh：
#   ... && sudo bash /tmp/qqbot-boot.sh --yes
#   ... && sudo bash /tmp/qqbot-boot.sh --panel-bind 127.0.0.1
#
# 可覆盖的环境变量：
#   QQBOT_SRC_DIR   源码克隆到哪，默认 $HOME/qqbot-dev
#   QQBOT_REPO      换仓库地址（自建镜像 / fork）
#   QQBOT_BRANCH    换分支，默认 main
#
# 为什么下载到文件再执行，而不是 curl | bash：
#   管道形式在下载失败时会让 bash 读到空脚本、静默退出 0，
#   用户以为装完了其实什么都没发生。先落盘，curl 失败就 && 短路。
set -euo pipefail

REPO="${QQBOT_REPO:-https://github.com/caspian-vex/qqbot-dev.git}"
BRANCH="${QQBOT_BRANCH:-main}"
SRC_DIR="${QQBOT_SRC_DIR:-${HOME:-/root}/qqbot-dev}"

# ---------- 输出 ----------
if [ -t 1 ]; then
  C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_END=$'\033[0m'
else
  C_OK=; C_WARN=; C_ERR=; C_DIM=; C_END=
fi
ok()   { echo "${C_OK}✓${C_END} $*"; }
warn() { echo "${C_WARN}!${C_END} $*"; }
die()  { echo "${C_ERR}✗${C_END} $*" >&2; exit 1; }
step() { echo; echo "${C_DIM}── $* ──${C_END}"; }

# ---------- 前置 ----------
# 不做「自动 sudo 重跑自己」，两个原因：
#   1. sudo 默认 env_reset，QQBOT_SRC_DIR 这类自定义变量会丢，用户看到的是「设了没生效」；
#      Ubuntu 默认 sudoers 又不允许 -E，显式 VAR=val 也可能被 SETENV 规则拒掉。
#   2. 管道形式下 $0 是 bash 而不是文件，压根没法重跑自己。
# 直接要求带 sudo，报错里把完整命令打出来，复制粘贴即可。
need_root() {
  [ "$(id -u)" -eq 0 ] && return 0
  die "需要 root 权限。请这样运行：sudo bash $0 $*"
}

# 不同发行版的包管理器差异只在这一处，install.sh 里不再重复处理
ensure_git() {
  step "git"

  if command -v git >/dev/null 2>&1; then
    ok "已安装：$(git --version)"
    return 0
  fi

  warn "未检测到 git，开始安装"
  if command -v apt-get >/dev/null 2>&1; then
    # -qq 压掉进度条；noninteractive 防止某些镜像上 tzdata 之类的交互卡住
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q git
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q git
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache git
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm git
  else
    die "没找到可用的包管理器，请手动安装 git 后重试"
  fi

  command -v git >/dev/null 2>&1 || die "git 安装失败，请手动安装后重试"
  ok "安装完成：$(git --version)"
}

# ---------- 源码 ----------
sync_repo() {
  step "获取源码"

  if [ -d "$SRC_DIR/.git" ]; then
    # 已经 clone 过：走增量拉取，这样重跑这条命令就等于升级
    ok "$SRC_DIR 已存在，拉取最新代码"
    # 不用 reset --hard：万一里面有人改过东西，不该被这条命令冲掉
    if ! git -C "$SRC_DIR" pull --ff-only -q 2>/dev/null; then
      warn "拉取失败（本地有改动或网络不通），继续用现有代码"
    fi
  elif [ -e "$SRC_DIR" ]; then
    die "$SRC_DIR 已存在但不是 git 仓库。删掉它，或用 QQBOT_SRC_DIR 换个目录"
  else
    echo "${C_DIM}clone $REPO（$BRANCH）→ $SRC_DIR${C_END}"
    # --depth=1：只要最新一次提交，clone 快很多，2G 机器上磁盘也省
    git clone --depth=1 --branch "$BRANCH" "$REPO" "$SRC_DIR" \
      || die "clone 失败，检查网络或换 QQBOT_REPO 指向镜像"
  fi

  ok "源码就位：$(git -C "$SRC_DIR" log --oneline -1)"
}

main() {
  echo
  echo "qqbot 一键部署"
  echo "源码目录 $SRC_DIR"

  need_root "$@"
  ensure_git
  sync_repo

  step "交给 install.sh"
  cd "$SRC_DIR"
  # 用 bash 显式调用而不是 ./install.sh：git 丢了执行位（Windows 上 clone、
  # 或从 zip 解压）时也能跑
  exec bash ./install.sh "$@"
}

main "$@"
