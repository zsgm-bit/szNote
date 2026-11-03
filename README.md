# szNote

[![build](https://github.com/zsgm-bit/szNote/actions/workflows/build.yml/badge.svg)](https://github.com/zsgm-bit/szNote/actions/workflows/build.yml)
[![license](https://img.shields.io/badge/license-AGPL--3.0%20%7C%20Commercial-blue.svg)](LICENSE)

类 [note.ms](https://note.ms) 的自托管网页笔记服务：Go 前后端一体、单二进制、零第三方依赖。
打开 `http://your-host:9720/任意地址` 就能写 Markdown、贴图、传文件，到期自动删除。

## 特性

- **短链 / 自定义地址**：`http://<host>:9720/39gj29` 或 `/自定义名`（字母数字、`-`、`_`，最长 64）
- **站点密码保护**：设置 `SZN_PASSWORD` 后，阅读 / 编辑 / 上传统一登录；会话为 HMAC 签名 Cookie（7 天有效），密码比较恒定时间，连续失败 5 次锁 IP 10 分钟
- **Markdown + 三态预览**：点「预览」＝分屏，再点＝全屏预览，再点回到编辑（`Ctrl+P` 同效）
- **纯文本模式**：一键切到纯文本（每个笔记独立记忆），不解析 Markdown，日志里的 `#`、`*`、`_` 原样保留，附「复制全部」；排错贴日志 / 命令用这个
- **图片粘贴 / 拖拽**：`Ctrl+V` 贴截图自动上传并插入引用；拖到编辑区插引用，拖到右侧附件栏只存不插
- **代码友好**：代码块不折行、`tab-size: 4`，右上角一键「复制」，复制内容保留原始缩进与换行，粘到 Linux `bash` 或 Windows `cmd`/PowerShell 均可直接执行
- **文本附件在线预览**：`.log` `.txt` `.conf` `.yml` `.sh` `.ps1` 等直接按 UTF-8 纯文本打开（≤ 512KB 内嵌，更大改下载），无需先落地再看
- **正文上限 5MB**：长日志、长输出可直接整段贴进正文（超过返回 413 并提示具体上限），更大内容走附件
- **有效期 1 / 3 / 5 / 7 天**：按笔记选择，以最后一次查看 / 保存 / 上传为记账起点，到期整文件夹删除（服务端白名单校验）
- **整包导出 zip**：正文 + 全部附件一键打包
- **附件管理**：缩略图、悬停看完整文件名、一键下载、同名文件自动加序号
- **下载限速**：默认每连接 100 KB/s，兼容 Range 断点续传
- **配置独立成文件**：全部参数集中在 `config.json`，改完重启即生效，不用改 compose
- **零依赖**：纯 Go 标准库；前端 marked / DOMPurify 本地内嵌，构建不联网拉模块
- **持久化**：数据落在 `./data`，容器重建不丢

> 安全提示：知道地址（及密码）的人即可查看和编辑，请勿存放敏感信息。
> 公网部署务必：① 设置 `config.json` 的 `server.password`；② 前置 Nginx / Caddy 做 HTTPS（服务本身不提供 TLS，密码走明文 HTTP 会被嗅探）。
> 附件响应带 CSP sandbox，非白名单类型强制下载，不会在浏览器中执行脚本。
> 如果配置文件里填了真实密码，请勿提交到公开仓库 —— 可改用 `SZN_PASSWORD` 环境变量注入。

## 快速开始

### 方式一：Docker（推荐）

```bash
# 1. 先按需修改 config.json（密码 / 上限 / 限速 / 有效期）
# 2. 启动
docker compose up -d --build
```

访问 `http://localhost:9720`。

### 方式二：单二进制

从 [Releases](https://github.com/zsgm-bit/szNote/releases) 下载对应平台文件，解压后直接运行：

```bash
# Linux / macOS
chmod +x sznote_linux_amd64 && ./sznote_linux_amd64

# Windows
sznote_windows_amd64.exe
```

二进制会读取**当前工作目录下的 `config.json`**，把它放在同一个目录即可；也可以用 `SZN_CONFIG=/path/to/config.json` 指定。

### 方式三：源码构建

```bash
git clone https://github.com/zsgm-bit/szNote.git
cd szNote
make build          # 产出 ./sznote
make run            # 构建并运行
```

或不用 make：`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sznote .`

## 配置

所有参数集中在项目根目录的 **`config.json`** 里，改完重启生效。
Docker 部署时该文件以只读方式挂载进容器（`./config.json:/app/config.json`），**改配置不用重新构建镜像**。

```json
{
  "server":   { "port": 9720, "password": "", "password_hash": "", "session_days": 7 },
  "storage":  { "data_dir": "./data", "max_note_mb": 5, "max_file_mb": 50, "inline_preview_kb": 512, "max_note_total_mb": 200 },
  "policy":   { "default_ttl_hours": 24, "rate_kbps": 100 },
  "security": { "max_login_attempts": 5, "lock_minutes": 10, "trusted_proxies": ["127.0.0.1", "::1"], "rate_rps": 50, "allow_anonymous": false }
}
```

| 配置项 | 默认 | 说明 |
|---|---|---|
| `server.port` | `9720` | 监听端口（Docker 下对外端口请改 `docker-compose.yml` 的 `ports` 左侧） |
| `server.password` | 空 | 站点访问密码（明文），阅读 / 编辑 / 上传统一使用 |
| `server.password_hash` | 空 | 站点密码的 PBKDF2 哈希（推荐，见下方「密码哈希」），优先于明文 |
| `server.session_days` | `7` | 登录会话有效天数（会话存服务端，退出即吊销） |
| `storage.data_dir` | `./data` | 数据目录（容器内为 `/app/data`） |
| `storage.max_note_mb` | `5` | 单条笔记正文上限 (MB) |
| `storage.max_file_mb` | `50` | **单个附件大小上限 (MB)** |
| `storage.inline_preview_kb` | `512` | 文本附件在线预览上限 (KB)，超过则改为下载 |
| `storage.max_note_total_mb` | `200` | 单笔记（正文+附件）总大小上限 (MB)，`0` = 不限制 |
| `policy.default_ttl_hours` | `24` | 新笔记默认有效期，只能填 24 / 72 / 120 / 168（1/3/5/7 天） |
| `policy.rate_kbps` | `100` | 每连接下载限速 (KB/s)，`0` = 不限速（内网建议 0） |
| `security.max_login_attempts` | `5` | 同一 IP 连续登录失败多少次锁定 |
| `security.lock_minutes` | `10` | 锁定时长（分钟） |
| `security.trusted_proxies` | `["127.0.0.1","::1"]` | 可信反向代理 IP/CIDR：命中才解析 `X-Forwarded-For`（取真实客户端 IP 计数限速） |
| `security.rate_rps` | `50` | 每 IP 全局请求速率上限 (req/s)，防扫描与资源耗尽 |
| `security.allow_anonymous` | `false` | **显式接受"无密码匿名访问"风险**；不设密码又不开启此项时服务拒绝启动 |

> **安全默认值**：不设置密码（`password` / `password_hash` 均为空）且未显式声明
> `allow_anonymous: true` 时，服务**拒绝启动**。本仓库自带的 `config.json` 已为内网共用场景
> 打开了 `allow_anonymous`，**公网 / 穿透部署务必设置密码并把它改回 `false`**。

**密码哈希（推荐）**：用内置命令生成哈希后填入 `password_hash`，避免密码明文落盘：

```bash
./sznote -hash 你的密码
# 输出形如 pbkdf2$210000$<salt>$<hash>，整行复制到 server.password_hash 即可
```

环境变量仅用于临时覆盖（`SZN_` 前缀，旧版 `QN_` 前缀仍兼容）：

| 变量 | 对应配置项 |
|---|---|
| `SZN_PORT` / `SZN_PASSWORD` / `SZN_SESSION_DAYS` | `server.*` |
| `SZN_DATA` / `SZN_MAX_NOTE_MB` / `SZN_MAX_FILE_MB` / `SZN_INLINE_PREVIEW_KB` | `storage.*` |
| `SZN_TTL_HOURS` / `SZN_RATE_KBPS` | `policy.*` |
| `SZN_MAX_LOGIN_ATTEMPTS` / `SZN_LOCK_MINUTES` / `SZN_ALLOW_ANONYMOUS` | `security.*` |

## 存储结构

每条笔记是 `data/notes/<地址>/` 下的一个独立文件夹，无内存缓存层，读写直达磁盘；
过期清理以 `note.md` 的修改时间为准，整文件夹删除：

```
data/notes/<地址>/
├── note.md     # 正文（修改时间 = 最后使用时间）
├── meta.json   # 元数据：{"ttl_hours": 24, "plain": true}
└── files/      # 附件
    ├── pic.png
    └── doc.pdf
```

`plain` 为该笔记是否处于纯文本模式（缺省 = Markdown）。

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 健康检查（始终开放，返回是否启用密码、正文/附件上限） |
| POST | `/api/login` | 登录，body `{"password":"..."}` |
| POST | `/api/logout` | 退出登录 |
| GET | `/api/{slug}` | 读取笔记 `{slug, exists, content, files[], ttl_hours, plain}` |
| PUT | `/api/{slug}` | 保存，body `{"content":"...","ttl_hours":24,"plain":false}` |
| POST | `/api/{slug}/upload` | multipart 上传，字段名 `file` |
| DELETE | `/api/{slug}/file/{name}` | 删除附件 |
| GET | `/api/{slug}/export` | 整包导出 zip |
| GET | `/f/{slug}/{name}` | 附件访问（图片 / PDF / 文本内嵌，其余下载） |

除 `health`、`login` 外，启用密码后所有接口均需登录 Cookie。
`ttl_hours` 仅接受 24 / 72 / 120 / 168；`plain` 与 `ttl_hours` 都可省略，省略 = 不修改。
正文超过 `storage.max_note_mb` 返回 `413`。

## 开发

```bash
make vet            # go vet
make build          # 本平台二进制
make cross          # 交叉编译到 dist/（linux/darwin/windows × amd64/arm64）
make docker         # 本地构建镜像 sznote:local
make clean
```

目录：

```
.
├── main.go                 # 全部后端逻辑（标准库）
├── config.json             # 全部运行参数
├── static/                 # 前端（HTML/CSS/JS + 内嵌 marked、DOMPurify）
├── Dockerfile              # 两阶段构建，产物 ~15MB
├── docker-compose.yml
├── Makefile
└── .github/workflows/build.yml
```

## 自动化构建（CI/CD）

`.github/workflows/build.yml` 在以下时机自动执行：

| 触发 | 动作 |
|---|---|
| PR / push | `go vet` + 编译 + 冒烟测试（鉴权、读写、上传、导出 zip、非法 TTL 拦截） |
| push `main` | 交叉编译 6 个平台并上传 artifact；构建 `linux/amd64,linux/arm64` 镜像推送 `ghcr.io/zsgm-bit/sznote:latest` |
| push tag `v*` | 额外把各平台二进制打包为 `.tar.gz` / `.zip` + `checksums.txt`，自动创建 GitHub Release |

镜像使用示例：

```bash
docker run -d --name sznote -p 9720:9720 \
  -e SZN_PASSWORD='your-password' \
  -v "$PWD/data:/app/data" \
  ghcr.io/zsgm-bit/sznote:latest
```

## 安全机制（v1.0.2 加固）

- **鉴权与会话**：站点密码支持明文或 PBKDF2 哈希（`sznote -hash` 生成）；会话令牌带随机 nonce 且存于服务端会话表——**退出登录 / 重启进程即吊销**，被盗 Cookie 无法在登出后继续使用；Cookie 带 `HttpOnly + SameSite=Strict`，检测到 HTTPS（直连或反代 `X-Forwarded-Proto`）自动加 `Secure`。
- **启动安全检查**：监听所有网卡但未设密码时拒绝启动，除非显式 `allow_anonymous: true`（仅限可信内网）。
- **限速**：每 IP 全局令牌桶（`rate_rps`，默认 50 req/s）+ 登录失败锁定（支持 `trusted_proxies` 反代取真实 IP）。
- **上传策略**：禁止 `.html/.htm/.svg/.hta/.php` 等主动内容格式；内容嗅探拒绝伪装成 `.dat` 的网页/SVG；单笔记总大小配额（`max_note_total_mb`）；上传内存阈值固定 8MB。
- **响应头**：全站 CSP（`default-src 'self'`、`frame-ancestors 'none'`、`form-action 'none'`）+ `X-Frame-Options: DENY` + `Referrer-Policy: no-referrer`（笔记地址不会被外泄给第三方）；API 与页面 `Cache-Control: no-store`。**副作用：笔记 Markdown 里的外链图片不再在预览中显示**（仅本站附件 `/f/...` 与 `data:` 图片可内嵌）。
- **渲染净化**：DOMPurify 显式禁用 `form/input/style` 等标签与 `action/style` 属性，防止在笔记里构造同源钓鱼页。
- **附件服务**：`CSP: sandbox` + `nosniff`；SVG 一律强制下载；HTTP Server 带 `ReadHeaderTimeout` 等超时防 Slowloris。
- **容器加固**：compose 默认 `read_only + cap_drop ALL + no-new-privileges` + tmpfs `/tmp`。

## 故障排查

- **启动报「拒绝启动：…无访问密码」**：设置 `server.password`（或 `password_hash`），或在可信内网把 `security.allow_anonymous` 设为 `true`。
- **上传提示「不允许上传该类型文件」/「文件内容与类型不符」**：主动内容格式（html/svg 等）被禁止，属预期安全行为；需要分享网页代码请贴正文或打包成 zip。
- **端口占用**：改 `docker-compose.yml` 里 `ports` 的宿主机端口，如 `"8080:9720"`。
- **改了 config.json 不生效**：需要重启进程（`docker compose restart`）；Docker 下确认挂载的是同一个文件。
- **config.json 写错**：启动日志会打印「解析失败，全部改用内置默认值」，按提示修 JSON（常见是多了逗号）。
- **贴日志后标题/斜体乱掉**：日志里的 `#`、`*`、`_` 被当成 Markdown 了 —— 顶栏点「Markdown」切到**纯文本模式**。
- **提示正文超过上限**：调大 `storage.max_note_mb`，或把大日志当附件上传（附件上限 `storage.max_file_mb`，默认 50MB）。
- **提示附件总大小超过上限**：调大 `storage.max_note_total_mb`（默认 200MB），或删掉旧附件。
- **预览里外链图片不显示**：CSP 只允许本站附件与 `data:` 图片内嵌，属预期；把图片下载后作为本站附件插入即可。
- **预览大日志很慢**：受 `policy.rate_kbps` 限速影响（默认 100KB/s），内网部署建议设 `0`；超过 `inline_preview_kb` 的文本改为下载。
- **附件点了没反应**：exe、html 等非白名单类型强制下载，属预期安全行为。
- **数据在哪**：`./data/notes/<地址>/note.md` 是正文，`files/` 是附件；清理以 `note.md` 修改时间为准。
- **忘记密码**：改 `config.json` 的 `server.password` 后重启。

## 授权

本项目采用 **AGPL-3.0 + 商业许可 双轨制（dual licensing）**：

- 开源许可：[GNU AGPL-3.0](LICENSE) —— 遵守其义务即可免费使用，**包含商业使用**
- 商业许可：[COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md) —— 无法履行 AGPL 义务时的付费替代方案

| 场景 | 授权方式 |
|---|---|
| 个人学习、自用、兴趣项目 | AGPL-3.0，免费 |
| 教学、学术研究、非营利组织内部使用 | AGPL-3.0，免费 |
| 公司内部评估、POC、演示 | AGPL-3.0，免费 |
| 商业使用，且愿意公开修改后的源码 | AGPL-3.0，免费 |
| **闭源内部部署并修改源码** | **需商业许可** |
| **对外提供网页服务（含 SaaS）但不公开修改后的源码** | **需商业许可** |
| **集成进商业产品、硬件、方案对外销售** | **需商业许可** |
| **OEM / 白标发布** | **需商业许可** |
| **基于本项目的收费支持 / 培训 / 运维** | **需商业许可** |

> **AGPL-3.0 §13（网络服务条款）是关键**：只要用户通过网络与修改后的版本交互，就必须向这些用户提供
> 获取完整对应源码的方式——包括你自己的修改。**不打算公开修改内容，就需要商业许可。**

商业许可形式：年度订阅、一次性买断、OEM/白标（按产品线议价）。请通过
[Issues](https://github.com/zsgm-bit/szNote/issues)（标题注明 `[商业授权]`）说明使用主体、部署方式、
是否修改源码与规模，获取报价与合同。

> 版本说明：`v1.0.0` 及更早的发布物沿用旧的 szNote License v1.0；**自 `v1.0.1` 起改为 AGPL-3.0 + 商业许可双轨制**，
> 已发布的旧版本授权不受影响。

第三方组件许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
