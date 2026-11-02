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
- **图片粘贴 / 拖拽**：`Ctrl+V` 贴截图自动上传并插入引用；拖到编辑区插引用，拖到右侧附件栏只存不插
- **代码友好**：代码块不折行、`tab-size: 4`，右上角一键「复制」，复制内容保留原始缩进与换行，粘到 Linux `bash` 或 Windows `cmd`/PowerShell 均可直接执行
- **有效期 1 / 3 / 5 / 7 天**：按笔记选择，以最后一次查看 / 保存 / 上传为记账起点，到期整文件夹删除（服务端白名单校验）
- **整包导出 zip**：正文 + 全部附件一键打包
- **附件管理**：缩略图、悬停看完整文件名、一键下载、同名文件自动加序号
- **下载限速**：默认每连接 100 KB/s，兼容 Range 断点续传
- **零依赖**：纯 Go 标准库；前端 marked / DOMPurify 本地内嵌，构建不联网拉模块
- **持久化**：数据落在 `./data`，容器重建不丢

> 安全提示：知道地址（及密码）的人即可查看和编辑，请勿存放敏感信息。
> 公网部署务必：① 设置 `SZN_PASSWORD`；② 前置 Nginx / Caddy 做 HTTPS（服务本身不提供 TLS，密码走明文 HTTP 会被嗅探）。
> 附件响应带 CSP sandbox，非白名单类型强制下载，不会在浏览器中执行脚本。

## 快速开始

### 方式一：Docker（推荐）

```bash
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

### 方式三：源码构建

```bash
git clone https://github.com/zsgm-bit/szNote.git
cd szNote
make build          # 产出 ./sznote
make run            # 构建并运行
```

或不用 make：`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sznote .`

## 配置

全部通过环境变量（旧版 `QN_` 前缀仍然兼容，优先级低于新名）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `SZN_PORT` | `9720` | 监听端口 |
| `SZN_DATA` | `./data` | 数据目录 |
| `SZN_PASSWORD` | 空 | 站点访问密码（阅读 / 编辑同一密码），留空 = 不启用 |
| `SZN_TTL_HOURS` | `24` | 新笔记默认有效期；页面可选 24 / 72 / 120 / 168（1/3/5/7 天） |
| `SZN_MAX_NOTE_KB` | `1024` | 单条笔记正文上限 (KB) |
| `SZN_MAX_FILE_MB` | `50` | 单个上传文件上限 (MB) |
| `SZN_RATE_KBPS` | `0`（compose 内为 100） | 每连接附件下载限速 (KB/s)，`0` = 不限速 |

## 存储结构

每条笔记是 `data/notes/<地址>/` 下的一个独立文件夹，无内存缓存层，读写直达磁盘；
过期清理以 `note.md` 的修改时间为准，整文件夹删除：

```
data/notes/<地址>/
├── note.md     # 正文（修改时间 = 最后使用时间）
├── meta.json   # 元数据：{"ttl_hours": 24|72|120|168}
└── files/      # 附件
    ├── pic.png
    └── doc.pdf
```

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 健康检查（始终开放，返回是否启用密码） |
| POST | `/api/login` | 登录，body `{"password":"..."}` |
| POST | `/api/logout` | 退出登录 |
| GET | `/api/{slug}` | 读取笔记 `{slug, exists, content, files[], ttl_hours}` |
| PUT | `/api/{slug}` | 保存，body `{"content":"...","ttl_hours":24}` |
| POST | `/api/{slug}/upload` | multipart 上传，字段名 `file` |
| DELETE | `/api/{slug}/file/{name}` | 删除附件 |
| GET | `/api/{slug}/export` | 整包导出 zip |
| GET | `/f/{slug}/{name}` | 附件访问（图片 / PDF / 文本内嵌，其余下载） |

除 `health`、`login` 外，启用密码后所有接口均需登录 Cookie。`ttl_hours` 仅接受 24 / 72 / 120 / 168。

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
- **镜像拉不下来**：基础镜像为 `golang:1.23-alpine` / `alpine:3.20`，网络受限时给 Docker 配镜像加速。
- **附件点了没反应**：exe、html 等非白名单类型强制下载，属预期安全行为。
- **数据在哪**：`./data/notes/<地址>/note.md` 是正文，`files/` 是附件；清理以 `note.md` 修改时间为准。
- **忘记密码**：`SZN_PASSWORD` 就是密码，改环境变量重启即可。
