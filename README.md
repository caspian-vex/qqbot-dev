# qqbot

一个只为「在 QQ 上跟大模型聊天」而存在的机器人。单二进制、单容器、自带面板。

替代 AstrBot + SnowLuma 的组合，面向 1 核 2G 的小机器：两个容器合计内存上限 1.25G，
其中机器人本体常驻不到 100MB。

---

## 它是什么

```
QQ ←→ NapCat（协议端） ←WebSocket→ qqbot（判断 + 调模型 + 回复 + 面板）
```

- **NapCat** 负责登录 QQ、收发消息，通过 OneBot v11 正向 WebSocket 暴露接口。
- **qqbot** 连上去，判断这条消息要不要回、该带哪些上下文、然后调模型、把结果发回去。
- 面板是 qqbot 自己提供的，不是另起的服务，也不是反代别人的面板。

## 它不是什么

不处理图片/语音/视频，没有插件系统，不支持多平台，不做群管理。
这些是刻意的取舍，理由写在 [`docs/00-决策记录.md`](docs/00-决策记录.md)。

---

## 前置条件

| 项 | 要求 | 怎么查 |
| --- | --- | --- |
| 架构 | x86_64 / arm64 | `uname -m` |
| 内存 | ≥ 1.8 GB（2G 是设计基准） | `free -m` |
| 磁盘 | ≥ 8 GB 可用 | `df -h /` |
| 系统 | Debian 12+ / Ubuntu 22.04+（能跑 `get.docker.com`） | `cat /etc/os-release` |
| 权限 | root | `id -u` 输出 0 |
| 网络 | 能访问 github.com、docker hub、api.deepseek.com | — |

内存不足 1.8G 时安装脚本会警告并询问是否继续。低于 1.5G 基本不要试，
构建镜像阶段会被 OOM 杀掉。

**建议先加 2G swap**（1 核 2G 机器构建镜像时的兜底）：

```bash
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

---

## 安装

一条命令。先装 git（最小化 Debian/Ubuntu 没带），然后拉代码、跑安装脚本：

```bash
apt-get update && apt-get install -y git
git clone https://github.com/caspian-vex/qqbot-dev.git
cd qqbot-dev && sudo ./install.sh
```

安装脚本会依次做：

1. 检查架构、内存、磁盘、swap
2. 检查 Docker，没有就问你要不要装
3. 生成密钥（`etc/env`，0600）与引导配置
4. **下载官方预编译二进制并校验 sha256**；下不到就回退到服务器本地构建
5. 组装镜像、启动容器、等面板健康
6. 把 `qqbot` 运维命令装到 `/usr/local/bin`

装完会打印面板地址、初始密码、NapCat WebUI Token，以及下一步要做什么。

**首次安装约 3.5 分钟**，绝大部分时间花在拉 NapCat 镜像（2.16G）上。
第 4 步是刻意设计的：1 核 2G 上跑 `go build` 要 90 秒且内存接近上限，
而 CI 已经用同样的参数把二进制编好了，下载只要几秒。两条路径的产物等价，
装完会打印 `镜像来源` 告诉你是哪条。

常用变体：

```bash
sudo ./install.sh --yes                    # 全程不询问
sudo ./install.sh --panel-bind 127.0.0.1   # 面板只允许本机访问
sudo ./install.sh --dir /srv/qqbot         # 换安装目录
sudo ./install.sh --from-source            # 跳过下载，强制在服务器上编译
```

> 默认面板绑 `0.0.0.0`，装完直接访问 `http://<服务器IP>:8080`，不需要 SSH 隧道。
> 想只允许本机就加 `--panel-bind 127.0.0.1`。

**下载慢 / 下不动**（GitHub 在国内不稳定）：把下载地址指向镜像站

```bash
sudo QQBOT_BINARY_BASE=https://mirror.example.com/qqbot ./install.sh
```

> 这台机器上还要装 1Panel 的话，**先装 1Panel 再装本项目**。1Panel 会把 Docker 一起装好，
> 安装脚本检测到就直接复用，不会重复安装也不会互相抢。

---

## 上线：放行端口 → 扫码 → 填 Key

**先放行端口**。云服务器的安全组默认是关的，不放行就是「装完打不开」：

| 端口 | 用途 | 是否必须 |
| --- | --- | --- |
| 8080 | 面板 | 是 |
| 6099 | NapCat WebUI（扫码用） | 是 |
| 3001 | OneBot WS | **不要放行** |

在云厂商控制台加两条 TCP 入站规则；机器上装了 ufw 的话：

```bash
ufw allow 8080/tcp && ufw allow 6099/tcp
```

> 3001 不能放行。它默认没有 Token，一旦公网可达，任何人连上来就能冒充机器人发消息。
> 这个端口写死绑 `127.0.0.1`，不要改。

然后：

1. 打开 `http://<服务器IP>:6099/webui`，Token 用安装结束打印的那个，扫码登录 QQ
2. 打开 `http://<服务器IP>:8080`，`admin` + 安装时打印的密码登录，在「设置」里填 DeepSeek 的 API Key

扫码成功后 qqbot 会在 5 秒内自动连上，`qqbot logs -f` 能看到「已登录」。
随时用 `qqbot url` 找回这些地址。

### 设置页填什么

| 项 | 值 |
| --- | --- |
| 模型地址 | `https://api.deepseek.com`（默认已填） |
| 模型名称 | `deepseek-flash`（默认已填） |
| API Key | 你的 DeepSeek Key |
| 思考模式 | 默认关闭。开启后温度参数不生效（DeepSeek 的限制） |

保存后去「人格」页确认当前人格是 `default`。然后在群里 @ 机器人说一句话，
或者用面板「测试发送」直接发一条，验证链路。

> **注意**：面板走 HTTP，密码是明文传输。介意就自己挂一层 HTTPS 反代。
> 登录后建议在「账号」页开启 TOTP。连续登录失败 10 次会锁 15 分钟。
> **面板密码只在安装结束时打印一次**，忘了用 `qqbot reset-password`。

---

## 日常运维

```
qqbot status                  容器与面板状态、资源占用
qqbot url                     面板与 WebUI 的访问地址
qqbot reset-password          重置面板密码（忘了密码时用，数据不动）
qqbot logs -f                 跟踪日志（qqbot logs napcat 看协议端）
qqbot restart                 重启
qqbot upgrade [--src <目录>]  重建镜像并切过去，失败自动回滚
qqbot rollback <版本>         回滚到指定镜像版本
qqbot backup                  备份数据与密钥（先停容器保证一致性，只留最近 7 份）
qqbot restore <文件>          从备份恢复
qqbot tunnel                  面板被改回只绑 127.0.0.1 时的备用访问方式
qqbot uninstall [--purge]     卸载（--purge 同时删数据）
```

| 事项 | 命令 / 位置 |
| --- | --- |
| 看资源占用 | `qqbot status` |
| 定期备份 | `qqbot backup`（只留最近 7 份） |
| 备份挪到别处 | 从 `/opt/qqbot/backup/` 拉走 |
| 改监听地址 | 改 `/opt/qqbot/etc/env` 的 `PANEL_BIND`，然后 `qqbot restart` |
| 改协议端地址 | 改 `/opt/qqbot/etc/qqbot.toml`，然后 `qqbot restart` |
| 改模型 / 人格 / 触发规则 | 面板里改，立即生效，不用重启 |

安装目录在 `/opt/qqbot`：`data/` 和 `etc/` 不能删，`src/` 可以随时重建。

### 升级

```bash
cd <你 clone 下来的源码目录> && git pull && sudo ./install.sh --yes
```

重跑安装脚本会重新同步源码、重新下载最新预编译二进制；密钥与数据都不受影响。

> `qqbot upgrade` 走的是源码构建（约 2 分钟），因为它要编译你 `src/` 里的新代码；
> 想走预编译的快速路径（几十秒），用上面这条 `install.sh`。

---

## 从旧机器迁移

**顺序很重要**：先在旧机器备份，再在新机器装好，最后恢复。

### 1. 旧机器上备份

```bash
qqbot backup
# → /opt/qqbot/backup/qqbot-20261027-095142.tar.gz
```

包里有 `data/qqbot/qqbot.db`（会话、用量、设置）、`etc/qqbot.toml`、`etc/env`（密钥）。

**`etc/env` 里的 `QQBOT_SECRET_KEY` 必须一起搬。** 它是加密 LLM API Key 的主密钥，
丢了就等于所有 Key 都要重新录（数据库里的密文再也解不开）。

NapCat 的 QQ 登录态**不在**备份里。想免扫码就单独搬：

```bash
tar czf /root/napcat-state.tar.gz -C /opt/qqbot data/napcat
```

不搬也行，新机器扫码重登一次更干净。

### 2. 新机器上装好后恢复

先按「安装」一节装完（**不要扫码、不要配任何东西**），然后：

```bash
scp root@<旧IP>:/opt/qqbot/backup/qqbot-*.tar.gz /root/
qqbot restore /root/qqbot-20261027-095142.tar.gz
```

`restore` 会：停容器 → 解包覆盖 `data/` 与 `etc/` → 重新对齐文件属主 → 启动。
恢复后**面板密码和 API Key 都沿用旧的**，不需要重新配。

同时搬了 NapCat 登录态的话：

```bash
scp root@<旧IP>:/root/napcat-state.tar.gz /root/
qqbot down
tar xzf /root/napcat-state.tar.gz -C /opt/qqbot
chown -R 1000:1000 /opt/qqbot/data/napcat
qqbot up
```

### 3. 检查

```bash
qqbot status          # 两个容器 Up，面板健康
qqbot logs -f         # 看到「已登录」
```

确认没问题再停旧机器，**别提前关**——万一新机器有问题还能退回去。

---

## 常见问题

### 面板打不开

按这个顺序查，多数情况是第一条：

```bash
curl -s localhost:8080/healthz     # 服务端自己通不通
```

- 本机通、外面不通 → **安全组 / 防火墙没放行 8080**
- 本机也不通 → `qqbot logs qqbot` 看退出原因
- 容器活着但端口没监听 → `docker ps` 看 PORTS 列，应该是 `0.0.0.0:8080->8080/tcp`，
  而不是 `127.0.0.1:8080`

改绑定地址（两种都只改 `etc/env` 并重建容器，数据不动）：

```bash
sudo ./install.sh --panel-bind 127.0.0.1   # 只允许本机
sudo ./install.sh --panel-bind 0.0.0.0     # 改回公网可达
```

### 面板密码没记下来

```bash
qqbot reset-password
```

输出新密码，会话、配置、人格、API Key 都不动。
默认会**同时关闭 TOTP**（走到这一步通常已经进不去了，留着 TOTP 等于换完还是登不进）；
想保留就加 `--keep-totp`。

### 机器人不回消息

按顺序排查：

1. `qqbot logs -f` 有没有「已登录」
2. 没登录 → NapCat WebUI 扫码
3. 登录了但 `连接断开` → NapCat 的 WebSocket 服务端没开，或 Token 不一致
4. 连上了但不回 → 面板「概览」看最后事件时间；群聊要 @ 才触发
5. 触发了但没回复 → 看日志里的模型错误，多半是 API Key 或余额

> NapCat「网络配置」里那条 WebSocket 服务端应该是端口 `3001`、**Token 留空**。
> 填了值反而会让 qqbot 握手 401。

### 想确认 3001 没有暴露

```bash
docker ps --format '{{.Names}}\t{{.Ports}}' | grep 3001
# 必须是 127.0.0.1:3001->3001/tcp，出现 0.0.0.0 就说明被人改过

curl --noproxy '*' -m 5 http://<服务器IP>:3001/
# 期望：连接失败。返回任何 HTTP 状态码都说明端口真的暴露了，立刻改回去。
```

### 安装时走了源码构建，明明有预编译产物

安装日志里那一段会打印原因：

| 提示 | 原因 | 处理 |
| --- | --- | --- |
| `下载失败（网络不通…）` | GitHub 连不上，或这个版本还没发 Release | 换 `QQBOT_BINARY_BASE` 指向镜像站 |
| `校验文件 checksums.txt 下载失败` | Release 的 assets 不齐 | 三个文件都要有 |
| `sha256 不匹配` | 镜像站放的是别的项目产物，或文件被截断 | 确认 `QQBOT_BINARY_BASE` 指向本项目 |

想直接跳过下载、不浪费这几秒：`sudo ./install.sh --yes --from-source`。

### 构建时被 OOM 杀掉

1 核 2G 构建镜像内存会顶到上限。先加 swap（见「前置条件」），再 `qqbot upgrade`。
每次都失败就改用预编译路径，服务器上完全不跑编译器。

### 换镜像 tag / 回滚

```bash
qqbot rollback 20261027-0941            # 换回旧版本镜像
qqbot upgrade --src /root/qqbot-dev     # 从新源码重建
```

---

## 卸载

```bash
qqbot uninstall            # 停容器、删命令，数据留在 /opt/qqbot/data
qqbot uninstall --purge    # 连数据一起删（不可恢复）
```

---

## 文档

| 文件 | 内容 |
| --- | --- |
| `docs/00-决策记录.md` | 15 条 ADR：为什么这么选，否掉了什么 |
| `docs/01-需求与范围.md` | 需求展开、硬约束、明确不做什么 |
| `docs/05-验收.md` | 验收清单与实测数据 |
| `docs/开发手册.md` | 给维护者：代码结构、本地开发、构建发布、排障 |

## 技术栈

Go（标准库为主）+ `modernc.org/sqlite`（纯 Go，无 CGO）+ `BurntSushi/toml` + `gorilla/websocket`。
前端原生 JS，手写、无构建步骤。镜像 `alpine` + 静态二进制，最终约 20MB。
