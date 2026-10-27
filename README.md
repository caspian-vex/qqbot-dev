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

## 安装

服务器上执行（Debian / Ubuntu，root）：

```bash
git clone <本仓库> qqbot-dev && cd qqbot-dev && sudo ./install.sh
```

安装脚本会依次做：

1. 检查架构、内存、磁盘、swap
2. 检查 Docker，没有就问你要不要装
3. 生成密钥（`etc/env`，0600）与引导配置
4. **下载官方预编译二进制并校验 sha256**；下不到就回退到服务器本地构建
5. 组装镜像、启动容器、等面板健康
6. 把 `qqbot` 运维命令装到 `/usr/local/bin`

装完会打印面板地址、初始密码、NapCat WebUI Token，以及下一步要做什么。

首次安装约 **3.5 分钟**，绝大部分时间花在拉 NapCat 镜像（2.16G）上。
第 4 步是刻意设计的：1 核 2G 上跑 `go build` 要 90 秒且内存接近上限，
而 CI 已经用同样的参数把二进制编好了，下载只要几秒。
两条路径的产物等价，装完会打印 `镜像来源` 告诉你是哪条。

**非交互安装**：`sudo ./install.sh --yes`

**只允许本机访问**：`sudo ./install.sh --panel-bind 127.0.0.1`

**强制在服务器上编译**：`sudo ./install.sh --from-source`

**下载慢 / 下不动**（GitHub 在国内不稳定）：把下载地址指向镜像站

```bash
sudo QQBOT_BINARY_BASE=https://mirror.example.com/qqbot ./install.sh
```

### 装完之后必须做的两件事

**先放行端口**。云服务器的安全组默认是关的，不放行就是「装完打不开」：

```
8080   面板
6099   NapCat WebUI（扫码用）
```

然后：

1. 打开 `http://<服务器IP>:6099/webui`，Token 用安装结束打印的那个，扫码登录 QQ
2. 打开 `http://<服务器IP>:8080`，`admin` + 安装时打印的密码登录，在「设置」里填 DeepSeek 的 API Key

扫码成功后 qqbot 会在 5 秒内自动连上，`qqbot logs -f` 能看到「已登录」。
随时用 `qqbot url` 找回这些地址。

> **注意**：面板走 HTTP，密码是明文传输。介意就自己挂一层 HTTPS 反代。
> 登录后建议在「账号」页开启 TOTP。
> OneBot 的 3001 端口始终只绑 `127.0.0.1`——它默认没有 Token，不能暴露公网。

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

安装目录布局见 ADR-014。简单说：`data/` 和 `etc/` 不能删，`src/` 可以随时重建。

---

## 本地开发

不需要 Docker，用内置的 mock 协议端和 mock 模型端跑通整条链路。

```bash
go build ./...
go test ./...                  # 含 20+ 个端到端用例

# 起一套假环境：3001 假协议端 / 3010 假模型 / 3020 操作面板
make mock

# 另开一个终端，让机器人连上假的协议端
QQBOT_ROOT=/tmp/qqbot-dev go run ./cmd/qqbot run
```

`http://127.0.0.1:3020` 是一个自包含的调试页，可以点按钮造一条群消息、
断开连接、让模型返回 500、清空记录——不用真的登 QQ 就能验证各种边界情况。

数据落在 `$QQBOT_ROOT/data/qqbot/qqbot.db`，删掉就是干净环境。

### 测试

```bash
make test          # 普通测试
make test-race     # 需要 gcc；Windows 上没有就报错
make test-remote   # 同步到服务器跑 -race（本地没 gcc 时用这个）
```

`test-remote` 需要环境变量 `QQBOT_SSH_HOST`、`QQBOT_SSH_KEY`，见 `scripts/remote-test.sh`。

### 前端

`web/dist/` 是手写的静态文件，没有构建步骤，改完刷新浏览器即可。
它们被 `//go:embed` 打进二进制，所以**必须提交进 git**（见 ADR-003 的踩坑记录）。

---

## 文档

| 文件 | 内容 |
| --- | --- |
| `docs/00-决策记录.md` | 14 条 ADR：为什么这么选，否掉了什么 |
| `docs/01-需求与范围.md` | 需求展开、硬约束、明确不做什么 |
| `docs/02-部署手册.md` | 换新服务器怎么装、怎么迁移、常见故障 |
| `docs/05-验收.md` | 验收清单与实测数据 |

## 技术栈

Go（标准库为主）+ `modernc.org/sqlite`（纯 Go，无 CGO）+ `BurntSushi/toml`。
前端原生 JS。镜像 `alpine` + 静态二进制，最终约 20MB。
