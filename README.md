# feishu-terminal

[![CI](https://github.com/VLa1111/feishu-terminal/actions/workflows/ci.yml/badge.svg)](https://github.com/VLa1111/feishu-terminal/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

把飞书 / Lark 聊天窗口变成一台服务器的持久交互式终端。
A Feishu/Lark long-connection bot that turns a chat into a persistent interactive shell — no public IP, domain or open ports required.

## English

**feishu-terminal** connects out to Feishu over a WebSocket long connection and gives every chat its own
persistent `tmux` session. Think of SSH-in-a-chat: run commands, answer prompts, drive TUI programs, and keep
`cd`, environment variables and background jobs across bot restarts and upgrades.

Highlights:

- **Persistent shell per chat** — backed by tmux; survives bot restarts, `systemctl restart` and self-updates.
- **Async long-running jobs** — results are pushed when done; oversized output is uploaded as a `.log` file.
- **Interactive cards** — y / n / Enter / Ctrl-C buttons, plus `/watch` cards that refresh in place for `top`/`htop`.
- **Agent-friendly** — `/help ai` and `/help json` return a machine-readable capability spec so another bot can drive it.
- **Self-update from GitHub Releases** — `/checkupdate`, `/upgrade`, `/rollback`, with SHA-256 verification and a
  `--version` self-test before the binary is replaced. Updates re-exec in place: the PID and tmux sessions survive.
- **Installable** — cross-compiled linux/amd64 + linux/arm64 tarballs with `install.sh` and a systemd unit.

Quick start:

```bash
# grab the latest tarball from
# https://github.com/VLa1111/feishu-terminal/releases/latest
tar xzf feishu-terminal-<version>-linux-amd64.tar.gz
cd feishu-terminal-<version>-linux-amd64
sudo ./install.sh
sudo vim /opt/feishu-terminal/.env    # FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_ALLOW_OPEN_IDS
sudo systemctl restart feishu-terminal
```

Enable self-updates by adding this to `.env`:

```
FEISHU_UPDATE_URL=https://github.com/VLa1111/feishu-terminal/releases/latest/download/release.json
```

Then send `/checkupdate` or `/upgrade` in the chat. Releases are built and published by GitHub Actions on every `v*` tag.

The full guide below is in Chinese. License: [MIT](LICENSE).

## 中文文档

在能出网的服务器上跑一个飞书机器人，用飞书窗口当「连续交互式终端」给服务器发 shell 指令。
基于飞书**长连接模式**（WebSocket），不需要公网 IP、域名或开放端口。

### 原理

- 长连接：`larkws` 客户端主动连到 `wss://open.feishu.cn` 接收事件。
- 会话：每个聊天（私聊按用户、群聊按群）维护一个**持久 PTY**，`cd`/环境变量/后台进程跨消息保留。
- 权限：`open_id` / `user_id` 白名单，只放行白名单内的发送者。
- 终端美化：会话跑在 `zsh` 里，通过 `ZDOTDIR` 指向 `zsh/` 目录下的 bot 专用 `.zshrc`，加载 oh-my-zsh 并提供**地址栏提示符**（当前目录 + git 分支），每条回复都是完整终端回显。

### 快速开始

```bash
cp .env.sample .env
# 编辑 .env，填入 FEISHU_APP_ID / FEISHU_APP_SECRET
go build -o feishu-terminal .
./feishu-terminal
```

日志出现 `connected to wss://...` 即连接成功。

> 终端美化依赖 `zsh`（默认 `/bin/zsh`）与 oh-my-zsh（可选，装了才加载）。bot 专用配置在 `zsh/.zshrc`，可按需调整提示符样式。

### 打包与部署到新服务器

```bash
make dist          # 交叉编译 linux/amd64 + linux/arm64
```

> 打包需要 Go 1.23+。也可以在任意装了 Go 的机器上执行，或直接使用 GitHub Actions 产出的 Release 包。

`dist/` 里的产物：

| 文件 | 用途 |
|---|---|
| `feishu-terminal-<ver>-linux-<arch>.tar.gz` | 新机器部署包：二进制 + README + `.env.sample` + `zsh/` + systemd 单元 + `install.sh` |
| `feishu-terminal-linux-<arch>` | 裸二进制，`/upgrade` 下载用 |
| `release.json` | 版本清单（版本号 + 各架构地址 + SHA-256） |
| `SHA256SUMS` | 所有产物的校验和 |

部署到新机器（推荐直接从 GitHub Release 下载）：

```bash
# 按架构选 amd64 或 arm64
curl -LO https://github.com/VLa1111/feishu-terminal/releases/latest/download/feishu-terminal-<ver>-linux-amd64.tar.gz
tar xzf feishu-terminal-<ver>-linux-amd64.tar.gz
cd feishu-terminal-<ver>-linux-amd64
sudo ./install.sh                  # 装到 /opt/feishu-terminal，注册 systemd 服务
sudo vim /opt/feishu-terminal/.env # 填 FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_ALLOW_OPEN_IDS
sudo systemctl restart feishu-terminal
```

也可以从本机构建后 scp 过去：

```bash
scp dist/feishu-terminal-<ver>-linux-amd64.tar.gz user@newhost:/tmp/
ssh user@newhost
tar xzf /tmp/feishu-terminal-<ver>-linux-amd64.tar.gz
cd feishu-terminal-<ver>-linux-amd64
sudo ./install.sh
```

- 默认安装到 `/opt/feishu-terminal`，以 sudo 发起者身份运行——**bot 的 shell 就是这个用户的权限**。可用 `PREFIX=... RUN_USER=... sudo ./install.sh` 覆盖。
- 目标机需要 `tmux` 和 `zsh`（`install.sh` 会检查并提示）。
- systemd 单元设置了 `KillMode=process`：`systemctl restart` 只重启 bot，tmux 里的 shell 和后台任务保留。
- 重复执行 `install.sh` 即可升级，已有 `.env` 不会被覆盖。
- **一台服务器 = 一个飞书应用**：同一个应用在两台机器上同时跑长连接，事件只会在其中一个实例上被处理，命令会随机落到某台机器。要在保留现有 bot 的同时再部署一台，请去开放平台新建一个应用（换一套 `FEISHU_APP_ID` / `FEISHU_APP_SECRET`，并让 AI bot 的 `mention_map` 指向新 bot）。

### 在线升级（/upgrade）

推荐直接使用 GitHub Release：每次推送 `v*` tag，GitHub Actions 会自动构建并发布产物。在 `.env` 里写：

```
FEISHU_UPDATE_URL=https://github.com/VLa1111/feishu-terminal/releases/latest/download/release.json
FEISHU_UPDATE_CHECK=6h
```

`releases/latest/download` 总是指向最新正式版本；清单里的构件地址是相对路径，会自动解析到同一个 Release 下。

自建更新源或本地验证时，把 `dist/` 放到任意静态 HTTP(S) 服务下：

```bash
make dist
make serve-dist        # http://<本机IP>:18080/release.json
```

然后对 bot 发：

```
/version             # 当前版本、pid、运行时长、可执行文件、更新源
/checkupdate         # 用 FEISHU_UPDATE_URL 检查
/checkupdate http://192.168.1.10:18080/release.json
/upgrade             # 下载 → 校验 → 自检 → 替换 → 原地重启
/upgrade http://192.168.1.10:18080/release.json
/rollback            # 回到上一版本；再执行一次切回来
```

`FEISHU_UPDATE_CHECK` 开启后，bot 会定时检查并往最近活跃的聊天推一条新版本通知。

升级过程的安全措施：

- 清单里的 `sha256` 与下载内容比对，不一致直接放弃；
- 新二进制先跑 `--version` 自检，架构不对或文件损坏不会覆盖能跑的版本；
- 旧版本备份为 `<可执行文件>.rollback`；
- 用 `syscall.Exec` 原地重启：PID、cwd、环境变量、日志重定向都不变，飞书长连接自动重连；tmux 是独立进程，**shell 和后台任务都不丢**；
- 目标目录必须有写权限，否则升级会明确报错且不动当前版本。

安全提醒：`/upgrade <URL>` 允许白名单内的人安装任意二进制。白名单本来就能执行任意 shell 命令，所以这不扩大权限；但**不要把不可信的 bot 加进白名单**。HTTPS 能保证清单与二进制未被篡改；局域网明文 HTTP 下 SHA-256 只能防传输损坏，不防中间人。

### Docker 部署（放到服务器）

服务器上装好 Docker + Docker Compose 后：

```bash
# 1) 本地打一个干净源码包（不含二进制与 .env），scp 到服务器
make pack
scp feishu-terminal-src.tar.gz user@server:/tmp/

# 2) 服务器上解压、配置、启动
ssh user@server
mkdir -p ~/feishu-terminal && tar xzf /tmp/feishu-terminal-src.tar.gz -C ~/feishu-terminal
cd ~/feishu-terminal
cp .env.sample .env          # 填入 FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_ALLOW_OPEN_IDS
make up                      # = docker compose up -d --build
make logs                    # 看到 connected to wss://... 即成功
```

说明：

- **长连接模式不开端口**：容器只主动连出到飞书，无需映射/开放任何端口，也无需公网 IP。
- **配置保密**：通过 `env_file: .env` 注入环境变量，`.env` 已被 `.dockerignore` 排除，不会写进镜像层。
- **镜像内已装 `zsh` + `git`**：终端美化（地址栏 + 分支）开箱即用。oh-my-zsh 未装（可选，需则在 `Dockerfile` 里加）。
- **更新**：改完代码后 `make up` 会重新构建并重启；只改 `.env` 则 `make restart` 即可。

### 飞书后台配置

1. 打开 [飞书开放平台](https://open.feishu.cn)，创建**企业自建应用**（长连接模式只支持自建应用）。
2. 在「凭证与基础信息」里拿到 `App ID` / `App Secret`，填入 `.env`。
3. 进入「事件与回调」→「订阅方式」选择 **使用长连接接收事件**。
4. 添加事件，订阅 **接收消息 `im.message.receive_v1`**。
5. **按钮卡片需要卡片回调**：在「事件与回调」→「回调配置」里订阅
   **卡片回传交互 `card.action.trigger`**（同样走长连接接收，不需要公网地址）。
   没配这一项时卡片照常显示，但点按钮不会有反应。
6. 在「应用发布」→「版本管理与发布」创建版本并发布（或至少把自己加进可用成员）。
7. 私聊 bot 发一条消息，看运行日志里打印的 `open_id`，把它填进 `FEISHU_ALLOW_OPEN_IDS`，重启即可放行。

### 使用

- 直接发命令：`ls -la`、`cd /tmp`、`pwd`、`cat 文件` 等
- `/help` 查看帮助卡片（带「查看当前画面 / 任务状态 / AI 规格」按钮）
- `/help ai` 输出给 AI agent 的精简能力规格；`/help json` 是同一份内容的 JSON 版
- `/reset` 重置当前会话（杀掉会话内所有进程）
- `/version` 查看版本、运行时长、可执行文件路径与更新源
- `/checkupdate` / `/upgrade` / `/rollback` 在线检查、升级与回滚（见上文「在线升级」）
- **群聊**：@机器人 后跟命令才会执行，例如 `@terminal-bot ls -la`

#### 交互式命令

命令在前台运行、终端停在提示符以外时，**下一条消息会作为它的标准输入**（相当于在终端里打字回车）。
所以 `apt install`、`read`、`python` REPL 之类可以直接回 `y`。

- `/screen [行数]` 查看终端当前画面（默认当前一屏）
- `/key <按键>` 发送特殊按键：`ctrl-c` / `tab` / `enter` / `esc` / `up` / `down` / `left` / `right` / `bspace`
- `/raw <命令>` 把 TUI 程序直接放到终端前台，例如 `/raw htop`，之后用 `/screen` 看画面、`/key ctrl-c` 退出
- `/jobs` 查看正在跟踪的后台任务

命令超过 `FEISHU_EXEC_TIMEOUT` 还没结束时会发一张卡片，上面有 **y / n / Enter / Ctrl-C / 查看画面** 按钮——
`apt install` 这类等确认的提示，点一下就能回答，不用打字。

#### 实时画面（/watch）

`/watch` 把终端画面放进一张卡片里**原地刷新**，适合 `top`/`htop` 这类动态程序：

```
/watch top                  # 每 2 秒刷新，最多 5 分钟
/watch top --every 3s --for 10m
/raw htop                   # 也可以先手动起 TUI，再：
/watch                      # 只监视当前画面（不给命令）
/watch stop                 # 或点卡片上的「停止刷新」
```

刷新停止不会杀掉进程：TUI 还在前台，用 `/key ctrl-c` 退出。

#### 进入容器

`docker attach` 必须用 `/raw` 起，否则容器输出会被重定向到任务文件、屏幕上看不到：

```
/raw docker attach 122d82619ac6
/key ctrl-p
/key ctrl-q          # 两下都是 Ctrl-P Ctrl-Q 的脱离序列（不是 Ctrl-C！）
```

**不要**对 attach 发 Ctrl-C / Ctrl-D：docker 会把信号转发给容器主进程，PID 1 退出就等于容器停止。
多个终端同时 attach 是允许的，脱离互不影响。

#### 长命令与长输出

- 命令超过 `FEISHU_EXEC_TIMEOUT`（默认 8 秒）没结束，bot 先回执，**结束后自动把结果发到群里**；期间进程不会被杀。
- 输出超过 `FEISHU_OUTPUT_LIMIT`（默认 3000 字节）时，完整输出会作为 `.log` 文件发送，消息里只留首尾。
  > 文件上传需要应用具备上传文件资源的权限；没开时自动退化为分段发送。

#### 会话

每个聊天对应一个 **tmux 会话**（socket 名 `feishu-terminal`，与你自己的 tmux 完全隔离）。
bot 重启不会丢 shell——`cd`、环境变量、后台进程都还在。

### 接入另一个 AI bot

想让另一个接入 AI agent 的 bot 直接给终端 bot 发指令：

1. 把两个 bot 拉进同一个群（飞书里 bot 之间不能直接私聊）。
2. 让 AI bot 在群里 **@终端 bot** 发一条消息，例如 `@terminal-bot pwd`。
3. 看终端 bot 的运行日志，会打印出 AI bot 的 `open_id` / `user_id`，把它填进 `FEISHU_ALLOW_OPEN_IDS`（逗号分隔）并重启。
4. 之后 AI bot 只要在群里 @终端 bot 发指令即可执行，终端 bot 会把结果回在群里。
5. 让 Agent 先发一次 `@terminal-bot /help ai`：返回的纯文本规格包含全部命令、状态机与限制，
   会留在群历史里，后续每轮都能读到（需要 `group_chat_history_share`）。

### 已知限制

- TUI 程序（`vim`/`top`/`htop`）用 `/raw` 起、`/watch` 自动刷新；`/watch` 到期后**不会**杀进程，
  需要 `/key ctrl-c` 退出。极端情况下飞书 PATCH 有频控，刷新间隔不要低于 1 秒。
- 按钮回调里的操作者同样要过白名单：群成员能看见卡片，但不在白名单的人点按钮会被拒绝。
- 密码提示（`sudo`/`ssh`/`passwd`）读的是 `/dev/tty`，不会走标准输入，无法在聊天里输入。
  而且密码会永久留在群消息里——请用 NOPASSWD / 密钥登录替代。
- 命令的 stdout/stderr 被重定向到文件，所以依赖 TTY 判断的程序会认为自己在管道里（多数只是少了颜色）。
- 务必保持白名单只含本人（及可信的 AI bot）；这是把 shell 暴露在聊天里，风险自负。

### 开发与发布

依赖 Go 1.23+、`tmux`、`zsh`。

- `make build` — 构建本地二进制，版本取自 `VERSION`
- `make test` — 跑全部测试（会话相关测试需要真实 tmux）
- `make dist` — 生成 amd64/arm64 发布产物
- `./feishu-terminal --version` — 查看构建时注入的版本

发布新版本：修改 `VERSION` 并提交，然后打 tag：

```bash
git tag v2.0.1
git push origin v2.0.1
```

GitHub Actions 会跑测试、构建两个架构、创建 Release 并上传 `release.json`、裸二进制和部署包。
已部署的服务器用 `/checkupdate` / `/upgrade` 拉取，或开启 `FEISHU_UPDATE_CHECK` 后自动收到通知。

### 许可证

[MIT](LICENSE)

