#!/usr/bin/env bash
#
# qqbot 一键安装。
#
#   sudo ./install.sh                        源码目录里直接跑
#   sudo ./install.sh --yes                  不询问，全部用默认值
#   sudo ./install.sh --panel-bind 127.0.0.1 面板只允许本机访问
#   sudo ./install.sh --dir /srv/qqbot       换安装目录
#
# 只做三件事：准备 Docker → 生成配置与密钥 → 构建并启动。
# 装完直接访问 http://<服务器IP>:8080，不需要 SSH 隧道。
# 域名、证书、反向代理不在本项目范围内。
set -euo pipefail

DIR="${QQBOT_DIR:-/opt/qqbot}"
PANEL_BIND_SET=0
if [ -n "${PANEL_BIND:-}" ]; then
  PANEL_BIND_SET=1
fi
PANEL_BIND="${PANEL_BIND:-0.0.0.0}"
ASSUME_YES=0
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

while [ $# -gt 0 ]; do
  case "$1" in
    --yes|-y)      ASSUME_YES=1; shift ;;
    --dir)         DIR="$2"; shift 2 ;;
    --panel-bind)  PANEL_BIND="$2"; PANEL_BIND_SET=1; shift 2 ;;
    -h|--help)     sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

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

# 询问一律走 /dev/tty。
# 为什么：`curl ... | bash` 时 stdin 是脚本本身，read 会读到脚本内容而不是键盘。
ask() {
  local prompt="$1" default="${2:-n}" reply
  if [ "$ASSUME_YES" = 1 ] || [ ! -r /dev/tty ]; then
    [ "$default" = "y" ]
    return
  fi
  printf '%s [%s] ' "$prompt" "$([ "$default" = y ] && echo Y/n || echo y/N)" > /dev/tty
  read -r reply < /dev/tty || reply=""
  reply="${reply:-$default}"
  case "$reply" in [yY]*) return 0 ;; *) return 1 ;; esac
}

# 最小化 Debian 上可能既没有 curl 也没有 wget，两个都试。
fetch() {
  local url="$1" out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$out" "$url"
  else
    return 1
  fi
}

http_ok() {
  local url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS -m 2 "$url" >/dev/null 2>&1
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 2 -O /dev/null "$url" >/dev/null 2>&1
  else
    return 1
  fi
}

http_get() {
  local url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS -m 4 "$url" 2>/dev/null
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 4 -O - "$url" 2>/dev/null
  fi
}

# 猜这台机器的公网 IP，只用于在安装结束的提示里拼出可点的地址。
# 云服务器的公网 IP 不挂在网卡上（网卡是 10.x / 172.x 内网地址），
# 所以只能问外部服务；三个都试，谁先回用谁，全失败就退回网卡地址。
public_ip() {
  local ip
  for u in https://api.ipify.org https://ifconfig.me/ip https://ipinfo.io/ip; do
    ip=$(http_get "$u" | tr -d '[:space:]')
    case "$ip" in
      ""|*[!0-9.]*) continue ;;
    esac
    echo "$ip"
    return 0
  done
  ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  echo "${ip:-<服务器IP>}"
}

# ---------- 预检 ----------
preflight() {
  step "环境检查"

  [ "$(id -u)" -eq 0 ] || die "需要 root 权限（sudo ./install.sh）"

  case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "不支持的架构: $(uname -m)" ;;
  esac
  ok "架构 $ARCH"

  local mem_mb
  mem_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
  if [ "$mem_mb" -lt 1800 ]; then
    warn "内存仅 ${mem_mb}MB。本项目按 2G 设计，低于 1.8G 可能被 OOM 打断"
    ask "仍要继续？" n || die "已取消"
  else
    ok "内存 ${mem_mb}MB"
  fi

  mkdir -p "$DIR"
  local avail_gb
  avail_gb=$(df -Pk "$DIR" | awk 'NR==2{print int($4/1024/1024)}')
  [ "$avail_gb" -ge 8 ] || die "磁盘不足：${avail_gb}GB 可用，至少需要 8GB"
  ok "磁盘剩余 ${avail_gb}GB"

  # swap 在 2G 机器上是防 OOM 的兜底，没有就提示
  if [ "$(awk '/SwapTotal/{print $2}' /proc/meminfo)" -eq 0 ]; then
    warn "未启用 swap。建议加 2G swapfile 兜底，否则构建镜像时容易 OOM"
  else
    ok "swap 已启用"
  fi
}

# ---------- Docker ----------
ensure_docker() {
  step "Docker"

  if command -v docker >/dev/null 2>&1; then
    ok "docker 已安装：$(docker --version)"
  else
    warn "未检测到 docker"
    if ask "现在安装 Docker（官方脚本，含 compose 插件）？" y; then
      echo "${C_DIM}下载安装脚本…${C_END}"
      fetch https://get.docker.com /tmp/get-docker.sh \
        || die "下载失败（curl/wget 都没有或网络不通），请手动安装 Docker 后重试"
      sh /tmp/get-docker.sh || die "Docker 安装失败，请手动安装后重试"
      rm -f /tmp/get-docker.sh
      ok "docker 安装完成"
    else
      die "需要 Docker 才能继续"
    fi
  fi

  systemctl enable --now docker >/dev/null 2>&1 || true
  docker info >/dev/null 2>&1 || die "docker 守护进程未运行，试试 systemctl start docker"

  docker compose version >/dev/null 2>&1 \
    || die "缺少 docker compose v2 插件（docker-compose-plugin）"
  ok "compose $(docker compose version --short 2>/dev/null || echo v2)"
}

# ---------- 密钥 ----------
rand_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
  else
    head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

env_get() { grep "^$1=" "$DIR/etc/env" 2>/dev/null | cut -d= -f2- || true; }

# 幂等写入：文件已有该键就保留原值，避免重装时把正在用的密钥换掉。
ensure_kv() {
  local file="$1" key="$2" value="$3"
  if [ -f "$file" ] && grep -q "^${key}=" "$file" 2>/dev/null; then
    return
  fi
  echo "${key}=${value}" >> "$file"
}

# 强制覆盖：用于每次构建都应该刷新的项（版本号、构建时间）。
env_set() {
  local file="$1" key="$2" value="$3"
  if [ -f "$file" ] && grep -q "^${key}=" "$file" 2>/dev/null; then
    sed -i "s|^${key}=.*|${key}=${value}|" "$file"
  else
    echo "${key}=${value}" >> "$file"
  fi
}

gen_config() {
  step "配置与密钥"

  mkdir -p "$DIR"/{etc,src,data/qqbot} \
           "$DIR"/data/napcat/{qq,config,plugins,logs}

  local env_file="$DIR/etc/env"
  if [ -f "$env_file" ]; then
    ok "沿用已有密钥文件（不重新生成）"
  else
    umask 077
    : > "$env_file"
    ok "生成密钥文件"
  fi

  ensure_kv "$env_file" QQBOT_SECRET_KEY   "$(rand_hex 32)"
  ensure_kv "$env_file" WEBUI_TOKEN        "$(rand_hex 16)"
  ensure_kv "$env_file" NAPCAT_TAG         "latest"
  ensure_kv "$env_file" PANEL_BIND         "0.0.0.0"
  ensure_kv "$env_file" NAPCAT_WEBUI_BIND  "0.0.0.0"
  ensure_kv "$env_file" GOPROXY            "https://goproxy.cn,direct"
  ensure_kv "$env_file" QQBOT_VERSION      "$(date +%Y%m%d-%H%M)"

  # 显式指定过绑定地址就覆盖，否则保留原值（重装不该悄悄改掉监听范围）
  if [ "$PANEL_BIND_SET" = 1 ]; then
    env_set "$env_file" PANEL_BIND "$PANEL_BIND"
  fi

  # 这两个描述「本次要构建的源码」，每次安装都刷新
  env_set "$env_file" QQBOT_COMMIT \
    "$(git -C "$SRC_DIR" rev-parse --short HEAD 2>/dev/null || echo none)"
  env_set "$env_file" QQBOT_BUILD_DATE "$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  chmod 600 "$env_file"
  ok "密钥文件 $env_file（0600）"

  # 目录属主必须与容器内运行用户一致，否则 SQLite 建不了库、NapCat 写不进配置。
  # 10001 = Dockerfile 里 qqbot 用户的 uid；1000 = NapCat 官方默认 uid。
  chown -R 10001:10001 "$DIR/data/qqbot"
  chown -R 1000:1000   "$DIR/data/napcat"
  ok "数据目录属主已对齐（qqbot 10001 / napcat 1000）"

  # 引导配置：已存在就不覆盖，避免冲掉用户改过的监听地址
  if [ -f "$DIR/etc/qqbot.toml" ]; then
    ok "沿用已有 $DIR/etc/qqbot.toml"
  else
    install -m 644 "$SRC_DIR/deploy/qqbot.toml" "$DIR/etc/qqbot.toml"
    ok "生成 $DIR/etc/qqbot.toml"
  fi

  install -m 644 "$SRC_DIR/deploy/compose.yaml" "$DIR/compose.yaml"
  ok "写入 compose.yaml"
}

# ---------- 源码 ----------
install_src() {
  step "同步源码"

  [ -f "$SRC_DIR/go.mod" ] || die "当前目录不像项目根目录（找不到 go.mod）"

  # 用 tar 同步而不是 cp -r：能一次性排除产物与本地密钥目录。
  # 注意 --exclude=./dist 的 ./ 前缀：写成 --exclude=dist 会连 web/dist 一起排掉，
  # 而 web/dist 是前端源码，被 embed 进二进制，缺了编译直接失败。
  tar czf - -C "$SRC_DIR" \
      --exclude=./.git --exclude=./bin --exclude=./dist \
      --exclude=./.devroot --exclude=./.workbuddy-ai \
      --exclude='*.exe' --exclude='*.db' --exclude='*.db-shm' --exclude='*.db-wal' \
      . | tar xzf - -C "$DIR/src"

  ok "已同步到 $DIR/src"
}

# ---------- 构建与启动 ----------
build_and_up() {
  step "构建镜像"

  local env_file="$DIR/etc/env"
  local mem_mb
  mem_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
  if [ "$mem_mb" -lt 2048 ]; then
    warn "首次构建约 2-5 分钟（要下 Go 镜像和依赖），期间内存会接近上限，请勿中断"
  fi

  cd "$DIR"
  docker compose --env-file "$env_file" build
  ok "镜像构建完成"

  step "启动"
  docker compose --env-file "$env_file" up -d

  echo -n "等待面板就绪"
  local i running
  for i in $(seq 1 60); do
    if http_ok "http://127.0.0.1:8080/healthz"; then
      echo; ok "面板已就绪"; return 0
    fi
    # 容器已经退出就别再等了，直接把原因打出来
    running=$(docker inspect -f '{{.State.Running}}' qqbot-core 2>/dev/null || echo false)
    if [ "$running" != "true" ]; then
      echo; warn "qqbot 容器已退出，最近日志："
      docker compose --env-file "$env_file" logs --tail=40 qqbot || true
      return 1
    fi
    echo -n "."
    sleep 2
  done
  echo
  warn "面板 120 秒内未就绪，请查看：cd $DIR && docker compose logs qqbot"
  return 1
}

# ---------- 收尾 ----------
print_next() {
  local env_file="$DIR/etc/env"
  local webui_token panel_bind pw ip

  webui_token=$(env_get WEBUI_TOKEN)
  panel_bind=$(env_get PANEL_BIND)
  [ -n "$panel_bind" ] || panel_bind="0.0.0.0"

  pw=$(cd "$DIR" && docker compose --env-file "$env_file" logs qqbot 2>/dev/null \
        | grep -A3 '面板初始凭据' | grep '密码' | head -1 | sed 's/.*: *//' || true)

  ip=$(public_ip)

  local panel_url webui_url
  case "$panel_bind" in
    127.0.0.1)
      panel_url="http://127.0.0.1:8080（仅本机）"
      webui_url="http://127.0.0.1:6099/webui（仅本机）"
      ;;
    0.0.0.0)
      panel_url="http://${ip}:8080"
      webui_url="http://${ip}:6099/webui"
      ;;
    *)
      panel_url="http://${panel_bind}:8080"
      webui_url="http://${ip}:6099/webui"
      ;;
  esac

  cat <<EOF

${C_OK}========== 安装完成 ==========${C_END}

安装目录     $DIR
面板地址     $panel_url
面板账号     admin / ${pw:-见 qqbot logs}
NapCat WebUI $webui_url
WebUI Token  ${webui_token}

${C_WARN}打不开就先查安全组${C_END}
云服务器的安全组 / 防火墙默认是关着的，要放行两个 TCP 端口：
   8080   面板
   6099   NapCat WebUI（扫码用）
Debian/Ubuntu 装了 ufw 的话：ufw allow 8080/tcp && ufw allow 6099/tcp

${C_OK}下一步${C_END}

1) 打开 $webui_url
   Token 填上面的值，用要当机器人的 QQ 扫码

2) 扫码成功后 qqbot 会在 5 秒内自动连上，验证：
     qqbot logs -f          看到「已登录」即成功

3) 打开 $panel_url 登录面板
   在「设置」里填 DeepSeek API Key，然后去群里 @ 一下试试

${C_WARN}安全提醒（面板现在公网可达）${C_END}
- 面板是 HTTP，密码在网络上是明文传输的。介意的话自己挂一层 HTTPS 反代。
- 登录后建议立刻在「账号」页开启 TOTP 二次验证。
- OneBot 的 3001 端口仍然只绑 127.0.0.1，这是刻意保留的：
  它默认没有 Token，一旦暴露公网，任何人连上来就能冒充你的机器人发消息。

${C_DIM}常用命令（已安装到 /usr/local/bin/qqbot）${C_END}
   qqbot status      容器与面板状态、资源占用
   qqbot logs -f     跟踪日志
   qqbot restart     重启
   qqbot backup      备份数据与密钥
   qqbot uninstall   卸载（默认保留数据）

EOF
}

# ---------- 运维命令 ----------
install_cli() {
  install -m 755 "$SRC_DIR/deploy/qqbotctl" /usr/local/bin/qqbot
  ok "已安装 qqbot 命令"
}

main() {
  echo
  echo "qqbot 安装程序"
  echo "目标目录 $DIR"
  preflight
  ensure_docker
  gen_config
  install_src
  build_and_up
  install_cli
  print_next
}

main "$@"
