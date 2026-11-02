# szNote

[![build](https://github.com/zsgm-bit/szNote/actions/workflows/build.yml/badge.svg)](https://github.com/zsgm-bit/szNote/actions/workflows/build.yml)
[![license](https://img.shields.io/badge/license-szNote%201.0%20(non--commercial%20free)-blue.svg)](LICENSE)

类 [note.ms](https://note.ms) 的自托管网页笔记服务：Go 前后端一体、单二进制、零第三方依赖。
打开 `http://your-host:9720/任意地址` 就能写 Markdown、贴图、传文件，到期自动删除。

> 授权提示：**非商业使用完全免费**（个人 / 教学 / 研究 / 非营利）；
> **商业使用与商业改编需付费授权**，详见 [授权](#授权)。

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
  "server":   { "port": 9720, "password": "", "session_days": 7 },
  "storage":  { "data_dir": "./data", "max_note_mb": 5, "max_file_mb": 50, "inline_preview_kb": 512 },
  "policy":   { "default_ttl_hours": 24, "rate_kbps": 100 },
  "security": { "max_login_attempts": 5, "lock_minutes": 10 }
}
```

| 配置项 | 默认 | 说明 |
|---|---|---|
| `server.port` | `9720` | 监听端口（Docker 下对外端口请改 `docker-compose.yml` 的 `ports` 左侧） |
| `server.password` | 空 | 站点访问密码，阅读 / 编辑 / 上传统一使用；留空 = 不启用 |
| `server.session_days` | `7` | 登录会话有效天数 |
| `storage.data_dir` | `./data` | 数据目录（容器内为 `/app/data`） |
| `storage.max_note_mb` | `5` | 单条笔记正文上限 (MB) |
| `storage.max_file_mb` | `50` | **单个附件大小上限 (MB)** |
| `storage.inline_preview_kb` | `512` | 文本附件在线预览上限 (KB)，超过则改为下载 |
| `policy.default_ttl_hours` | `24` | 新笔记默认有效期，只能填 24 / 72 / 120 / 168（1/3/5/7 天） |
| `policy.rate_kbps` | `100` | 每连接下载限速 (KB/s)，`0` = 不限速（内网建议 0） |
| `security.max_login_attempts` | `5` | 同一 IP 连续登录失败多少次锁定 |
| `security.lock_minutes` | `10` | 锁定时长（分钟） |

优先级：**内置默认值 → `config.json` → 环境变量**。字段少写就用默认值；JSON 写错会打印日志并整份回退默认值，不会带着半份配置启动。
配置文件路径可用 `SZN_CONFIG=/path/to/config.json` 指定。

环境变量仅用于临时覆盖（`SZN_` 前缀，旧版 `QN_` 前缀仍兼容）：

| 变量 | 对应配置项 |
|---|---|
| `SZN_PORT` / `SZN_PASSWORD` / `SZN_SESSION_DAYS` | `server.*` |
| `SZN_DATA` / `SZN_MAX_NOTE_MB` / `SZN_MAX_FILE_MB` / `SZN_INLINE_PREVIEW_KB` | `storage.*` |
| `SZN_TTL_HOURS` / `SZN_RATE_KBPS` | `policy.*` |
| `SZN_MAX_LOGIN_ATTEMPTS` / `SZN_LOCK_MINUTES` | `security.*` |

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

## 授权

本项目采用 **szNote License v1.0**（双轨授权，全文见 [LICENSE](LICENSE)）：

| 场景 | 是否需要付费 |
|---|---|
| 个人学习、兴趣项目、自用 | 免费 |
| 教学、学术研究、非营利组织内部使用 | 免费 |
| 商业实体的功能评估与演示 | 免费 |
| **营利组织内部部署，支撑经营业务** | **需商业授权** |
| **以 SaaS / 托管 / 云市场形式对外提供** | **需商业授权** |
| **集成进商业产品、硬件、方案对外销售** | **需商业授权** |
| **商业性改编、二次开发、定制开发** | **需商业授权** |
| **基于本项目的收费支持 / 培训 / 运维** | **需商业授权** |

商业授权（年度订阅或一次性买断）请通过仓库 [Issues](https://github.com/zsgm-bit/szNote/issues) 联系作者说明用途与规模，获取报价与合同。

第三方组件许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 故障排查

- **端口占用**：改 `docker-compose.yml` 里 `ports` 的宿主机端口，如 `"8080:9720"`。
- **改了 config.json 不生效**：需要重启进程（`docker compose restart`）；Docker 下确认挂载的是同一个文件。
- **config.json 写错**：启动日志会打印「解析失败，全部改用内置默认值」，按提示修 JSON（常见是多了逗号）。
- **贴日志后标题/斜体乱掉**：日志里的 `#`、`*`、`_` 被当成 Markdown 了 —— 顶栏点「Markdown」切到**纯文本模式**。
- **提示正文超过上限**：调大 `storage.max_note_mb`，或把大日志当附件上传（附件上限 `storage.max_file_mb`，默认 50MB）。
- **预览大日志很慢**：受 `policy.rate_kbps` 限速影响（默认 100KB/s），内网部署建议设 `0`；超过 `inline_preview_kb` 的文本改为下载。
- **附件点了没反应**：exe、html 等非白名单类型强制下载，属预期安全行为。
- **数据在哪**：`./data/notes/<地址>/note.md` 是正文，`files/` 是附件；清理以 `note.md` 修改时间为准。
- **忘记密码**：改 `config.json` 的 `server.password` 后重启。
