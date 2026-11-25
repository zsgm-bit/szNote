// SPDX-License-Identifier: MIT
// Copyright (c) 2026 szNote authors <https://github.com/zsgm-bit/szNote>
// 授权：MIT License（全文见 LICENSE）。

package main

// szNote —— 类 note.ms 的自托管网页笔记服务（前后端一体，单二进制）
// 功能：短链/自定义别名笔记、Markdown 渲染、图片粘贴/文件上传、24h 无人访问自动删除
// 仅使用 Go 标准库，无第三方依赖。

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed static
var staticFS embed.FS

const (
	notesSub = "notes"                           // dataDir 下存放各笔记的子目录名
	noteFile = "note.md"                         // 笔记正文文件名
	metaFile = "meta.json"                       // 笔记元数据（自定义有效期）
	filesDir = "files"                           // 每条笔记的附件目录名
	alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // 随机短链字母表（去掉易混淆字符）
	slugLen  = 10                                // 随机地址长度（31 字符表 ≈ 49.5 bit 熵）

	sessionCookie = "qn_session"

	maxTTLHours = 168 // 笔记最长有效期：7 天（0 = 永久，单独判断）
)

// fileConfig 对应 config.json 的结构。字段留空/省略时使用内置默认值。
type fileConfig struct {
	Server struct {
		Port               flexInt `json:"port"` // 允许 9720 或 "9720" 两种写法
		Password           string  `json:"password"`
		PasswordHash       string  `json:"password_hash"` // pbkdf2$iter$salt$hash（sznote -hash 生成），优先于明文
		SessionDays        int     `json:"session_days"`
		SessionIdleMinutes int     `json:"session_idle_minutes"` // 空闲超时（分钟）：超时后会话吊销并提示重新登录，0=不启用
	} `json:"server"`
	Storage struct {
		DataDir         string `json:"data_dir"`
		MaxNoteMB       int    `json:"max_note_mb"`
		MaxFileMB       int    `json:"max_file_mb"`
		InlinePreviewKB int    `json:"inline_preview_kb"`
		MaxNoteTotalMB  int    `json:"max_note_total_mb"` // 单笔记（含附件）总大小上限，0=不限制
	} `json:"storage"`
	Transfer struct {
		Enabled      bool     `json:"enabled"`
		MaxFileMB    int      `json:"max_file_mb"`    // 单个中转文件上限
		UpKBPS       int      `json:"up_kbps"`        // 全局上行限速（KB/s），0=不限速
		DownKBPS     int      `json:"down_kbps"`      // 全局下行限速（KB/s），0=不限速
		ExpireHours  int      `json:"expire_hours"`   // 完成后的保留时长（到期自动清理）
		MaxStorageMB int      `json:"max_storage_mb"` // 中转目录总配额，防存储耗尽
		ExtWhitelist []string `json:"ext_whitelist"`  // 允许中转的扩展名（服务器不解包，仅按扩展名+魔数识别）
	} `json:"transfer"`
	Policy struct {
		DefaultTTLHours int `json:"default_ttl_hours"`
		RateKBPS        int `json:"rate_kbps"`
	} `json:"policy"`
	Security struct {
		MaxLoginAttempts int      `json:"max_login_attempts"`
		LockMinutes      int      `json:"lock_minutes"`
		TrustedProxies   []string `json:"trusted_proxies"` // 可信反代 IP/CIDR，命中才解析 X-Forwarded-For
		RateRPS          int      `json:"rate_rps"`        // 每 IP 全局请求速率上限（req/s），0=内置默认 50
		AllowAnonymous   bool     `json:"allow_anonymous"` // 显式接受"无密码匿名访问"风险
	} `json:"security"`
}

var (
	port    = "9720" // 监听端口（热加载不支持，改后需重启）
	dataDir = "./data"

	slugRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{5,63}$`) // 6~64 位
	reserved = map[string]bool{"static": true, "api": true, "f": true, "favicon.ico": true, "transfer": true}

	writeMu sync.Mutex // 序列化所有写操作，避免并发写坏文件

	authSecret []byte // 会话签名密钥，进程生命周期内固定

	configPath = "config.json" // 实际加载的配置文件路径（日志用）

	// 热加载的运行配置：watcher 检测到 config.json 变化后整体替换，
	// 读取侧通过 cfg() 取一致性快照
	hot atomic.Pointer[hotConfig]

	// 登录失败锁定状态（带时间戳，可被 janitor 老化清理）
	failMu sync.Mutex
	failIP = map[string]*failState{}

	// 服务端会话表：nonce -> 会话状态（支持退出即吊销 + 空闲超时自动登出）
	sessMu     sync.Mutex
	sessions   = map[string]*sessInfo{}
	limMu      sync.Mutex
	limitBucks = map[string]*limiter{}
)

// hotConfig 是可热加载的运行时配置集合。整体替换、整体读取，避免半新半旧。
type hotConfig struct {
	authOn         bool
	sitePassword   string // 明文密码，与 passwordHash 二选一
	passwordHash   string // PBKDF2 哈希，优先于明文
	allowAnonymous bool

	sessionDays        int
	sessionIdleMinutes int // 空闲超时（分钟），0=不启用；超时后会话吊销、前端弹回登录页

	maxNote         int64         // 笔记正文上限
	maxFile         int64         // 单附件上限
	maxNoteTotal    int64         // 单笔记含附件总上限
	inlinePreviewKB int           // 文本附件在线预览上限（KB）
	ttl             time.Duration // 默认有效期

	transferEnabled bool          // 文件互传（中转）总开关
	trMaxFile       int64         // 单个中转文件上限
	trUpKBPS        int           // 全局上行限速（KB/s），0=不限速
	trDownKBPS      int           // 全局下行限速（KB/s），0=不限速
	trExpire        time.Duration // 完成后的保留时长
	trMaxStorage    int64         // 中转目录总配额
	trExts          []string      // 扩展名白名单（小写、带点）

	rateKBPS int // 每连接下载限速（KB/s），0=不限速
	rateRPS  float64

	maxLoginAttempts int
	lockMinutes      int

	trustedNets []*net.IPNet
}

// cfg 返回当前配置快照
func cfg() *hotConfig { return hot.Load() }

// sessInfo 单个会话的服务端状态：绝对过期 + 最近活跃（空闲超时判定用）
type sessInfo struct {
	exp  int64 // 绝对过期时间（Unix 秒），与会话令牌中的 exp 一致
	last int64 // 最近一次通过鉴权的时间（Unix 秒）；空闲超时即吊销
}

// failState 单个 IP 的登录失败状态
type failState struct {
	count int
	until time.Time // 锁定截止时间
	last  time.Time // 最近一次失败时间（老化清理用）
}

// limiter 每 IP 令牌桶
type limiter struct {
	tokens float64
	last   time.Time
}

// flexInt 兼容 "9720" 与 9720 两种写法，避免手写 JSON 时类型不匹配导致整份配置失效
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("需要整数，收到 %s", s)
	}
	*f = flexInt(n)
	return nil
}

// defaultConfig 内置默认值（config.json 缺失或缺字段时使用）
func defaultConfig() fileConfig {
	cfg := fileConfig{}
	cfg.Server.Port = 9720
	cfg.Server.SessionDays = 7
	cfg.Server.SessionIdleMinutes = 30
	cfg.Storage.DataDir = "./data"
	cfg.Storage.MaxNoteMB = 5
	cfg.Storage.MaxFileMB = 10
	cfg.Storage.InlinePreviewKB = 512
	cfg.Storage.MaxNoteTotalMB = 200
	cfg.Transfer.Enabled = true
	cfg.Transfer.MaxFileMB = 50
	cfg.Transfer.UpKBPS = 100
	cfg.Transfer.DownKBPS = 100
	cfg.Transfer.ExpireHours = 24
	cfg.Transfer.MaxStorageMB = 500
	cfg.Transfer.ExtWhitelist = []string{".zip"}
	cfg.Policy.DefaultTTLHours = 24
	cfg.Policy.RateKBPS = 100
	cfg.Security.MaxLoginAttempts = 5
	cfg.Security.LockMinutes = 10
	cfg.Security.TrustedProxies = []string{"127.0.0.1", "::1"} // 默认只信任本机反代
	cfg.Security.RateRPS = 50
	cfg.Security.AllowAnonymous = false
	return cfg
}

// loadConfig 按「内置默认值 → config.json → 环境变量」的优先级装配配置。
// 环境变量优先级最高，便于在容器/CI 里临时覆盖单个参数。
// initial=true 用于进程启动（port/data_dir 一并生效）；
// initial=false 用于热加载（port/data_dir 保持原值，这两项改后需重启）。
// 返回 false 表示 config.json 存在但解析失败（热加载时应保留旧配置）。
func loadConfig(initial bool) bool {
	def := defaultConfig()
	fc := def

	// 配置文件位置：SZN_CONFIG（或旧名 QN_CONFIG），默认工作目录下 config.json
	path := envOr("SZN_CONFIG", "config.json")
	configPath = path
	parseOK := true
	if b, err := os.ReadFile(path); err == nil {
		tmp := def // 先填默认值再解 JSON，缺字段就保留默认
		if err := json.Unmarshal(b, &tmp); err != nil {
			log.Printf("config: failed to parse %s, keeping previous values (%v)", path, err)
			parseOK = false
		} else {
			fc = tmp
			log.Printf("config: loaded %s", path)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("config: failed to read %s, using built-in defaults (%v)", path, err)
	} else {
		log.Printf("config: %s not found, using built-in defaults (copy config.example.json to config.json to customize)", path)
	}

	// 端口与数据目录仅启动时生效（热加载不改动，避免运行中切换存储位置）
	if initial {
		port = strconv.Itoa(int(fc.Server.Port))
		dataDir = fc.Storage.DataDir
		if v := envOr("SZN_PORT", ""); v != "" {
			port = v
		}
		if v := envOr("SZN_DATA", ""); v != "" {
			dataDir = v
		}
	}

	h := &hotConfig{
		sitePassword:       fc.Server.Password,
		passwordHash:       strings.TrimSpace(fc.Server.PasswordHash),
		sessionDays:        fc.Server.SessionDays,
		sessionIdleMinutes: fc.Server.SessionIdleMinutes,
		maxNote:            int64(fc.Storage.MaxNoteMB) << 20,
		maxFile:            int64(fc.Storage.MaxFileMB) << 20,
		maxNoteTotal:       int64(fc.Storage.MaxNoteTotalMB) << 20,
		inlinePreviewKB:    fc.Storage.InlinePreviewKB,
		ttl:                time.Duration(fc.Policy.DefaultTTLHours) * time.Hour,
		rateKBPS:           fc.Policy.RateKBPS,
		transferEnabled:    fc.Transfer.Enabled,
		trMaxFile:          int64(fc.Transfer.MaxFileMB) << 20,
		trUpKBPS:           fc.Transfer.UpKBPS,
		trDownKBPS:         fc.Transfer.DownKBPS,
		trExpire:           time.Duration(fc.Transfer.ExpireHours) * time.Hour,
		trMaxStorage:       int64(fc.Transfer.MaxStorageMB) << 20,
		trExts:             normalizeExts(fc.Transfer.ExtWhitelist),
		maxLoginAttempts:   fc.Security.MaxLoginAttempts,
		lockMinutes:        fc.Security.LockMinutes,
		allowAnonymous:     fc.Security.AllowAnonymous,
		rateRPS:            50,
	}
	if fc.Security.RateRPS > 0 {
		h.rateRPS = float64(fc.Security.RateRPS)
	}
	h.trustedNets = parseCIDRs(fc.Security.TrustedProxies)
	h.authOn = h.sitePassword != "" || h.passwordHash != ""

	// 环境变量覆盖（旧版 QN_ 前缀同样兼容）；SZN_PORT / SZN_DATA 只在启动时生效
	if v := envOr("SZN_PASSWORD", ""); v != "" {
		h.sitePassword = v
		h.authOn = true
	}
	if n, ok := envIntSet("SZN_ALLOW_ANONYMOUS"); ok {
		h.allowAnonymous = n != 0
	}
	if n, ok := envIntSet("SZN_SESSION_DAYS"); ok {
		h.sessionDays = n
	}
	if n, ok := envIntSet("SZN_MAX_NOTE_MB"); ok {
		h.maxNote = int64(n) << 20
	} else if n, ok := envIntSet("SZN_MAX_NOTE_KB"); ok { // 旧变量
		h.maxNote = int64(n) << 10
	}
	if n, ok := envIntSet("SZN_MAX_FILE_MB"); ok {
		h.maxFile = int64(n) << 20
	}
	if n, ok := envIntSet("SZN_INLINE_PREVIEW_KB"); ok {
		h.inlinePreviewKB = n
	}
	if n, ok := envIntSet("SZN_TTL_HOURS"); ok {
		h.ttl = time.Duration(n) * time.Hour
	}
	if n, ok := envIntSet("SZN_RATE_KBPS"); ok {
		h.rateKBPS = n
	}
	if n, ok := envIntSet("SZN_TRANSFER_MAX_MB"); ok {
		h.trMaxFile = int64(n) << 20
	}
	if n, ok := envIntSet("SZN_TRANSFER_UP_KBPS"); ok {
		h.trUpKBPS = n
	}
	if n, ok := envIntSet("SZN_TRANSFER_DOWN_KBPS"); ok {
		h.trDownKBPS = n
	}
	if n, ok := envIntSet("SZN_TRANSFER_EXPIRE_HOURS"); ok {
		h.trExpire = time.Duration(n) * time.Hour
	}
	if n, ok := envIntSet("SZN_MAX_LOGIN_ATTEMPTS"); ok {
		h.maxLoginAttempts = n
	}
	if n, ok := envIntSet("SZN_LOCK_MINUTES"); ok {
		h.lockMinutes = n
	}

	// 兜底：避免 0 值把服务搞成不可用
	if !validTTL(int(h.ttl.Hours())) {
		log.Printf("config: default_ttl_hours=%d not in 24/72/120/168, using 24", int(h.ttl.Hours()))
		h.ttl = 24 * time.Hour
	}
	if h.maxNote <= 0 {
		h.maxNote = 5 << 20
	}
	if h.maxFile <= 0 {
		h.maxFile = 10 << 20
	}
	if h.trMaxFile <= 0 {
		h.trMaxFile = 50 << 20
	}
	if h.trExpire <= 0 {
		h.trExpire = 24 * time.Hour
	}
	if h.trMaxStorage <= 0 {
		h.trMaxStorage = 500 << 20
	}
	if len(h.trExts) == 0 {
		h.trExts = []string{".zip"}
	}
	if h.trUpKBPS < 0 {
		h.trUpKBPS = 0
	}
	if h.trDownKBPS < 0 {
		h.trDownKBPS = 0
	}
	if h.sessionDays <= 0 {
		h.sessionDays = 7
	}
	if h.sessionIdleMinutes < 0 {
		h.sessionIdleMinutes = 30
	}
	if h.maxLoginAttempts <= 0 {
		h.maxLoginAttempts = 5
	}
	if h.lockMinutes <= 0 {
		h.lockMinutes = 10
	}

	// 热加载时解析失败不覆盖旧配置；启动时没有旧配置可保，照常落默认值
	if !parseOK && !initial {
		return false
	}
	hot.Store(h)
	return true
}

// ---------- 工具函数 ----------

// envOr 读取环境变量；为兼容旧版（QuickNote 时期），SZN_X 未设置时回退读取 QN_X
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if rest, ok := strings.CutPrefix(key, "SZN_"); ok {
		if v := os.Getenv("QN_" + rest); v != "" {
			return v
		}
	}
	return def
}

func envIntSet(key string) (int, bool) {
	if v := envOr(key, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}

func notesRoot() string { return filepath.Join(dataDir, notesSub) }

func noteDir(slug string) string  { return filepath.Join(notesRoot(), slug) }
func notePath(slug string) string { return filepath.Join(noteDir(slug), noteFile) }
func metaPath(slug string) string { return filepath.Join(noteDir(slug), metaFile) }

// legacySlugRe 历史地址：旧版允许 1~5 位。已存在的旧笔记继续可用（读写均可），但不再允许新建。
var legacySlugRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,4}$`)

func validSlug(slug string) bool {
	if reserved[slug] {
		return false
	}
	if slugRe.MatchString(slug) { // 6~64 位（当前规则）
		return true
	}
	if legacySlugRe.MatchString(slug) { // 1~5 位历史地址：仅当笔记已存在时放行
		if fi, err := os.Stat(noteDir(slug)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// noteMeta 每条笔记的元数据。TTLHours: 24/72/120/168（即 1/3/5/7 天）；文件不存在 = 使用全局默认
// Plain: true 表示纯文本模式（前端不解析 Markdown，适合贴日志/命令）
// PwdHash: 非空 = 该笔记受单笔记密码保护（"pbkdf2$iter$salt_hex$hash_hex"，同站点密码格式；
// 正文以 AES-256-CBC 密文 blob 落盘，字段可缺省，旧 meta 文件完全兼容）
// Rev: 正文版本号（软锁用），每次成功写入正文 +1；旧 meta 缺省从 0 起，首次写入后即为 1
type noteMeta struct {
	TTLHours int    `json:"ttl_hours"`
	Plain    bool   `json:"plain,omitempty"`
	PwdHash  string `json:"pwd_hash,omitempty"`
	Rev      uint64 `json:"rev,omitempty"`
}

func loadMeta(slug string) (noteMeta, bool) {
	var m noteMeta
	b, err := os.ReadFile(metaPath(slug))
	if err != nil {
		return m, false
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false
	}
	return m, true
}

// plainFor 该笔记是否处于纯文本模式（无 meta 视为 Markdown）
func plainFor(slug string) bool {
	m, ok := loadMeta(slug)
	return ok && m.Plain
}

// validTTL 允许的有效期：仅 1/3/5/7 天
func validTTL(h int) bool {
	return h == 24 || h == 72 || h == 120 || h == 168
}

// ttlFor 返回某条笔记的过期时长；旧数据（0=永久或非 1/3/5/7 天）统一按 7 天
func ttlFor(slug string) time.Duration {
	if m, ok := loadMeta(slug); ok {
		if !validTTL(m.TTLHours) {
			return maxTTLHours * time.Hour
		}
		return time.Duration(m.TTLHours) * time.Hour
	}
	return cfg().ttl // 全局默认 default_ttl_hours
}

// safeName 清洗上传文件名：去掉路径部分、控制字符与 Windows 非法字符
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		if strings.ContainsRune(`/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(name), "."))
	if len(name) > 120 {
		ext := filepath.Ext(name)
		name = name[:120-len(ext)] + ext
	}
	if name == "" || name == ".." {
		name = "file"
	}
	return name
}

// touch 更新笔记的“最后使用时间”：优先以 note.md 的修改时间记账（重启不丢）；
// 没有正文的纯附件笔记退化为更新目录 mtime，保证与清理口径一致
func touch(slug string) {
	p := notePath(slug)
	now := time.Now()
	if _, err := os.Stat(p); err == nil {
		_ = os.Chtimes(p, now, now)
		return
	}
	_ = os.Chtimes(noteDir(slug), now, now)
}

// randSlug 生成随机短地址：rejection sampling 消除取模偏置（CWE-338/341）
func randSlug() (string, error) { return randCode(slugLen) }

// randCode 生成 n 位随机码（同 randSlug 的均匀采样）
func randCode(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	max := byte(256 - 256%len(alphabet)) // 248，保证均匀
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, v := range buf {
			if v >= max {
				continue // 拒绝采样，避免前几个字符概率偏高
			}
			out = append(out, alphabet[int(v)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// normalizeExts 归一化扩展名白名单：小写、保证带前导点、去空项
func normalizeExts(list []string) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- API 处理器 ----------

// GET /api/{slug} —— 读取笔记内容与文件列表
func getNote(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	password, protected, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	content, exists := "", false
	if b, err := os.ReadFile(notePath(slug)); err == nil {
		content, exists = string(b), true
		if protected {
			pt, derr := decryptBody(password, b)
			if derr != nil {
				fail(w, http.StatusInternalServerError, "正文解密失败（密码正确但数据损坏）")
				return
			}
			content = string(pt)
		}
		touch(slug)
	}
	ttlHours := int(ttlFor(slug).Hours())
	m, _ := loadMeta(slug)
	resp := map[string]any{
		"slug":      slug,
		"exists":    exists,
		"content":   content,
		"files":     listFiles(slug),
		"ttl_hours": ttlHours,
		"plain":     plainFor(slug),
		"pwd":       protected,
		"rev":       m.Rev, // 软锁版本号：客户端保存时回传，不匹配则 409
	}
	if protected {
		resp["ntok"] = noteAttachToken(slug, m.PwdHash) // 给预览 <img>/文件列表下载链接用
	}
	writeJSON(w, http.StatusOK, resp)
}

// PUT /api/{slug} —— 保存笔记正文 {"content": "...", "ttl_hours": 24, "plain": false}
// ttl_hours: 只能是 24/72/120/168；缺省 = 不修改（新笔记用全局默认）
// plain: 缺省 = 不修改
// rev（软锁）: 客户端把 GET 时拿到的版本号回传。与当前版本一致 → 正常保存；
// 不一致 → 409 并返回服务端当前 rev（正文不会被改动）；
// 缺省 = 旧客户端/CLI 的强制写入语义，正文保留到历史后覆盖。
// rev + force:true = 用户在 409 冲突弹窗里选择「以我的版本覆盖」，
// 服务端把被覆盖的旧正文快照进历史（覆盖但可找回），再执行写入。
func saveNote(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	password, _, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	// JSON 转义会放大体积，请求体上限给正文上限的 2 倍 + 64KB 余量，
	// 真正的正文上限在解码后按字符数严格校验
	c := cfg()
	r.Body = http.MaxBytesReader(w, r.Body, c.maxNote*2+(1<<16))
	var body struct {
		Content  string `json:"content"`
		TTLHours *int   `json:"ttl_hours"`
		Plain    *bool  `json:"plain"`
		Rev      *uint64 `json:"rev"`
		Force    bool    `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// 区分超限（413）与格式错误（400）：统一报 413 会误导用户以为是正文超限
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, http.StatusRequestEntityTooLarge, "请求体超过大小限制")
		} else {
			fail(w, http.StatusBadRequest, "请求体 JSON 无效")
		}
		return
	}
	if int64(len(body.Content)) > c.maxNote {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("正文超过上限（%d MB）", c.maxNote>>20))
		return
	}
	if body.TTLHours != nil {
		t := *body.TTLHours
		if !validTTL(t) {
			fail(w, http.StatusBadRequest, "有效期只能是 1/3/5/7 天（24/72/120/168 小时）")
			return
		}
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	// 软锁版本校验（锁内判定，避免与并发写入竞态）
	var force bool // 本次写入是否需要先快照旧正文
	if body.Rev != nil {
		cur, _ := loadMeta(slug)
		if *body.Rev != cur.Rev {
			// 版本冲突：无论是否 force 都拒绝，让客户端拿最新 rev 重试
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "error": "笔记已被其他人修改（版本冲突）", "rev": cur.Rev,
			})
			return
		}
		force = body.Force // rev 匹配 + force = 冲突解决的「以我的版本覆盖」
	} else {
		force = true // 旧客户端 / CLI：不带 rev = 强制覆盖语义，旧正文入历史
	}
	content := []byte(body.Content)
	// 锁内无条件重查保护状态与密码有效性（prepareBodyForWrite 内部重读 meta，评审 P2-3）
	enc, eerr := prepareBodyForWrite(slug, password, content)
	if eerr != nil {
		if errors.Is(eerr, errNotePwdChanged) {
			fail(w, http.StatusConflict, "笔记密码已变更，请刷新后重新解锁")
		} else {
			log.Printf("note /%s: encrypt failed: %v", slug, eerr)
			fail(w, http.StatusInternalServerError, "正文加密失败")
		}
		return
	}
	if force {
		snapshotHistory(slug, password, content) // 覆盖前把旧正文存入历史（best-effort，失败不阻断保存）
	}
	content = enc
	if err := persistNote(slug, content, body.TTLHours, body.Plain); err != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	m, _ := loadMeta(slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rev": m.Rev})
}

// persistNote 原子写入正文并合并更新 meta（调用方需已持有 writeMu）。
// ttlHours/plain 非 nil 即合并写入，避免覆盖另一项设置；无论是否携带，
// 每次成功写入正文都 Rev+1 并落盘 meta——软锁版本号只认「正文变更」这一件事。
func persistNote(slug string, content []byte, ttlHours *int, plain *bool) error {
	c := cfg()
	dir := noteDir(slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := notePath(slug) + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, notePath(slug)); err != nil {
		return err
	}
	m, _ := loadMeta(slug)
	if ttlHours != nil {
		m.TTLHours = *ttlHours
	}
	if plain != nil {
		m.Plain = *plain
	}
	if m.TTLHours == 0 { // 还没设过有效期：落成全局默认，避免 0 被当成无效值
		m.TTLHours = int(c.ttl.Hours())
	}
	m.Rev++
	if mb, err := json.Marshal(m); err != nil {
		log.Printf("note /%s: meta marshal failed (body ok): %v", slug, err)
	} else if err := os.WriteFile(metaPath(slug), mb, 0o600); err != nil {
		log.Printf("note /%s: meta update failed (body ok): %v", slug, err)
	}
	return nil
}

// writeFileSync 写文件并 fsync 落盘（供密码切换的事务化写入用，减小崩溃窗口）
func writeFileSync(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// GET /api/{slug}/raw —— 纯文本读正文，curl 友好：
//
//	curl http://host:8080/api/mynote/raw
//
// 笔记不存在返回 404；响应头带 X-Note-Ttl-Hours / X-Note-Plain 供脚本取元信息。
func getNoteRaw(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	password, protected, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	b, err := os.ReadFile(notePath(slug))
	if err != nil {
		fail(w, http.StatusNotFound, "笔记不存在")
		return
	}
	if protected {
		pt, derr := decryptBody(password, b)
		if derr != nil {
			fail(w, http.StatusInternalServerError, "正文解密失败（密码正确但数据损坏）")
			return
		}
		b = pt
	}
	touch(slug)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Note-Ttl-Hours", strconv.Itoa(int(ttlFor(slug).Hours())))
	w.Header().Set("X-Note-Plain", strconv.FormatBool(plainFor(slug)))
	if protected {
		w.Header().Set("X-Note-Protected", "1")
	}
	w.Write(b)
}

// PUT|POST /api/{slug}/raw 与 PUT /{slug} —— 请求体即正文原文，无需 JSON 包装：
//
//	curl -T 1.txt http://host:8080/api/mynote/raw          # 上传文件
//	cat 1.txt | curl -T - http://host:8080/api/mynote/raw  # 管道写入
//	echo hi | curl --data-binary @- -X POST http://host:8080/api/mynote/raw
//	curl -T 1.txt "http://host:8080/mynote?ttl=168"        # 路径别名 + 可选参数
//
// 可选查询参数：ttl=24|72|120|168（天×24）、plain=true|false；缺省 = 不修改
//（新笔记沿用全局默认）。响应为 {"ok":true,"slug":...,"bytes":...}。
func saveNoteRaw(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	password, _, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	c := cfg()
	r.Body = http.MaxBytesReader(w, r.Body, c.maxNote+(1<<16))
	content, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("正文超过上限（%d MB）", c.maxNote>>20))
		return
	}
	var ttlHours *int
	if q := r.URL.Query().Get("ttl"); q != "" {
		t, err := strconv.Atoi(q)
		if err != nil || !validTTL(t) {
			fail(w, http.StatusBadRequest, "有效期只能是 1/3/5/7 天（24/72/120/168 小时）")
			return
		}
		ttlHours = &t
	}
	var plain *bool
	if q := r.URL.Query().Get("plain"); q != "" {
		b, err := strconv.ParseBool(q)
		if err != nil {
			fail(w, http.StatusBadRequest, "plain 参数只能是 true 或 false")
			return
		}
		plain = &b
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	// 锁内无条件重查保护状态与密码有效性（prepareBodyForWrite 内部重读 meta，评审 P2-3）
	enc, eerr := prepareBodyForWrite(slug, password, content)
	if eerr != nil {
		if errors.Is(eerr, errNotePwdChanged) {
			fail(w, http.StatusConflict, "笔记密码已变更，请刷新后重新解锁")
		} else {
			log.Printf("note /%s: encrypt failed: %v", slug, eerr)
			fail(w, http.StatusInternalServerError, "正文加密失败")
		}
		return
	}
	snapshotHistory(slug, password, content) // raw 写入 = 强制覆盖语义（CLI/管道），旧正文入历史（best-effort）
	content = enc
	if err := persistNote(slug, content, ttlHours, plain); err != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	m, _ := loadMeta(slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": slug, "bytes": len(content), "rev": m.Rev})
}

// blockedExts 禁止上传的主动内容格式（会在浏览器中执行脚本 / 可托管钓鱼页）
var blockedExts = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".shtml": true, ".hta": true,
	".svg": true, ".svgz": true,
	".php": true, ".asp": true, ".aspx": true, ".jsp": true, ".cgi": true,
	".url": true, ".lnk": true, ".scr": true,
}

// POST /api/{slug}/upload —— 上传文件（multipart 字段名 file），返回访问 URL
func upload(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	// 受保护笔记：附件管理同样需要密码（评审 P1-1，附件本体仍不加密）
	if _, _, ok := noteAuth(w, r, slug); !ok {
		return
	}
	c := cfg()
	r.Body = http.MaxBytesReader(w, r.Body, c.maxFile+(1<<20))
	// 内存阈值固定 8MB，超出部分自动落临时文件，避免并发上传放大内存
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		fail(w, http.StatusBadRequest, "文件过大或表单无效")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll() // 清理临时文件
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "缺少文件字段 file")
		return
	}
	defer f.Close()

	name := safeName(hdr.Filename)
	ext := strings.ToLower(filepath.Ext(name))
	if blockedExts[ext] {
		fail(w, http.StatusUnsupportedMediaType, "不允许上传该类型文件（网页/脚本类格式被禁止）")
		return
	}
	// 内容嗅探：非文本扩展名但内容是 HTML/SVG，拒绝（防止改扩展名绕过）
	if !textExts[ext] {
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		if n > 0 {
			ct := http.DetectContentType(head[:n])
			low := strings.ToLower(string(head[:n]))
			if strings.HasPrefix(ct, "text/html") || ct == "image/svg+xml" ||
				strings.Contains(low, "<svg") { // Go 嗅探器不认 <svg，手动补
				fail(w, http.StatusUnsupportedMediaType, "文件内容与类型不符（检测到网页/脚本内容）")
				return
			}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			fail(w, http.StatusInternalServerError, "读取文件失败")
			return
		}
	}
	dir := filepath.Join(noteDir(slug), filesDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(w, http.StatusInternalServerError, "无法创建文件目录")
		return
	}

	writeMu.Lock()
	defer writeMu.Unlock()

	// 单笔记总大小配额（防存储耗尽）：预检用客户端声明快速拒绝，写完后按实际大小复核
	var total int64
	if c.maxNoteTotal > 0 {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					if info, err := e.Info(); err == nil {
						total += info.Size()
					}
				}
			}
		}
		if total+hdr.Size > c.maxNoteTotal {
			fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("附件总大小超过上限（%d MB）", c.maxNoteTotal>>20))
			return
		}
	}

	// 同名文件自动追加序号：a.png -> a-1.png -> a-2.png（上限 999，防恶意预占拖长循环）
	final := name
	found := false
	for i := 1; i <= 999; i++ {
		if _, err := os.Stat(filepath.Join(dir, final)); os.IsNotExist(err) {
			found = true
			break
		}
		ext := filepath.Ext(name)
		final = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
	}
	if !found { // 极端情况兜底：时间戳命名，保证一定能落盘
		ext := filepath.Ext(name)
		final = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), time.Now().UnixMilli(), ext)
	}
	dst, err := os.OpenFile(filepath.Join(dir, final), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fail(w, http.StatusInternalServerError, "保存文件失败")
		return
	}
	if _, err := io.Copy(dst, f); err != nil {
		dst.Close()
		fail(w, http.StatusInternalServerError, "保存文件失败")
		return
	}
	dst.Close()
	// 配额复核：hdr.Size 是客户端自报的 multipart 声明、可伪造（报小写大），
	// 以落盘实际大小为准，超限即删除刚写入的文件
	if c.maxNoteTotal > 0 {
		p := filepath.Join(dir, final)
		if fi, err := os.Stat(p); err == nil && total+fi.Size() > c.maxNoteTotal {
			_ = os.Remove(p)
			fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("附件总大小超过上限（%d MB）", c.maxNoteTotal>>20))
			return
		}
	}
	touch(slug)

	log.Printf("upload: /%s -> %s", slug, final)
	writeJSON(w, http.StatusOK, map[string]string{
		"name": final,
		"url":  "/f/" + slug + "/" + final,
	})
}

// DELETE /api/{slug}/file/{name} —— 删除笔记的一个附件
func delFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	name := safeName(r.PathValue("name"))
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	// 受保护笔记：附件管理同样需要密码（评审 P1-1）
	if _, _, ok := noteAuth(w, r, slug); !ok {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := os.Remove(filepath.Join(noteDir(slug), filesDir, name)); err != nil {
		fail(w, http.StatusNotFound, "文件不存在")
		return
	}
	touch(slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// listFiles 列出笔记附件 [{name,size,modified,url}]
func listFiles(slug string) []map[string]any {
	entries, err := os.ReadDir(filepath.Join(noteDir(slug), filesDir))
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"name":     e.Name(),
			"size":     info.Size(),
			"modified": info.ModTime().Unix(),
			"url":      "/f/" + slug + "/" + e.Name(),
		})
	}
	return out
}

// ---------- 限速 ----------

// rateReader 令牌桶限速 Reader：以 rate 字节/秒 的平均速度放行读取，
// 突发上限为 1 秒的量，避免完全卡死小文件。
type rateReader struct {
	r       io.Reader
	rate    float64 // bytes/sec
	allowed float64
	last    time.Time
}

func (rr *rateReader) Read(p []byte) (int, error) {
	now := time.Now()
	rr.allowed += now.Sub(rr.last).Seconds() * rr.rate
	if rr.allowed > rr.rate { // 突发最多 1 秒量
		rr.allowed = rr.rate
	}
	rr.last = now

	max := int(rr.allowed)
	if max <= 0 {
		// 没有令牌：睡到攒够 1 字节再放行
		time.Sleep(time.Duration(float64(time.Second) / rr.rate))
		rr.allowed = 1
		max = 1
	}
	if max > len(p) {
		max = len(p)
	}
	n, err := rr.r.Read(p[:max])
	rr.allowed -= float64(n)
	return n, err
}

// Seek 委托给底层 ReadSeeker（http.ServeContent 处理 Range 断点续传时需要）
func (rr *rateReader) Seek(offset int64, whence int) (int64, error) {
	s, ok := rr.r.(io.Seeker)
	if !ok {
		return 0, fmt.Errorf("rateReader: 底层 Reader 不支持 Seek")
	}
	n, err := s.Seek(offset, whence)
	rr.allowed = 0
	rr.last = time.Now()
	return n, err
}

// maybeThrottle 按配置给 Reader 套限速（rateKBPS<=0 时原样返回）
func maybeThrottle(r io.ReadSeeker) io.ReadSeeker {
	kbps := cfg().rateKBPS
	if kbps <= 0 {
		return r
	}
	return &rateReader{r: r, rate: float64(kbps) * 1024, last: time.Now()}
}

// textExts 可在线预览的纯文本类扩展名（日志、配置、脚本等，服务端零解析，直接按文本流出）
var textExts = map[string]bool{
	".log": true, ".txt": true, ".text": true, ".out": true, ".err": true,
	".md": true, ".markdown": true, ".csv": true, ".tsv": true,
	".json": true, ".xml": true, ".yml": true, ".yaml": true, ".toml": true,
	".ini": true, ".conf": true, ".cfg": true, ".properties": true, ".env": true,
	".sh": true, ".bash": true, ".zsh": true, ".ps1": true, ".bat": true, ".cmd": true,
	".py": true, ".go": true, ".js": true, ".ts": true, ".java": true,
	".c": true, ".h": true, ".cpp": true, ".rs": true, ".sql": true, ".service": true,
}

// dispositionHeader 生成符合 RFC 6266/5987 的 Content-Disposition：
// ASCII 回退名 + UTF-8 百分号编码名，避免中文/引号文件名解析异常
func dispositionHeader(disp, name string) string {
	var ascii strings.Builder
	for _, r := range name {
		if r >= 32 && r < 127 && r != '"' && r != '\\' {
			ascii.WriteRune(r)
		} else {
			ascii.WriteByte('_')
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		disp, ascii.String(), url.PathEscape(name))
}

// GET /f/{slug}/{name} —— 提供附件下载/内嵌展示
func serveFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	name := safeName(r.PathValue("name"))
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	// 受保护笔记：附件访问同样需要密码（评审 P1-1）。
	// <img>/下载链接无法携带自定义请求头，允许 ?ntok= 短时令牌（解锁后经
	// getNote 或 /api/{slug}/ntok 签发，HMAC 绑定 slug+当前密码哈希+过期时间，
	// 防跨笔记重放；改密后旧令牌立即作废）。
	if m, _ := loadMeta(slug); m.PwdHash != "" {
		if !checkNoteToken(slug, m.PwdHash, r.URL.Query().Get("ntok")) {
			if _, _, ok := noteAuth(w, r, slug); !ok {
				return // noteAuth 已写好 401/429
			}
		}
	}
	p := filepath.Join(noteDir(slug), filesDir, name)
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	touch(slug)

	// 安全加固：附件沙箱化，禁止内嵌页面执行脚本
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")

	ext := strings.ToLower(filepath.Ext(name))
	ctype := mime.TypeByExtension(ext)
	disposition := "attachment"
	switch {
	case textExts[ext]:
		// 文本类：统一按 UTF-8 纯文本内嵌（浏览器零解析，服务端也只是流式转发）
		ctype = "text/plain; charset=utf-8"
		if c := cfg(); c.inlinePreviewKB > 0 && info.Size() <= int64(c.inlinePreviewKB)<<10 {
			disposition = "inline"
		}
	// SVG 可携带脚本/外部资源，一律强制下载；其余图片、音视频、PDF 可内嵌
	case strings.HasPrefix(ctype, "image/") && ext != ".svg" && ext != ".svgz",
		strings.HasPrefix(ctype, "audio/"),
		strings.HasPrefix(ctype, "video/"),
		ctype == "application/pdf":
		disposition = "inline"
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("Content-Disposition", dispositionHeader(disposition, name))

	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, name, info.ModTime(), maybeThrottle(f))
}

// ---------- 站点密码鉴权 ----------

// parseCIDRs 解析可信代理列表（支持裸 IP 与 CIDR）
func parseCIDRs(list []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			if ip.To4() != nil {
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)})
			} else {
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
			}
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, n)
		} else {
			log.Printf("config: cannot parse trusted_proxies entry %q, skipped", s)
		}
	}
	return nets
}

func ipTrusted(ip net.IP) bool {
	for _, n := range cfg().trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP 取客户端 IP：仅当直连来源是可信反代时才解析 X-Forwarded-For，
// 且取**最后一个**值（反代追加语义下才是真实客户端，靠前的值可被客户端伪造）
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ipTrusted(ip) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- { // 从右往左找第一个非可信 IP
				cand := strings.TrimSpace(parts[i])
				if c := net.ParseIP(cand); c != nil && ipTrusted(c) {
					continue
				}
				if cand != "" {
					return cand
				}
			}
		}
	}
	return host
}

const pbkdf2Iterations = 210000 // OWASP 推荐量级（PBKDF2-SHA256）

// pbkdf2Key 标准库实现的 PBKDF2-HMAC-SHA256（避免引入第三方依赖）
func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte {
	prf := func(data []byte) []byte {
		mac := hmac.New(sha256.New, password)
		mac.Write(data)
		return mac.Sum(nil)
	}
	var out []byte
	for block := 1; len(out) < keyLen; block++ {
		u := prf(append(append([]byte{}, salt...),
			byte(block>>24), byte(block>>16), byte(block>>8), byte(block)))
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			u = prf(u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// verifyHash 校验 "pbkdf2$iter$salt_hex$hash_hex" 格式的密码哈希
func verifyHash(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(want) < 16 {
		return false
	}
	got := sha256.Sum256(pbkdf2Key([]byte(password), salt, iter, len(want)))
	w := sha256.Sum256(want) // 先哈希成定长再比较，消除长度侧信道
	return subtle.ConstantTimeCompare(got[:], w[:]) == 1
}

// ---------- 单笔记密码（可选） ----------
//
// meta.json 里存 PwdHash（PBKDF2 验证哈希），正文以 AES-256-GCM 密文 blob 落盘——
// 磁盘泄露场景下受保护笔记的正文不可读，且认证标签保证密文不可被位翻转篡改（评审 P2-1）。
// 同一 PBKDF2 派生 64 字节：前 32 字节作验证哈希存 meta，后 32 字节作 AES 密钥（不落盘），
// 单次派生双用途。旧版 AES-256-CBC blob 仍可解（decryptBody 兼容读取，新写入一律 GCM）。
// 注意：密码经请求头明文传输（与登录同级别），公网部署需 HTTPS。

// deriveNoteKeys 从密码+盐派生 [验证哈希, AES 密钥] 各 32 字节
func deriveNoteKeys(password string, salt []byte) (verifyBytes, encKey []byte) {
	k := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, 64)
	return k[:32], k[32:]
}

// newPwdHash 生成 meta.pwd_hash 字段内容（随机盐 + pbkdf2$iter$salt_hex$hash_hex）
func newPwdHash(password string) (hash string, err error) {
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return
	}
	vb, _ := deriveNoteKeys(password, salt)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(vb)), nil
}

// encBlob 受保护笔记 note.md 的落盘格式（自包含：盐/nonce 随内容走）。
// GCM 新格式：salt/iv/data 走 base64（避免 hex 让密文体积翻倍）；旧 CBC 格式：hex。
type encBlob struct {
	Enc  string `json:"enc"` // "aes-256-gcm"（新）/ "aes-256-cbc"（旧，只读）
	Salt string `json:"salt"`
	IV   string `json:"iv"` // GCM 为 12 字节 nonce
	Data string `json:"data"`
}

func pkcs7Pad(b []byte, bs int) []byte {
	n := bs - len(b)%bs
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, bs int) ([]byte, error) {
	if len(b) == 0 || len(b)%bs != 0 {
		return nil, errors.New("填充数据无效")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > bs {
		return nil, errors.New("填充数据无效")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("填充数据无效")
		}
	}
	return b[:len(b)-n], nil
}

// encryptBody 用密码加密正文，返回可直接落盘的 blob JSON。
// AES-256-GCM：认证标签使密文不可被位翻转篡改（评审 P2-1）；
// base64 编码替代 hex，5MB 正文落盘体积约 6.7MB 而非 10.5MB（评审 P3）。
func encryptBody(password string, plain []byte) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	_, key := deriveNoteKeys(password, salt)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	return json.Marshal(encBlob{
		Enc:  "aes-256-gcm",
		Salt: base64.StdEncoding.EncodeToString(salt),
		IV:   base64.StdEncoding.EncodeToString(nonce),
		Data: base64.StdEncoding.EncodeToString(ct),
	})
}

// decryptBody 解密落盘 blob，返回正文明文。GCM 认证失败即报错——
// 密文被篡改不可能静默解出错误内容；旧版 AES-256-CBC 兼容读取。
func decryptBody(password string, blob []byte) ([]byte, error) {
	var e encBlob
	if err := json.Unmarshal(blob, &e); err != nil {
		return nil, errors.New("不是受保护笔记的正文格式")
	}
	switch e.Enc {
	case "aes-256-gcm":
		salt, err1 := base64.StdEncoding.DecodeString(e.Salt)
		nonce, err2 := base64.StdEncoding.DecodeString(e.IV)
		ct, err3 := base64.StdEncoding.DecodeString(e.Data)
		if err1 != nil || err2 != nil || err3 != nil ||
			len(salt) < 8 || len(nonce) != 12 || len(ct) < 16 {
			return nil, errors.New("加密数据无效")
		}
		_, key := deriveNoteKeys(password, salt)
		blk, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(blk)
		if err != nil {
			return nil, err
		}
		return gcm.Open(nil, nonce, ct, nil)
	case "aes-256-cbc": // 旧格式兼容读取（hex 编码）
		salt, err1 := hex.DecodeString(e.Salt)
		iv, err2 := hex.DecodeString(e.IV)
		ct, err3 := hex.DecodeString(e.Data)
		if err1 != nil || err2 != nil || err3 != nil || len(salt) < 8 || len(iv) != 16 || len(ct) == 0 {
			return nil, errors.New("加密数据无效")
		}
		_, key := deriveNoteKeys(password, salt)
		blk, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		pt := make([]byte, len(ct))
		cipher.NewCBCDecrypter(blk, iv).CryptBlocks(pt, ct)
		return pkcs7Unpad(pt, blk.BlockSize())
	default:
		return nil, errors.New("不是受保护笔记的正文格式")
	}
}

// noteTokTTL 附件短时令牌有效期：<img>/下载链接无法携带自定义请求头，
// 受保护笔记解锁后的页面改用 HMAC 令牌访问附件（绑定 slug + 过期时间）
const noteTokTTL = 15 * time.Minute

// noteAttachToken 签发附件短时令牌 "exp_hex.hmac_hex"，HMAC(authSecret, "ntok|slug|pwdHash|exp")。
// HMAC 消息掺入当前密码哈希：set/remove 密码后哈希变化，旧令牌立即全部作废
// （不依赖 15 分钟自然过期），避免换锁/共享电脑场景的残留窗口。
func noteAttachToken(slug, pwdHash string) string {
	exp := time.Now().Add(noteTokTTL).Unix()
	mac := hmac.New(sha256.New, authSecret)
	fmt.Fprintf(mac, "ntok|%s|%s|%d", slug, pwdHash, exp)
	return fmt.Sprintf("%x.%x", exp, mac.Sum(nil))
}

// checkNoteToken 校验附件短时令牌：未过期且 MAC 与 slug+当前密码哈希匹配
func checkNoteToken(slug, pwdHash, tok string) bool {
	if tok == "" {
		return false
	}
	i := strings.IndexByte(tok, '.')
	if i <= 0 {
		return false
	}
	exp, err := strconv.ParseInt(tok[:i], 16, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, authSecret)
	fmt.Fprintf(mac, "ntok|%s|%s|%d", slug, pwdHash, exp)
	got, err := hex.DecodeString(tok[i+1:])
	if err != nil || len(got) != mac.Size() {
		return false
	}
	return subtle.ConstantTimeCompare(mac.Sum(nil), got) == 1
}

// noteAuth 单笔记密码校验：受保护笔记要求请求头 X-Note-Password。
// 返回 (password, protected, ok)；ok=false 时已写好 401/429 响应。
// 失败尝试与站点登录共用一份 failIP 锁定计数，防在线爆破。
func noteAuth(w http.ResponseWriter, r *http.Request, slug string) (password string, protected bool, ok bool) {
	m, _ := loadMeta(slug)
	if m.PwdHash == "" {
		return "", false, true
	}
	ip := clientIP(r)
	if loginLocked(ip) {
		w.Header().Set("Retry-After", strconv.Itoa(cfg().lockMinutes*60))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试次数过多，请稍后再试"})
		return "", true, false
	}
	pw := r.Header.Get("X-Note-Password")
	if !verifyHash(pw, m.PwdHash) {
		recordLoginFail(ip)
		log.Printf("note /%s: wrong note password from %s", slug, ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"protected": true,
			"error":     "该笔记已设密码，请提供正确密码（请求头 X-Note-Password）",
		})
		return "", true, false
	}
	clearLoginFails(ip) // 密码验证成功即重置失败计数（与登录一致）
	return pw, true, true
}

// errNotePwdChanged 鉴权通过后、拿锁前密码被并发修改（换/删密码竞态），调用方应让客户端重新解锁
var errNotePwdChanged = errors.New("笔记密码已变更")

// prepareBodyForWrite 写入前处理（必须在 writeMu 锁内调用）：锁内重读 meta 判定保护状态，
// 受保护则校验 password 仍与当前哈希匹配——防止「鉴权时未设密码、拿锁前他人设了密码」
// 竞态导致明文落盘，或「他人换了密码」导致用旧密钥加密出永久解不开的数据（评审 P2-3）。
// 加密失败返回错误，调用方应拒绝写入——绝不能静默降级成明文落盘。
func prepareBodyForWrite(slug, password string, content []byte) ([]byte, error) {
	m, _ := loadMeta(slug)
	if m.PwdHash == "" {
		return content, nil
	}
	if !verifyHash(password, m.PwdHash) {
		return nil, errNotePwdChanged
	}
	return encryptBody(password, content)
}

func noteProtected(slug string) bool {
	m, ok := loadMeta(slug)
	return ok && m.PwdHash != ""
}

// checkPassword 校验登录密码（优先 password_hash；明文先哈希再恒定比较）
func checkPassword(input string) bool {
	c := cfg()
	if c.passwordHash != "" {
		return verifyHash(input, c.passwordHash)
	}
	a := sha256.Sum256([]byte(input))
	b := sha256.Sum256([]byte(c.sitePassword))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// newSessionToken 生成 "nonce.exp.mac" 会话令牌，并登记进服务端会话表
func newSessionToken(d time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(b)
	exp := time.Now().Add(d).Unix()
	mac := hmac.New(sha256.New, authSecret)
	fmt.Fprintf(mac, "%s|%d", nonce, exp)
	token := fmt.Sprintf("%s.%x.%x", nonce, exp, mac.Sum(nil))
	sessMu.Lock()
	sessions[nonce] = &sessInfo{exp: exp, last: time.Now().Unix()}
	sessMu.Unlock()
	return token, nil
}

// checkSession 校验会话令牌与服务端登记（只读，不刷新活跃时间）。
// /api/session 探测端点用它：前端轮询不会把空闲计时清零，
// 否则「超时弹回登录页」的看门狗自己就会把会话续下去。
func checkSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[1], 16, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	mac := hmac.New(sha256.New, authSecret)
	fmt.Fprintf(mac, "%s|%d", parts[0], expiry)
	got, err := hex.DecodeString(parts[2])
	if err != nil || len(got) != mac.Size() {
		return false
	}
	if subtle.ConstantTimeCompare(mac.Sum(nil), got) != 1 {
		return false
	}
	sessMu.Lock()
	info := sessions[parts[0]]
	ok := info != nil && info.exp == expiry
	if ok {
		// 空闲超时：超过 session_idle_minutes 无任何鉴权请求即吊销
		if idle := int64(cfg().sessionIdleMinutes) * 60; idle > 0 &&
			time.Now().Unix()-info.last > idle {
			delete(sessions, parts[0])
			ok = false
		}
	}
	sessMu.Unlock()
	// 会话必须存在于服务端表（退出/重启/清理后即吊销）
	return ok
}

// validSession 校验通过后刷新活跃时间（滑动空闲窗口）
func validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || !checkSession(r) {
		return false
	}
	nonce := strings.SplitN(c.Value, ".", 2)[0]
	sessMu.Lock()
	if info := sessions[nonce]; info != nil {
		info.last = time.Now().Unix()
	}
	sessMu.Unlock()
	return true
}

// requireAuth 包一层站点密码校验（未设密码时直接放行）
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg().authOn || validSession(r) {
			next(w, r)
			return
		}
		fail(w, http.StatusUnauthorized, "需要访问密码")
	}
}

func loginLocked(ip string) bool {
	failMu.Lock()
	defer failMu.Unlock()
	st := failIP[ip]
	return st != nil && time.Now().Before(st.until)
}

func recordLoginFail(ip string) {
	c := cfg()
	failMu.Lock()
	defer failMu.Unlock()
	st := failIP[ip]
	if st == nil {
		st = &failState{}
		failIP[ip] = st
	}
	st.count++
	st.last = time.Now()
	if st.count >= c.maxLoginAttempts {
		st.until = time.Now().Add(time.Duration(c.lockMinutes) * time.Minute)
		st.count = 0
	}
}

func clearLoginFails(ip string) {
	failMu.Lock()
	defer failMu.Unlock()
	delete(failIP, ip)
}

// POST /api/login {"password": "..."}
func login(w http.ResponseWriter, r *http.Request) {
	c := cfg()
	if !c.authOn {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求无效")
		return
	}
	ip := clientIP(r)
	if loginLocked(ip) {
		w.Header().Set("Retry-After", strconv.Itoa(c.lockMinutes*60))
		fail(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试次数过多，请 %d 分钟后再试", c.lockMinutes))
		return
	}
	if !checkPassword(body.Password) {
		recordLoginFail(ip)
		log.Printf("login failed: %s", ip)
		fail(w, http.StatusUnauthorized, "密码错误")
		return
	}
	clearLoginFails(ip)
	token, err := newSessionToken(time.Duration(c.sessionDays) * 24 * time.Hour)
	if err != nil {
		fail(w, http.StatusInternalServerError, "会话生成失败")
		return
	}
	// Secure：TLS 直连或可信反代声明了 X-Forwarded-Proto: https 时启用
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   cfg().sessionDays * 24 * 3600,
	})
	log.Printf("login ok: %s", ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/{slug}/password —— 单笔记密码管理 {"action":"set"|"remove","password":"..","current":".."}
// set：新密码 4–128 位；已设密码的笔记必须带正确的 current（正文旧密钥解密 → 新密钥重加密）。
// remove：必须带正确 current，正文解密还原明文并清除 meta 里的 pwd_hash。
// 失败尝试与站点登录共用 failIP 锁定计数。
func setNotePassword(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Action   string `json:"action"`
		Password string `json:"password"`
		Current  string `json:"current"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体 JSON 无效")
		return
	}
	if body.Action != "set" && body.Action != "remove" {
		fail(w, http.StatusBadRequest, "action 只能是 set 或 remove")
		return
	}
	ip := clientIP(r)
	if loginLocked(ip) {
		w.Header().Set("Retry-After", strconv.Itoa(cfg().lockMinutes*60))
		fail(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试次数过多，请 %d 分钟后再试", cfg().lockMinutes))
		return
	}

	m, _ := loadMeta(slug)
	protected := m.PwdHash != ""

	// 受保护笔记：两种操作都必须先验证当前密码
	if protected && !verifyHash(body.Current, m.PwdHash) {
		recordLoginFail(ip)
		log.Printf("note /%s: wrong current password (%s) from %s", slug, body.Action, ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"protected": true, "error": "当前密码错误"})
		return
	}

	// 读取现有正文：受保护 → 用当前密码解密；否则就是明文（可能不存在 = 空笔记设密码）
	var plain []byte
	var origBody []byte // 原始落盘字节（加密态），meta rename 失败时回滚用
	var hadBody bool
	if b, err := os.ReadFile(notePath(slug)); err == nil {
		hadBody = true
		origBody = b
		if protected {
			pt, derr := decryptBody(body.Current, b)
			if derr != nil {
				fail(w, http.StatusInternalServerError, "正文解密失败（数据损坏）")
				return
			}
			plain = pt
		} else {
			plain = b
		}
	}

	writeMu.Lock()
	defer writeMu.Unlock()

	// 事务化写入（评审 P1-2）：先写 meta.tmp（fsync）→ rename 正文 → rename meta；
	// 任一步失败即中止，meta rename 失败时回滚正文，杜绝
	// 「正文已换新密钥、meta 哈希还是旧的」这种永久不可读的不一致状态。
	rollback := func() {
		if !hadBody {
			if err := os.Remove(notePath(slug)); err != nil {
				log.Printf("note /%s: CRITICAL rollback remove failed: %v", slug, err)
			}
			return
		}
		tmp := notePath(slug) + ".tmp"
		if err := writeFileSync(tmp, origBody); err != nil {
			log.Printf("note /%s: CRITICAL rollback write failed: %v", slug, err)
			return
		}
		if err := os.Rename(tmp, notePath(slug)); err != nil {
			log.Printf("note /%s: CRITICAL rollback rename failed: %v", slug, err)
		}
	}

	if body.Action == "set" {
		if len(body.Password) < 4 || len(body.Password) > 128 {
			fail(w, http.StatusBadRequest, "密码长度需 4–128 位")
			return
		}
		// 正文用新密码重加密并原子写回（新笔记也创建，正文可为空）
		blob, eerr := encryptBody(body.Password, plain)
		if eerr != nil {
			fail(w, http.StatusInternalServerError, "正文加密失败")
			return
		}
		hash, herr := newPwdHash(body.Password)
		if herr != nil {
			fail(w, http.StatusInternalServerError, "密码哈希生成失败")
			return
		}
		m.PwdHash = hash
		if m.TTLHours == 0 { // 与 persistNote 同规则：0 会 ttlFor 被当无效值按 7 天，落成全局默认
			m.TTLHours = int(cfg().ttl.Hours())
		}
		mb, merr := json.Marshal(m)
		if merr != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		if err := os.MkdirAll(noteDir(slug), 0o700); err != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		metaTmp := metaPath(slug) + ".tmp"
		if err := writeFileSync(metaTmp, mb); err != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		tmp := notePath(slug) + ".tmp"
		if err := writeFileSync(tmp, blob); err != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		if err := os.Rename(tmp, notePath(slug)); err != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		if err := os.Rename(metaTmp, metaPath(slug)); err != nil {
			rollback() // 正文已是新密文但 meta 还是旧哈希 → 立即回滚正文
			fail(w, http.StatusInternalServerError, "写入失败（已回滚，请重试）")
			return
		}
		clearLoginFails(ip)
		reencryptHistory(slug, body.Current, true, body.Password) // 历史快照随新密码重加密（best-effort）
		log.Printf("note /%s: password set (body %d bytes)", slug, len(plain))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pwd": true})
		return
	}

	// remove：正文解密还原明文，清除 meta 哈希
	if !protected {
		fail(w, http.StatusBadRequest, "该笔记未设密码")
		return
	}
	m.PwdHash = ""
	if m.TTLHours == 0 {
		m.TTLHours = int(cfg().ttl.Hours())
	}
	mb, merr := json.Marshal(m)
	if merr != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	metaTmp := metaPath(slug) + ".tmp"
	if err := writeFileSync(metaTmp, mb); err != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	if hadBody {
		tmp := notePath(slug) + ".tmp"
		if err := writeFileSync(tmp, plain); err != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		if err := os.Rename(tmp, notePath(slug)); err != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
	}
	if err := os.Rename(metaTmp, metaPath(slug)); err != nil {
		rollback() // 正文已还原明文但 meta 还标着受保护 → 立即回滚正文
		fail(w, http.StatusInternalServerError, "写入失败（已回滚，请重试）")
		return
	}
	clearLoginFails(ip)
	reencryptHistory(slug, body.Current, false, "") // 历史快照随解除保护还原明文（best-effort）
	log.Printf("note /%s: password removed", slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pwd": false})
}

// ---------- 正文历史快照（软锁的「覆盖但可找回」兜底） ----------
//
// 强制覆盖（CLI/raw 写入、冲突弹窗里选「以我的版本覆盖」）前，把被覆盖的旧正文
// 原样复制进 history/ 子目录并登记 index.json；快照 blob 与正文同格式
// （受保护笔记 = 密文 blob，明文笔记 = 明文），密码变更时随正文一起重加密。

const (
	histDirName = "history" // 每条笔记目录下的历史子目录（随笔记过期一并删除）
	histMax     = 10        // 保留的最新快照数，超出按时间从旧到新淘汰
)

type histItem struct {
	Rev  uint64 `json:"rev"`  // 被覆盖时该正文的版本号
	TS   int64  `json:"ts"`   // 快照时间（Unix 秒）
	Size int    `json:"size"` // 落盘字节数（受保护笔记为密文大小，略大于明文）
	Enc  bool   `json:"enc"`  // true = blob 已按笔记密码加密（始终等于笔记当前保护状态）
	File string `json:"file"` // 历史子目录内的 blob 文件名
}

type histIndex struct {
	Items []histItem `json:"items"` // 按时间升序
}

func histDirPath(slug string) string        { return filepath.Join(noteDir(slug), histDirName) }
func histBlobPath(slug, name string) string { return filepath.Join(histDirPath(slug), name) }

func loadHistIndex(slug string) histIndex {
	var idx histIndex
	if b, err := os.ReadFile(filepath.Join(histDirPath(slug), "index.json")); err == nil {
		_ = json.Unmarshal(b, &idx)
	}
	return idx
}

// saveHistIndex 原子写历史索引（tmp + rename，权限 600）
func saveHistIndex(slug string, idx histIndex) error {
	if err := os.MkdirAll(histDirPath(slug), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(histDirPath(slug), "index.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(histDirPath(slug), "index.json"))
}

// snapshotHistory 把即将被覆盖的当前正文复制进历史（best-effort：
// 失败只记日志，绝不阻断保存）。旧正文与本次写入内容相同（CLI 反复管道
// 同一文件）时跳过——覆盖不丢失任何东西就不必留档。
// password 为当前笔记密码（未保护笔记为 ""），incoming 为本次写入的明文。
// 调用方需持有 writeMu。
func snapshotHistory(slug, password string, incoming []byte) {
	b, err := os.ReadFile(notePath(slug))
	if err != nil {
		return // 新笔记首次保存：没有旧正文可快照
	}
	m, _ := loadMeta(slug)
	oldPlain := b
	if m.PwdHash != "" {
		if pt, derr := decryptBody(password, b); derr != nil {
			// 旧正文解不开（理论不会发生）：仍原样留档，宁可多存不可丢失
			log.Printf("note /%s: history snapshot decrypt old body failed: %v", slug, derr)
		} else {
			oldPlain = pt
		}
	}
	if bytes.Equal(oldPlain, incoming) {
		return // 内容没变：覆盖零丢失，不刷历史
	}
	idx := loadHistIndex(slug)
	rev := m.Rev
	if rev == 0 {
		rev = 1 // 升级前遗留的正文：meta 还没有 rev，按 1 记
	}
	if err := os.MkdirAll(histDirPath(slug), 0o700); err != nil {
		log.Printf("note /%s: history mkdir failed: %v", slug, err)
		return
	}
	name := fmt.Sprintf("v%d_%d.bin", rev, time.Now().UnixNano())
	if err := os.WriteFile(histBlobPath(slug, name), b, 0o600); err != nil {
		log.Printf("note /%s: history snapshot write failed: %v", slug, err)
		return
	}
	idx.Items = append(idx.Items, histItem{
		Rev: rev, TS: time.Now().Unix(), Size: len(b), Enc: m.PwdHash != "", File: name,
	})
	pruneHistory(slug, &idx)
	if err := saveHistIndex(slug, idx); err != nil {
		log.Printf("note /%s: history index write failed: %v", slug, err)
	}
}

// pruneHistory 快照数超上限时从最旧的开始淘汰（连同 blob 文件一起删）
func pruneHistory(slug string, idx *histIndex) {
	if len(idx.Items) <= histMax {
		return
	}
	for i := 0; i < len(idx.Items)-histMax; i++ {
		_ = os.Remove(histBlobPath(slug, idx.Items[i].File))
	}
	idx.Items = idx.Items[len(idx.Items)-histMax:]
}

// reencryptHistory 密码变更后同步处理历史快照：改为受保护 → 全部用新密码加密；
// 解除保护 → 全部还原明文。best-effort：个别快照解密失败只记日志（该条将无法
// 读取，但不影响正文）。调用方需持有 writeMu 且正文/meta 已按新状态提交成功。
// oldPwd 为变更前密码（此前未设密码时不会被用到）；newProtected = 变更后是否受保护。
func reencryptHistory(slug, oldPwd string, newProtected bool, newPwd string) {
	idx := loadHistIndex(slug)
	if len(idx.Items) == 0 {
		return
	}
	changed := false
	for i := range idx.Items {
		it := &idx.Items[i]
		if !newProtected && !it.Enc {
			continue // 明文 → 明文：无事可做
		}
		// 注意：it.Enc 只标记保护状态，不标记密码版本——改密后（状态不变）
		// 旧快照仍是用旧密码加密的，必须解密重加密
		blob, err := os.ReadFile(histBlobPath(slug, it.File))
		if err != nil {
			continue
		}
		var plain []byte
		if it.Enc {
			if plain, err = decryptBody(oldPwd, blob); err != nil {
				log.Printf("note /%s: history v%d decrypt failed (skipped): %v", slug, it.Rev, err)
				continue
			}
		} else {
			plain = blob
		}
		var out []byte
		if newProtected {
			if out, err = encryptBody(newPwd, plain); err != nil {
				log.Printf("note /%s: history v%d encrypt failed (skipped): %v", slug, it.Rev, err)
				continue
			}
		} else {
			out = plain
		}
		tmp := histBlobPath(slug, it.File) + ".tmp"
		if err := os.WriteFile(tmp, out, 0o600); err != nil {
			log.Printf("note /%s: history v%d rewrite failed: %v", slug, it.Rev, err)
			continue
		}
		if err := os.Rename(tmp, histBlobPath(slug, it.File)); err != nil {
			log.Printf("note /%s: history v%d rename failed: %v", slug, it.Rev, err)
			continue
		}
		it.Enc = newProtected
		changed = true
	}
	if changed {
		if err := saveHistIndex(slug, idx); err != nil {
			log.Printf("note /%s: history index rewrite failed: %v", slug, err)
		}
	}
}

// GET /api/{slug}/history —— 列出历史快照（新→旧）：
//
//	curl http://host:8080/api/mynote/history
//
// 返回 {"ok":true,"items":[{"rev":3,"ts":1764000000,"size":1234},...]}
func noteHistory(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	if _, _, ok := noteAuth(w, r, slug); !ok {
		return
	}
	idx := loadHistIndex(slug)
	items := make([]map[string]any, 0, len(idx.Items))
	for i := len(idx.Items) - 1; i >= 0; i-- {
		it := idx.Items[i]
		items = append(items, map[string]any{"rev": it.Rev, "ts": it.TS, "size": it.Size})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

// GET /api/{slug}/history/{rev} —— 读取某条历史快照的明文内容（受保护笔记需带密码头）
func noteHistoryGet(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	password, _, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	rev, err := strconv.ParseUint(r.PathValue("rev"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "历史版本号无效")
		return
	}
	idx := loadHistIndex(slug)
	var hit *histItem
	for i := len(idx.Items) - 1; i >= 0; i-- { // 同 rev 取最新一条
		if idx.Items[i].Rev == rev {
			hit = &idx.Items[i]
			break
		}
	}
	if hit == nil {
		fail(w, http.StatusNotFound, "没有该版本的历史快照")
		return
	}
	blob, err := os.ReadFile(histBlobPath(slug, hit.File))
	if err != nil {
		fail(w, http.StatusInternalServerError, "历史快照读取失败")
		return
	}
	content := blob
	if hit.Enc {
		pt, derr := decryptBody(password, blob)
		if derr != nil {
			log.Printf("note /%s: history v%d decrypt failed: %v", slug, rev, derr)
			fail(w, http.StatusInternalServerError, "历史快照解密失败（密码可能在该快照产生后变更过）")
			return
		}
		content = pt
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rev": hit.Rev, "ts": hit.TS, "content": string(content)})
}

// GET /api/{slug}/ntok —— 签发附件短时令牌（受保护笔记解锁后供 <img>/下载链接使用）
func noteToken(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	_, protected, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	tok := ""
	if protected {
		m, _ := loadMeta(slug)
		tok = noteAttachToken(slug, m.PwdHash)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ntok": tok})
}

// POST /api/logout —— 注销：吊销服务端会话（不只是删 Cookie）
func logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if parts := strings.Split(c.Value, "."); len(parts) == 3 {
			sessMu.Lock()
			delete(sessions, parts[0])
			sessMu.Unlock()
		}
	}
	// Secure 跟随连接协议：纯 HTTP 部署下带 Secure 的清除 Cookie 会被浏览器拒收
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/{slug}/export —— 整包导出 zip（note.md + 附件）
func exportZip(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	np := notePath(slug)
	if _, err := os.Stat(np); err != nil {
		fail(w, http.StatusNotFound, "笔记不存在")
		return
	}
	password, protected, ok := noteAuth(w, r, slug)
	if !ok {
		return
	}
	touch(slug)

	// 正文先解密再写响应头（评审 P2-2）：解密失败以 500 干净中止，
	// 绝不在 zip 里悄悄放一个空的 note.md 让用户误以为正文为空。
	var bodySrc io.Reader
	if protected {
		b, rerr := os.ReadFile(np)
		if rerr != nil {
			fail(w, http.StatusInternalServerError, "读取正文失败")
			return
		}
		pt, derr := decryptBody(password, b)
		if derr != nil {
			log.Printf("note /%s: export decrypt failed: %v", slug, derr)
			fail(w, http.StatusInternalServerError, "正文解密失败（数据损坏）")
			return
		}
		bodySrc = bytes.NewReader(pt)
	} else {
		f, err := os.Open(np)
		if err != nil {
			fail(w, http.StatusInternalServerError, "读取正文失败")
			return
		}
		defer f.Close()
		bodySrc = f
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", dispositionHeader("attachment", slug+".zip"))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// 正文（受保护笔记导出解密后的明文；附件始终不加密）
	if zentry, err := zw.CreateHeader(&zip.FileHeader{Name: "note.md", Modified: time.Now()}); err == nil {
		_, _ = io.Copy(zentry, bodySrc)
	}
	// 附件
	if entries, err := os.ReadDir(filepath.Join(noteDir(slug), filesDir)); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			f, err := os.Open(filepath.Join(noteDir(slug), filesDir, e.Name()))
			if err != nil {
				continue
			}
			mod := time.Now()
			if info, err := e.Info(); err == nil {
				mod = info.ModTime()
			}
			if zentry, err := zw.CreateHeader(&zip.FileHeader{Name: "files/" + e.Name(), Modified: mod}); err == nil {
				_, _ = io.Copy(zentry, f)
			}
			f.Close()
		}
	}
}

// ---------- 文件互传（服务器纯中转） ----------
//
// 设计约束：服务器只做字节搬运，不解压、不校验、不运行数据包；
// zip 炸弹等风险的防御手段是「永不解包 + 实收字节数硬上限 + 扩展名白名单 + PK 魔数识别」。
// 上行/下行各一个全局字节令牌桶，速率每次从 cfg() 热读取，改配置即时生效。

const (
	transfersSub   = "transfers"   // dataDir 下互传中转目录
	trCodeLen      = 8             // 取件码长度（31 字符表 ≈ 39.7 bit 熵）
	trChunkSize    = 256 << 10     // 上传分块大小（客户端按此切片）
	trSubRead      = 32 << 10      // 服务端限速读取粒度
	trStalePartial = 6 * time.Hour // 未完成上传的保留时间（超时视为僵尸，清理）
)

var (
	upBucket   byteBucket // 全局上行桶：所有上传共享
	downBucket byteBucket // 全局下行桶：所有下载共享
)

// byteBucket 全局字节令牌桶：take 按 rate 放行 n 字节，速率由调用方每次传入（支持热加载）
type byteBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *byteBucket) take(rate float64, n int64) {
	if rate <= 0 || n <= 0 {
		return
	}
	for {
		b.mu.Lock()
		if b.last.IsZero() { // 首次使用给 1 秒突发量，避免小文件完全卡死
			b.tokens = rate
			b.last = time.Now()
		}
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * rate
		if b.tokens > rate { // 突发上限 1 秒量
			b.tokens = rate
		}
		b.last = now
		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return
		}
		wait := time.Duration((float64(n) - b.tokens) / rate * float64(time.Second))
		b.mu.Unlock()
		time.Sleep(wait)
	}
}

// downThrottle 全局限速 Seeker：包住中转文件交给 http.ServeContent（自动支持 Range 断点续传）
type downThrottle struct{ r io.ReadSeeker }

func (d *downThrottle) Read(p []byte) (int, error) {
	step := trSubRead
	if len(p) < step {
		step = len(p)
	}
	if rate := float64(cfg().trDownKBPS) * 1024; rate > 0 {
		downBucket.take(rate, int64(step))
	}
	return d.r.Read(p[:step])
}

func (d *downThrottle) Seek(offset int64, whence int) (int64, error) {
	return d.r.Seek(offset, whence)
}

// trMeta 一个中转会话的元数据
type trMeta struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`   // 声明的文件总大小（完成时与实际字节数校验）
	Client  string `json:"client"` // 发送端续传令牌（客户端随机生成）
	Ready   bool   `json:"ready"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Expires int64  `json:"expires"` // Ready 后的过期时间（Unix 秒）
}

func transfersRoot() string         { return filepath.Join(dataDir, transfersSub) }
func trBinPath(code string) string  { return filepath.Join(transfersRoot(), code+".bin") }
func trMetaPath(code string) string { return filepath.Join(transfersRoot(), code+".json") }

func fileSizeOf(p string) int64 {
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return fi.Size()
	}
	return -1
}

func loadTrMeta(code string) (*trMeta, bool) {
	b, err := os.ReadFile(trMetaPath(code))
	if err != nil {
		return nil, false
	}
	var m trMeta
	if err := json.Unmarshal(b, &m); err != nil || m.Code == "" {
		return nil, false
	}
	return &m, true
}

func saveTrMeta(m *trMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(trMetaPath(m.Code), b, 0o600)
}

// trStorageUsed 中转目录当前占用（.bin 总字节数）
func trStorageUsed() int64 {
	entries, err := os.ReadDir(transfersRoot())
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".bin") {
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
	}
	return total
}

// findTrByClient 按发送端令牌找未完成的会话（断点续传入口）
func findTrByClient(token string) *trMeta {
	entries, err := os.ReadDir(transfersRoot())
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if m, ok := loadTrMeta(strings.TrimSuffix(e.Name(), ".json")); ok && !m.Ready && m.Client == token {
			return m
		}
	}
	return nil
}

// POST /api/transfer/init —— 创建或续接一个发送会话
// 请求 {"name":"x.zip","size":123456,"token":"<客户端随机hex>"}
// 返回 {"code":"ab12cd34","chunk":262144,"offset":0}（offset=服务端已收字节数，续传起点）
func trInit(w http.ResponseWriter, r *http.Request) {
	c := cfg()
	if !c.transferEnabled {
		fail(w, http.StatusForbidden, "文件互传未启用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求无效")
		return
	}
	if body.Size <= 0 {
		fail(w, http.StatusBadRequest, "文件大小无效")
		return
	}
	if body.Size > c.trMaxFile {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("文件超过中转上限（%d MB）", c.trMaxFile>>20))
		return
	}
	name := safeName(body.Name)
	ext := strings.ToLower(filepath.Ext(name))
	allowed := false
	for _, e := range c.trExts {
		if ext == e {
			allowed = true
			break
		}
	}
	if !allowed {
		fail(w, http.StatusUnsupportedMediaType,
			"仅允许中转以下类型："+strings.Join(c.trExts, " "))
		return
	}
	if len(body.Token) < 16 || len(body.Token) > 128 {
		fail(w, http.StatusBadRequest, "会话令牌无效")
		return
	}
	if _, err := hex.DecodeString(body.Token); err != nil {
		fail(w, http.StatusBadRequest, "会话令牌无效")
		return
	}

	// 续传：同令牌的未完成会话直接续上
	if m := findTrByClient(body.Token); m != nil {
		if cur := fileSizeOf(trBinPath(m.Code)); cur >= 0 {
			m.Updated = time.Now().Unix()
			_ = saveTrMeta(m)
			writeJSON(w, http.StatusOK, map[string]any{"code": m.Code, "chunk": trChunkSize, "offset": cur})
			return
		}
	}

	// 存储配额（防中转目录被塞爆）
	if c.trMaxStorage > 0 && trStorageUsed()+body.Size > c.trMaxStorage {
		fail(w, http.StatusInsufficientStorage, "服务器中转空间已满，请稍后再试")
		return
	}

	var code string
	for i := 0; i < 5; i++ {
		c2, err := randCode(trCodeLen)
		if err != nil {
			fail(w, http.StatusInternalServerError, "取件码生成失败")
			return
		}
		if _, err := os.Stat(trMetaPath(c2)); os.IsNotExist(err) {
			code = c2
			break
		}
	}
	if code == "" {
		fail(w, http.StatusInternalServerError, "取件码生成失败")
		return
	}
	now := time.Now().Unix()
	m := &trMeta{Code: code, Name: name, Size: body.Size, Client: body.Token,
		Created: now, Updated: now, Expires: now + int64(trStalePartial.Seconds())}
	f, err := os.OpenFile(trBinPath(code), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fail(w, http.StatusInternalServerError, "创建中转会话失败")
		return
	}
	f.Close()
	if err := saveTrMeta(m); err != nil {
		_ = os.Remove(trBinPath(code))
		fail(w, http.StatusInternalServerError, "创建中转会话失败")
		return
	}
	log.Printf("transfer: created %s (%s, %d bytes)", code, name, body.Size)
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "chunk": trChunkSize, "offset": int64(0)})
}

// PUT /api/transfer/{code}/chunk?offset=N —— 追加一块数据（限速读取，全局上行桶）
//
// trCodeLocks：每个取件码一把锁。同一 code 的「读进度 → 判 offset → 流式写 → 记 meta」
// 全程串行——否则发送端重试的旧请求与续传的新请求可同时通过 off==cur 判定，
// 交错 WriteAt 把同一偏移区间覆写撕裂（complete 的总字节数校验发现不了内容错乱）。
// 慢发送端最长占用 2 分钟（读超时），只影响自己这个 code。
var trCodeLocks sync.Map // code -> *sync.Mutex

func trCodeLock(code string) *sync.Mutex {
	v, _ := trCodeLocks.LoadOrStore(code, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func trChunk(w http.ResponseWriter, r *http.Request) {
	c := cfg()
	if !c.transferEnabled {
		fail(w, http.StatusForbidden, "文件互传未启用")
		return
	}
	code := r.PathValue("code")
	mu := trCodeLock(code)
	mu.Lock()
	defer mu.Unlock()
	// 锁内重读 meta：complete/delete 可能刚持有同一把锁改过状态
	m, ok := loadTrMeta(code)
	if !ok || m.Ready {
		fail(w, http.StatusNotFound, "取件码不存在或已完成传输")
		return
	}
	cur := fileSizeOf(trBinPath(m.Code))
	if cur < 0 {
		fail(w, http.StatusNotFound, "取件码不存在")
		return
	}
	off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if off > cur {
		// 客户端进度超前（服务端可能被清理过）：把真实进度告诉它
		writeJSON(w, http.StatusConflict, map[string]any{"error": "offset ahead of server", "offset": cur})
		return
	}
	if off < cur {
		// 重复投递：幂等返回当前进度
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "offset": cur})
		return
	}
	if c.trMaxFile-cur <= 0 {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("文件超过中转上限（%d MB）", c.trMaxFile>>20))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, c.trMaxFile-cur+1)

	// 流式落盘：小粒度读 + 逐块 WriteAt，请求体不再整体攒在内存里
	// （恶意客户端可直接 PUT 50MB，N 个并发请求就是 N×50MB 内存放大）。
	// writeMu 只在单块 WriteAt 时短暂持有，不阻塞期间的其他写入操作。
	// 限速读取：全局桶 pacing，TCP 背压自然把发送端压到限额
	rc := http.NewResponseController(w)
	writeMu.Lock()
	f, ferr := os.OpenFile(trBinPath(m.Code), os.O_WRONLY, 0o600)
	writeMu.Unlock()
	if ferr != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	defer f.Close()
	tmp := make([]byte, trSubRead)
	var written int64

	// PK 魔数校验：先聚齐前 2 字节（网络分片可能让首个 Read 只返回 1 字节），
	// 校验通过后把这 2 字节一并落盘，主循环从 written 处续写
	if cur == 0 {
		var pk [2]byte
		got := 0
		for got < 2 {
			if rate := float64(cfg().trUpKBPS) * 1024; rate > 0 {
				upBucket.take(rate, trSubRead)
			}
			_ = rc.SetReadDeadline(time.Now().Add(2 * time.Minute))
			n, rerr := r.Body.Read(pk[got:])
			got += n
			if rerr != nil {
				break // EOF（体不足 2 字节）或读错误：PK 判定自然拒绝
			}
		}
		if !bytes.HasPrefix(pk[:got], []byte("PK")) {
			fail(w, http.StatusUnsupportedMediaType, "文件内容不是有效的压缩包（缺少 PK 头）")
			return
		}
		writeMu.Lock()
		_, werr := f.WriteAt(pk[:got], off)
		writeMu.Unlock()
		if werr != nil {
			fail(w, http.StatusInternalServerError, "写入失败")
			return
		}
		written = int64(got)
	}

	for {
		if rate := float64(cfg().trUpKBPS) * 1024; rate > 0 {
			upBucket.take(rate, trSubRead)
		}
		_ = rc.SetReadDeadline(time.Now().Add(2 * time.Minute)) // 每次读取续期，卡死连接 2 分钟内断开
		n, rerr := r.Body.Read(tmp)
		if n > 0 {
			writeMu.Lock()
			_, werr := f.WriteAt(tmp[:n], off+written)
			writeMu.Unlock()
			if werr != nil {
				fail(w, http.StatusInternalServerError, "写入失败")
				return
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			fail(w, http.StatusBadRequest, "读取数据失败")
			return
		}
	}

	writeMu.Lock()
	m.Updated = time.Now().Unix()
	werr := saveTrMeta(m)
	writeMu.Unlock()
	if werr != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "offset": off + written})
}

// POST /api/transfer/{code}/complete —— 结束发送：校验字节数后对外可见
func trComplete(w http.ResponseWriter, r *http.Request) {
	c := cfg()
	if !c.transferEnabled {
		fail(w, http.StatusForbidden, "文件互传未启用")
		return
	}
	code := r.PathValue("code")
	// 与 trChunk 同一把 per-code 锁：流式写未结束时不允许翻转 Ready
	mu := trCodeLock(code)
	mu.Lock()
	defer mu.Unlock()
	m, ok := loadTrMeta(code)
	if !ok || m.Ready {
		fail(w, http.StatusNotFound, "取件码不存在或已完成传输")
		return
	}
	cur := fileSizeOf(trBinPath(m.Code))
	if cur != m.Size {
		fail(w, http.StatusBadRequest,
			fmt.Sprintf("传输未完成（已收 %d / %d 字节），请继续发送", cur, m.Size))
		return
	}
	writeMu.Lock()
	m.Ready = true
	m.Expires = time.Now().Unix() + int64(c.trExpire.Seconds())
	err := saveTrMeta(m)
	writeMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, "保存失败")
		return
	}
	log.Printf("transfer: ready %s (%s, %d bytes, expires in %v)", m.Code, m.Name, m.Size, c.trExpire)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": m.Code, "expires": m.Expires})
}

// GET /api/transfer —— 列出待接收的文件（同站自己人互相可见）
func trList(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(transfersRoot())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{}})
		return
	}
	now := time.Now().Unix()
	items := []map[string]any{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		m, ok := loadTrMeta(strings.TrimSuffix(e.Name(), ".json"))
		if !ok || !m.Ready || now > m.Expires {
			continue
		}
		items = append(items, map[string]any{
			"code": m.Code, "name": m.Name, "size": m.Size,
			"created": m.Created, "expires": m.Expires,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GET /api/transfer/info?code=xxx —— 查询取件码对应的文件信息
// （刻意不用 /api/transfer/{code}：与 /api/{slug}/export 这类路由在 ServeMux 里无法判定优先级）
func trManifest(w http.ResponseWriter, r *http.Request) {
	m, ok := loadTrMeta(strings.TrimSpace(r.URL.Query().Get("code")))
	if !ok {
		fail(w, http.StatusNotFound, "取件码不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": m.Code, "name": m.Name, "size": m.Size,
		"ready": m.Ready, "expires": m.Expires,
	})
}

// GET /api/transfer/download?code=xxx —— 下载（ServeContent 提供 Range 断点续传，全局下行桶限速）
func trDownload(w http.ResponseWriter, r *http.Request) {
	m, ok := loadTrMeta(strings.TrimSpace(r.URL.Query().Get("code")))
	if !ok || !m.Ready || time.Now().Unix() > m.Expires { // 与 trList 同口径：过期不可下载
		fail(w, http.StatusNotFound, "取件码不存在、未完成或已过期")
		return
	}
	p := trBinPath(m.Code)
	info, err := os.Stat(p)
	if err != nil {
		fail(w, http.StatusNotFound, "文件不存在")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取失败")
		return
	}
	defer f.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", dispositionHeader("attachment", m.Name))
	http.ServeContent(w, r, m.Name, info.ModTime(), &downThrottle{r: f})
}

// DELETE /api/transfer/{code} —— 撤回一个中转文件
func trDelete(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	// 与 trChunk 同一把 per-code 锁：流式写进行中不允许删除（Windows 下
	// 打开中的 fd 还会让 RemoveAll 失败）；锁序 per-code → writeMu 与其他入口一致
	mu := trCodeLock(code)
	mu.Lock()
	defer mu.Unlock()
	writeMu.Lock()
	defer writeMu.Unlock()
	_, ok := loadTrMeta(code)
	if !ok {
		fail(w, http.StatusNotFound, "取件码不存在或已过期")
		return
	}
	_ = os.Remove(trBinPath(code))
	_ = os.Remove(trMetaPath(code))
	trCodeLocks.Delete(code) // 释放 per-code 锁条目，防 map 随 code 数量无界增长
	log.Printf("transfer: deleted %s (by user)", code)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// cleanTransfers 清理过期中转文件（janitor 调用）：
// Ready 且超过 Expires → 删；未 Ready 超过 trStalePartial 没有新数据 → 删；孤儿 .bin → 删
func cleanTransfers() {
	entries, err := os.ReadDir(transfersRoot())
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".bin") {
			if _, err := os.Stat(trMetaPath(strings.TrimSuffix(name, ".bin"))); err != nil {
				_ = os.Remove(filepath.Join(transfersRoot(), name))
				log.Printf("transfer: removed orphan blob %s", name)
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		m, ok := loadTrMeta(strings.TrimSuffix(name, ".json"))
		if !ok {
			continue
		}
		expired := m.Ready && now > m.Expires
		stale := !m.Ready && now-m.Updated > int64(trStalePartial.Seconds())
		if expired || stale {
			writeMu.Lock()
			_ = os.Remove(trBinPath(m.Code))
			_ = os.Remove(trMetaPath(m.Code))
			writeMu.Unlock()
			trCodeLocks.Delete(m.Code) // 释放 per-code 锁条目（正在等锁的请求会在锁内重读 meta 时 404）
			reason := "expired"
			if stale {
				reason = "stale partial"
			}
			log.Printf("transfer: removed %s (%s)", m.Code, reason)
		}
	}
}

// ---------- 页面路由 ----------

// staticVer 静态资源版本号：取关键文件的内容哈希。发版后 URL 自动变化，
// 于是 /static/ 可以放心用一年强缓存（immutable），回访时零请求。
var staticVer = func() string {
	h := sha256.New()
	for _, p := range []string{
		"static/style.css", "static/app.js",
		"static/js/marked.min.js", "static/js/purify.min.js",
	} {
		if b, err := fs.ReadFile(staticFS, p); err == nil {
			_, _ = h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// 两版页面在启动时预渲染（弱主机上避免每请求做十几次字符串替换）：
// 两页都内联 CSS（渲染不再等外链样式表的第二个 RTT），head 里的
// preload 让 app.js 与 HTML 剩余部分并行下载；
// home —— 额外裁掉 markdown 库相关内容；editor —— 保留。
var (
	pageHome   = buildPage(false)
	pageEditor = buildPage(true)
)

// cspInlineStyle 内联 CSS 的 CSP 白名单项（'sha256-...'）；CSS 读取失败时为空
// （页面退回外链样式表，style-src 只剩 'self'，功能不受损）
var cspInlineStyle string

// pageCSP 页面专属 CSP：style-src 放行本页内联样式块（哈希白名单），其余严格同源。
// 覆盖 securityHeaders 中间件的默认值；必须在 ETag 304 分支之前就设置——
// 否则 304 响应会带着中间件的 style-src 'self' 落进浏览器缓存条目，
// 缓存复用时内联 <style> 被拦截，整页裸奔（reload 强制重校验，必现）。
func pageCSP() string {
	styleSrc := "'self'"
	if cspInlineStyle != "" {
		styleSrc += " " + cspInlineStyle
	}
	return "default-src 'self'; img-src 'self' data:; script-src 'self'; " +
		"style-src " + styleSrc + "; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}

// buildPage 按页面准备 HTML：内联压缩后的样式表，首页裁掉用不到的脚本。
func buildPage(editor bool) []byte {
	raw, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		return nil
	}
	page := strings.ReplaceAll(string(raw), "__V__", staticVer)
	if css, err := fs.ReadFile(staticFS, "static/style.css"); err == nil {
		min := minifyCSS(string(css))
		link := `<link rel="stylesheet" href="/static/style.css?v=` + staticVer + `">`
		page = strings.Replace(page, link, "<style>"+min+"</style>", 1)
		sum := sha256.Sum256([]byte(min))
		cspInlineStyle = "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	if !editor {
		// 首页不解析 Markdown，marked + DOMPurify（约 27 KB）纯属浪费带宽
		page = dropBetween(page, "<!--EDITOR_SCRIPTS_START-->", "<!--EDITOR_SCRIPTS_END-->")
		page = dropBetween(page, "<!--EDITOR_PRELOAD_START-->", "<!--EDITOR_PRELOAD_END-->")
	}
	return []byte(page)
}

// minifyCSS 构建期压缩样式表（仅内联进页面用；/static/ 下原文件保持可读）。
// 本项目的样式表不含形如 "/*" 的字符串字面量，直接按标记删除注释是安全的。
func minifyCSS(css string) string {
	for {
		i := strings.Index(css, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(css[i:], "*/")
		if j < 0 {
			css = css[:i]
			break
		}
		css = css[:i] + css[i+j+2:]
	}
	css = strings.Join(strings.Fields(css), " ")
	for _, sep := range []string{"{", "}", ";", ",", ":", ">"} {
		css = strings.ReplaceAll(css, " "+sep, sep)
		css = strings.ReplaceAll(css, sep+" ", sep)
	}
	return css
}

// dropBetween 删除 start 与 end 标记之间的内容（含标记）；标记缺失时原样返回。
func dropBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return s
	}
	return s[:i] + s[i+j+len(end):]
}

// bootCap 服务端内嵌正文的字节上限：超过则退回接口加载（避免大笔记拖慢首屏）
const bootCap = 256 << 10

// escapeHTMLRCData 为 <textarea> RCDATA 上下文转义：& < > 三个字符足够，
// 浏览器读取 .value 时会自动还原。
func escapeHTMLRCData(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// GET / 与 GET /{slug} 都返回同一个前端页面（单页应用，由 JS 区分首页/编辑器）
func pageHandler(w http.ResponseWriter, r *http.Request) {
	editor := false
	slug := ""
	if r.URL.Path != "/" {
		slug = strings.TrimPrefix(r.URL.Path, "/")
		if !validSlug(slug) {
			http.Error(w, "404 页面不存在（笔记地址仅允许字母、数字、- 和 _，最长 64 字符）", http.StatusNotFound)
			return
		}
		editor = true
	}
	tpl := pageHome
	ttl := "public, max-age=600, stale-while-revalidate=86400" // 首页壳体几乎不变
	if editor {
		tpl = pageEditor
		// 编辑器页内嵌正文，缓存窗口收紧：改动后最多 1 分钟内浏览器仍用本地副本
		ttl = "public, max-age=60, stale-while-revalidate=600"
		// 服务端预切骨架：编辑器页直接显示编辑器、隐藏首页，首屏绘制不依赖 app.js
		tpl = bytes.Replace(tpl, []byte(`<main id="editor" class="hidden">`), []byte(`<main id="editor">`), 1)
		tpl = bytes.Replace(tpl, []byte(`<main id="home">`), []byte(`<main id="home" class="hidden">`), 1)
		// 已存在、未启用站点密码且未设单笔记密码的笔记：正文直接内嵌进 textarea，
		// 首屏即见全文，省掉「脚本就绪 → 接口往返」两跳。
		// 启用站点密码或单笔记密码时绝不能内嵌，否则绕过鉴权。
		content := ""
		if !cfg().authOn && !noteProtected(slug) {
			if b, err := os.ReadFile(notePath(slug)); err == nil && len(b) > 0 && len(b) <= bootCap {
				content = escapeHTMLRCData(string(b))
			}
		}
		tpl = bytes.ReplaceAll(tpl, []byte("__BOOT__"), []byte(content))
	} else {
		tpl = bytes.ReplaceAll(tpl, []byte("__BOOT__"), nil)
	}
	if len(tpl) == 0 {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	// 页面壳体可缓存：回访直接读磁盘缓存（stale-while-revalidate 后台刷新），
	// LCP 趋近 0；/static/ 资源按内容哈希强缓存，旧壳体 + 新资源也能正常解析。
	w.Header().Set("Cache-Control", ttl)
	// ETag 基于壳体 + 内嵌正文（不含每请求变化的头），后台重校验时命中 304 零传输
	sum := sha256.Sum256(tpl)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	// CSP 必须在 304 分支之前覆写中间件默认值：304 的头会被浏览器并入缓存条目
	w.Header().Set("Content-Security-Policy", pageCSP())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(tpl)
}

// GET /api/session —— 前端会话探测（需登录）。
// 只读校验、不刷新活跃时间，因此轮询它不会把空闲计时清零；
// 前端以此实现「空闲超时 → 弹回登录页并提示重新登录」。
func sessionProbe(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"idle_minutes": cfg().sessionIdleMinutes,
	})
}

// GET /api/health —— 健康检查（始终开放，auth 告诉前端是否需要登录）
func health(w http.ResponseWriter, _ *http.Request) {
	c := cfg()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"auth":        c.authOn,
		"max_note_mb": c.maxNote >> 20,
		"max_file_mb": c.maxFile >> 20,
		"transfer": map[string]any{
			"enabled":     c.transferEnabled,
			"max_file_mb": c.trMaxFile >> 20,
			"up_kbps":     c.trUpKBPS,
			"down_kbps":   c.trDownKBPS,
			"exts":        c.trExts,
		},
	})
}

// ---------- 过期清理 ----------

func janitor() {
	cleanOnce() // 启动时先清一轮
	cleanTransfers()
	cleanupRuntime()
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		cleanOnce()
		cleanTransfers()
		cleanupRuntime()
	}
}

// cleanupRuntime 老化内存状态：过期会话、闲置令牌桶、过期登录失败记录
func cleanupRuntime() {
	now := time.Now().Unix()
	idle := int64(cfg().sessionIdleMinutes) * 60
	sessMu.Lock()
	for nonce, v := range sessions {
		if now > v.exp || (idle > 0 && now-v.last > idle) {
			delete(sessions, nonce)
		}
	}
	sessMu.Unlock()

	limMu.Lock()
	for ip, b := range limitBucks {
		if time.Since(b.last) > 15*time.Minute {
			delete(limitBucks, ip)
		}
	}
	limMu.Unlock()

	failMu.Lock()
	for ip, st := range failIP {
		if now > st.until.Unix() && time.Since(st.last) > 30*time.Minute {
			delete(failIP, ip)
		}
	}
	failMu.Unlock()
}

func cleanOnce() {
	entries, err := os.ReadDir(notesRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		dir := filepath.Join(notesRoot(), slug)
		np := filepath.Join(dir, noteFile)
		info, err := os.Stat(np)
		if err != nil {
			// 没有 note.md 的纯附件目录：按目录 mtime + 该笔记自身 meta 的 TTL 判断
			// （与正文笔记同一口径，避免“先拖附件未存正文”的笔记提前被全局默认清掉）
			if di, err2 := e.Info(); err2 == nil && time.Since(di.ModTime()) > ttlFor(slug) {
				log.Printf("cleanup: removed empty dir %s", slug)
				_ = os.RemoveAll(dir)
			}
			continue
		}
		noteTTL := ttlFor(slug)
		if time.Since(info.ModTime()) > noteTTL {
			// 与写入操作互斥，避免删到正在保存的笔记
			writeMu.Lock()
			if fi, err := os.Stat(np); err == nil && time.Since(fi.ModTime()) > noteTTL {
				log.Printf("cleanup: removed /%s (idle over %v)", slug, noteTTL)
				_ = os.RemoveAll(dir)
			}
			writeMu.Unlock()
		}
	}
}

// ---------- 安全响应头 & 全局限速中间件 ----------

// gzipWriterPool 复用 gzip.Writer，降低弱主机上的分配开销
var gzipWriterPool = sync.Pool{New: func() any { w := gzip.NewWriter(nil); w.Reset(&noopWriter{}); return w }}

type noopWriter struct{}

func (noopWriter) Write(b []byte) (int, error) { return len(b), nil }

// gzWriter 按响应内容类型决定是否压缩：文本类（HTML/CSS/JS/JSON/SVG）压缩，
// 二进制（zip、图片、附件）直通；实现在首次 Write 时依 Content-Type 判定。
type gzWriter struct {
	http.ResponseWriter
	poolWriter *gzip.Writer
	eligible   bool // 客户端接受 gzip
	active     bool // 本响应实际在压缩
	wroteHead  bool
}

func (g *gzWriter) decide() {
	if g.wroteHead {
		return
	}
	g.wroteHead = true
	ct := g.Header().Get("Content-Type")
	textual := strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "image/svg")
	if g.eligible && textual {
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		g.poolWriter.Reset(g.ResponseWriter)
		g.active = true
	}
}

func (g *gzWriter) WriteHeader(code int) {
	// ServeContent 等处理器会显式调用 WriteHeader，之后响应头即冻结——
	// 压缩判定必须在此之前完成
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzWriter) Write(b []byte) (int, error) {
	g.decide()
	if g.active {
		return g.poolWriter.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzWriter) Flush() {
	if g.active {
		g.poolWriter.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// gzipMiddleware 对页面 / 静态资源 / 普通 JSON API 做 gzip 压缩。
// 明确排除：带 Range 的请求（断点续传语义不可破坏）与互传下载（zip 已压缩且走 ServeContent）。
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		seg := strings.Trim(p, "/")
		// 单段路径 = 笔记页（/、/{slug}）也要压缩：不压缩时编辑器页 35KB 裸传，
		// 弱网下光下载就要 2 秒多——这是首页与编辑器耗时差一倍的真正原因。
		pagePath := p == "/" || (!strings.Contains(seg, "/") && slugRe.MatchString(seg))
		eligible := strings.HasPrefix(p, "/static/") || pagePath ||
			(strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/api/transfer/download"))
		if !eligible || r.Header.Get("Range") != "" ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		g := &gzWriter{ResponseWriter: w, poolWriter: gzipWriterPool.Get().(*gzip.Writer), eligible: true}
		defer func() {
			if g.active {
				g.poolWriter.Close()
			}
			gzipWriterPool.Put(g.poolWriter)
		}()
		next.ServeHTTP(g, r)
	})
}

// securityHeaders 为所有响应补齐安全头（页面 CSP 收紧到同源；/f/ 附件由处理器覆盖为 sandbox）
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; script-src 'self'; style-src 'self'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer") // 笔记地址即凭据，禁止外泄给第三方站点
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=()")
		p := r.URL.Path
		if strings.HasPrefix(p, "/static/") {
			// 资源 URL 带内容哈希（?v=...），发版后 URL 变化自动失效旧缓存，
			// 因此可以长期强缓存：回访零请求，弱网下省掉 4 个 RTT
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimit 每 IP 全局令牌桶限速（防暴力扫描与资源耗尽）
func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rps := cfg().rateRPS
		ip := clientIP(r)
		now := time.Now()
		limMu.Lock()
		b := limitBucks[ip]
		if b == nil {
			if len(limitBucks) > 100000 { // 兜底防内存膨胀
				limitBucks = map[string]*limiter{}
			}
			b = &limiter{tokens: rps, last: now}
			limitBucks[ip] = b
		}
		b.tokens += now.Sub(b.last).Seconds() * rps
		if b.tokens > rps*2 { // 突发上限 2 秒量
			b.tokens = rps * 2
		}
		b.last = now
		ok := b.tokens >= 1
		if ok {
			b.tokens--
		}
		limMu.Unlock()
		if !ok {
			w.Header().Set("Retry-After", "2")
			fail(w, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- 入口 ----------

func main() {
	// sznote -hash <密码>：生成 PBKDF2 哈希，填入 config.json 的 server.password_hash
	if len(os.Args) > 1 && os.Args[1] == "-hash" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sznote -hash <password>")
			os.Exit(2)
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			log.Fatal("failed to generate salt: ", err)
		}
		key := pbkdf2Key([]byte(os.Args[2]), salt, pbkdf2Iterations, 32)
		fmt.Printf("pbkdf2$%d$%s$%s\n", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key))
		return
	}

	loadConfig(true)
	start := cfg()

	// 启动安全检查：监听全部网卡却不设密码时，必须显式声明接受匿名访问
	if !start.authOn && !start.allowAnonymous {
		log.Printf("security check failed: no server.password / server.password_hash / SZN_PASSWORD set")
		log.Fatal("refusing to start: listening on all interfaces without an access password. " +
			"Set server.password (or server.password_hash), " +
			"or set security.allow_anonymous=true in config.json (trusted LAN only).\n" +
			"Steps: 1) run ./sznote -hash 'your-password'; " +
			"2) put the output into server.password_hash in config.json; 3) start again.\n" +
			"No config.json? Copy config.example.json next to the binary and rename it to config.json.")
	}

	if err := os.MkdirAll(notesRoot(), 0o700); err != nil {
		log.Fatalf("failed to create data dir %s: %v", notesRoot(), err)
	}
	if err := os.MkdirAll(transfersRoot(), 0o700); err != nil {
		log.Fatalf("failed to create transfers dir %s: %v", transfersRoot(), err)
	}
	if start.authOn {
		authSecret = make([]byte, 32)
		if _, err := rand.Read(authSecret); err != nil {
			log.Fatal("failed to generate session secret: ", err)
		}
		if start.passwordHash != "" {
			log.Printf("auth enabled (PBKDF2 hash, session %d days)", start.sessionDays)
		} else {
			log.Printf("auth enabled (plaintext password, session %d days)", start.sessionDays)
		}
	}
	if start.authOn {
		if start.sessionIdleMinutes > 0 {
			log.Printf("  session idle timeout: %d min (re-login required after idle)", start.sessionIdleMinutes)
		} else {
			log.Printf("  session idle timeout: off")
		}
	}
	if !start.authOn {
		log.Printf("!!! anonymous mode (allow_anonymous=true): anyone can read/write all notes, do NOT expose to the public internet !!!")
	}

	mux := http.NewServeMux()
	// 静态文件：禁用目录索引，拒绝路径穿越
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(strings.TrimPrefix(r.URL.Path, "/static/"))
		if p == "." || strings.HasPrefix(p, "..") || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, staticFS, "static/"+p)
	})
	mux.HandleFunc("GET /api/health", health)
	mux.HandleFunc("POST /api/login", login)
	mux.HandleFunc("POST /api/logout", logout)
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		if cfg().authOn && !checkSession(r) {
			fail(w, http.StatusUnauthorized, "需要访问密码")
			return
		}
		sessionProbe(w, r)
	})
	mux.HandleFunc("GET /api/{slug}", requireAuth(getNote))
	mux.HandleFunc("PUT /api/{slug}", requireAuth(saveNote))
	// 纯文本 API（curl / Linux CLI 友好）：/api/{slug}/raw 读写正文，
	// PUT /{slug} 为写入路径别名（读取不设 /{slug}/raw 别名——与 /static/ 子树路由冲突），
	// 详见 getNoteRaw/saveNoteRaw 注释
	mux.HandleFunc("GET /api/{slug}/raw", requireAuth(getNoteRaw))
	mux.HandleFunc("PUT /api/{slug}/raw", requireAuth(saveNoteRaw))
	mux.HandleFunc("POST /api/{slug}/raw", requireAuth(saveNoteRaw))
	mux.HandleFunc("PUT /{slug}", requireAuth(saveNoteRaw))
	mux.HandleFunc("POST /api/{slug}/upload", requireAuth(upload))
	mux.HandleFunc("POST /api/{slug}/password", requireAuth(setNotePassword))
	mux.HandleFunc("GET /api/{slug}/export", requireAuth(exportZip))
	mux.HandleFunc("GET /api/{slug}/ntok", requireAuth(noteToken))
	mux.HandleFunc("GET /api/{slug}/history", requireAuth(noteHistory))
	mux.HandleFunc("GET /api/{slug}/history/{rev}", requireAuth(noteHistoryGet))
	mux.HandleFunc("DELETE /api/{slug}/file/{name}", requireAuth(delFile))
	mux.HandleFunc("POST /api/transfer/init", requireAuth(trInit))
	mux.HandleFunc("GET /api/transfer", requireAuth(trList))
	mux.HandleFunc("GET /api/transfer/info", requireAuth(trManifest))
	mux.HandleFunc("PUT /api/transfer/{code}/chunk", requireAuth(trChunk))
	mux.HandleFunc("POST /api/transfer/{code}/complete", requireAuth(trComplete))
	mux.HandleFunc("GET /api/transfer/download", requireAuth(trDownload))
	mux.HandleFunc("DELETE /api/transfer/{code}", requireAuth(trDelete))
	mux.HandleFunc("GET /f/{slug}/{name}", requireAuth(serveFile))
	mux.HandleFunc("GET /{$}", pageHandler)
	mux.HandleFunc("GET /{slug}", pageHandler)

	go janitor()
	go configWatcher()

	log.Printf("szNote started: http://0.0.0.0:%s  config=%s", port, configPath)
	log.Printf("  data dir=%s  default ttl=%v  note limit=%dMB  file limit=%dMB  per-note total=%dMB",
		dataDir, start.ttl, start.maxNote>>20, start.maxFile>>20, start.maxNoteTotal>>20)
	if start.rateKBPS > 0 {
		log.Printf("  download throttle=%d KB/s per connection (0=off)  text preview limit=%dKB", start.rateKBPS, start.inlinePreviewKB)
	} else {
		log.Printf("  download throttle=off  text preview limit=%dKB", start.inlinePreviewKB)
	}
	log.Printf("  global rate=%.0f req/s/IP  trusted proxies=%d  hot reload=on (port/data_dir need restart)", start.rateRPS, len(start.trustedNets))
	if start.transferEnabled {
		up, down := "off", "off"
		if start.trUpKBPS > 0 {
			up = fmt.Sprintf("%dKB/s", start.trUpKBPS)
		}
		if start.trDownKBPS > 0 {
			down = fmt.Sprintf("%dKB/s", start.trDownKBPS)
		}
		log.Printf("  file relay: on  limit=%dMB  up=%s down=%s (global)  expire=%v  exts=%v",
			start.trMaxFile>>20, up, down, start.trExpire, start.trExts)
	} else {
		log.Printf("  file relay: off")
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           securityHeaders(gzipMiddleware(rateLimit(mux))),
		ReadHeaderTimeout: 10 * time.Second, // 防 Slowloris
		ReadTimeout:       10 * time.Minute, // 覆盖大文件慢速上传
		WriteTimeout:      0,                // 大文件限速下载可能很慢，交给 IdleTimeout
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// configWatcher 每 3 秒检查一次 config.json 是否变化，变化则重新加载。
// port / data_dir / 监听地址不可热加载（需重启）；其余参数即时生效。
func configWatcher() {
	var lastMod int64
	var lastSize int64
	if fi, err := os.Stat(configPath); err == nil {
		lastMod, lastSize = fi.ModTime().UnixNano(), fi.Size()
	}
	for range time.Tick(3 * time.Second) {
		fi, err := os.Stat(configPath)
		if err != nil {
			continue
		}
		m, s := fi.ModTime().UnixNano(), fi.Size()
		if m == lastMod && s == lastSize {
			continue
		}
		lastMod, lastSize = m, s

		old := cfg()
		if !loadConfig(false) { // 解析失败：保留旧配置，等下一次修改
			continue
		}
		neu := cfg()
		if old == neu {
			continue
		}
		log.Printf("config reloaded: ttl=%v note=%dMB file=%dMB preview=%dKB throttle=%dKB/s rate=%.0frps relay=%v(up=%dKB/s down=%dKB/s) auth=%v anon=%v (port/data_dir unchanged, restart to apply)",
			neu.ttl, neu.maxNote>>20, neu.maxFile>>20, neu.inlinePreviewKB, neu.rateKBPS, neu.rateRPS,
			neu.transferEnabled, neu.trUpKBPS, neu.trDownKBPS, neu.authOn, neu.allowAnonymous)
	}
}
