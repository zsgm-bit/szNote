// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (c) 2026 szNote authors <https://github.com/zsgm-bit/szNote>
// 双轨授权：AGPL-3.0（见 LICENSE）或商业许可（见 COMMERCIAL-LICENSE.md）。

package main

// szNote —— 类 note.ms 的自托管网页笔记服务（前后端一体，单二进制）
// 功能：短链/自定义别名笔记、Markdown 渲染、图片粘贴/文件上传、24h 无人访问自动删除
// 仅使用 Go 标准库，无第三方依赖。

import (
	"archive/zip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
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
	"time"
)

//go:embed static
var staticFS embed.FS

const (
	notesSub = "notes"                            // dataDir 下存放各笔记的子目录名
	noteFile = "note.md"                          // 笔记正文文件名
	metaFile = "meta.json"                        // 笔记元数据（自定义有效期）
	filesDir = "files"                            // 每条笔记的附件目录名
	alphabet = "abcdefghjkmnpqrstuvwxyz23456789"  // 随机短链字母表（去掉易混淆字符）
	slugLen  = 10                                 // 随机地址长度（31 字符表 ≈ 49.5 bit 熵）

	sessionCookie = "qn_session"

	maxTTLHours = 168 // 笔记最长有效期：7 天（0 = 永久，单独判断）
)

// fileConfig 对应 config.json 的结构。字段留空/省略时使用内置默认值。
type fileConfig struct {
	Server struct {
		Port         flexInt `json:"port"` // 允许 9720 或 "9720" 两种写法
		Password     string  `json:"password"`
		PasswordHash string  `json:"password_hash"` // pbkdf2$iter$salt$hash（sznote -hash 生成），优先于明文
		SessionDays  int     `json:"session_days"`
	} `json:"server"`
	Storage struct {
		DataDir         string `json:"data_dir"`
		MaxNoteMB       int    `json:"max_note_mb"`
		MaxFileMB       int    `json:"max_file_mb"`
		InlinePreviewKB int    `json:"inline_preview_kb"`
		MaxNoteTotalMB  int    `json:"max_note_total_mb"` // 单笔记（含附件）总大小上限，0=不限制
	} `json:"storage"`
	Policy struct {
		DefaultTTLHours int `json:"default_ttl_hours"`
		RateKBPS        int `json:"rate_kbps"`
	} `json:"policy"`
	Security struct {
		MaxLoginAttempts int      `json:"max_login_attempts"`
		LockMinutes      int      `json:"lock_minutes"`
		TrustedProxies   []string `json:"trusted_proxies"`   // 可信反代 IP/CIDR，命中才解析 X-Forwarded-For
		RateRPS          int      `json:"rate_rps"`          // 每 IP 全局请求速率上限（req/s），0=内置默认 50
		AllowAnonymous   bool     `json:"allow_anonymous"`   // 显式接受"无密码匿名访问"风险
	} `json:"security"`
}

var (
	port    = "9720"
	dataDir = "./data"
	ttl     = 24 * time.Hour
	maxNote = int64(5) << 20  // 笔记正文上限，默认 5MB
	maxFile = int64(50) << 20 // 单文件上限，默认 50MB
	// 文本类附件在线预览的大小上限（KB），超过则改为下载
	inlinePreviewKB = 512
	// 每连接下载限速（KB/s），0 = 不限速
	rateKBPS = 100

	slugRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	reserved = map[string]bool{"static": true, "api": true, "f": true, "favicon.ico": true}

	writeMu sync.Mutex // 序列化所有写操作，避免并发写坏文件

	sitePassword = "" // 站点密码（明文），与 password_hash 二选一
	passwordHash = "" // 站点密码（PBKDF2 哈希），优先于明文
	authOn       bool // 是否启用鉴权（password / password_hash 任一非空）
	authSecret   []byte
	sessionDays  = 7 // 登录会话有效期（天）

	maxLoginAttempts = 5  // 连续失败多少次锁 IP
	lockMinutes      = 10 // 锁定时长（分钟）

	maxNoteTotal = int64(200) << 20 // 单笔记含附件总大小上限
	rateRPS      = 50.0             // 每 IP 全局请求速率（req/s）

	allowAnonymous = false // 显式接受无密码匿名访问

	trustedNets []*net.IPNet // 可信反代网段，命中才信任 X-Forwarded-For

	configPath = "config.json" // 实际加载的配置文件路径（日志用）

	// 登录失败锁定状态（带时间戳，可被 janitor 老化清理）
	failMu sync.Mutex
	failIP = map[string]*failState{}

	// 服务端会话表：nonce -> 过期时间（支持退出即吊销）
	sessMu     sync.Mutex
	sessions   = map[string]int64{}
	limMu      sync.Mutex
	limitBucks = map[string]*limiter{}
)

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
	cfg.Storage.DataDir = "./data"
	cfg.Storage.MaxNoteMB = 5
	cfg.Storage.MaxFileMB = 50
	cfg.Storage.InlinePreviewKB = 512
	cfg.Storage.MaxNoteTotalMB = 200
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
func loadConfig() {
	def := defaultConfig()
	cfg := def

	// 配置文件位置：SZN_CONFIG（或旧名 QN_CONFIG），默认工作目录下 config.json
	path := envOr("SZN_CONFIG", "config.json")
	configPath = path
	if b, err := os.ReadFile(path); err == nil {
		tmp := def // 先填默认值再解 JSON，缺字段就保留默认
		if err := json.Unmarshal(b, &tmp); err != nil {
			log.Printf("配置: %s 解析失败，全部改用内置默认值（%v）", path, err)
		} else {
			cfg = tmp
			log.Printf("配置: 已加载 %s", path)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("配置: 读取 %s 失败，改用内置默认值（%v）", path, err)
	}

	// 应用到运行时变量
	port = strconv.Itoa(int(cfg.Server.Port))
	sitePassword = cfg.Server.Password
	passwordHash = strings.TrimSpace(cfg.Server.PasswordHash)
	sessionDays = cfg.Server.SessionDays
	dataDir = cfg.Storage.DataDir
	maxNote = int64(cfg.Storage.MaxNoteMB) << 20
	maxFile = int64(cfg.Storage.MaxFileMB) << 20
	inlinePreviewKB = cfg.Storage.InlinePreviewKB
	maxNoteTotal = int64(cfg.Storage.MaxNoteTotalMB) << 20
	ttl = time.Duration(cfg.Policy.DefaultTTLHours) * time.Hour
	rateKBPS = cfg.Policy.RateKBPS
	maxLoginAttempts = cfg.Security.MaxLoginAttempts
	lockMinutes = cfg.Security.LockMinutes
	allowAnonymous = cfg.Security.AllowAnonymous
	if cfg.Security.RateRPS > 0 {
		rateRPS = float64(cfg.Security.RateRPS)
	}
	trustedNets = parseCIDRs(cfg.Security.TrustedProxies)

	// 环境变量覆盖（旧版 QN_ 前缀同样兼容）
	if v := envOr("SZN_PORT", ""); v != "" {
		port = v
	}
	if v := envOr("SZN_PASSWORD", ""); v != "" {
		sitePassword = v
	}
	if n, ok := envIntSet("SZN_ALLOW_ANONYMOUS"); ok {
		allowAnonymous = n != 0
	}
	if v := envOr("SZN_DATA", ""); v != "" {
		dataDir = v
	}
	if n, ok := envIntSet("SZN_SESSION_DAYS"); ok {
		sessionDays = n
	}
	if n, ok := envIntSet("SZN_MAX_NOTE_MB"); ok {
		maxNote = int64(n) << 20
	} else if n, ok := envIntSet("SZN_MAX_NOTE_KB"); ok { // 旧变量
		maxNote = int64(n) << 10
	}
	if n, ok := envIntSet("SZN_MAX_FILE_MB"); ok {
		maxFile = int64(n) << 20
	}
	if n, ok := envIntSet("SZN_INLINE_PREVIEW_KB"); ok {
		inlinePreviewKB = n
	}
	if n, ok := envIntSet("SZN_TTL_HOURS"); ok {
		ttl = time.Duration(n) * time.Hour
	}
	if n, ok := envIntSet("SZN_RATE_KBPS"); ok {
		rateKBPS = n
	}
	if n, ok := envIntSet("SZN_MAX_LOGIN_ATTEMPTS"); ok {
		maxLoginAttempts = n
	}
	if n, ok := envIntSet("SZN_LOCK_MINUTES"); ok {
		lockMinutes = n
	}

	// 兜底：避免 0 值把服务搞成不可用
	if !validTTL(int(ttl.Hours())) {
		log.Printf("配置: default_ttl_hours=%d 不在 24/72/120/168 之内，改用 24", int(ttl.Hours()))
		ttl = 24 * time.Hour
	}
	if maxNote <= 0 {
		maxNote = 5 << 20
	}
	if maxFile <= 0 {
		maxFile = 50 << 20
	}
	if sessionDays <= 0 {
		sessionDays = 7
	}
	if maxLoginAttempts <= 0 {
		maxLoginAttempts = 5
	}
	if lockMinutes <= 0 {
		lockMinutes = 10
	}
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

func validSlug(slug string) bool {
	return slugRe.MatchString(slug) && !reserved[slug]
}

// noteMeta 每条笔记的元数据。TTLHours: 24/72/120/168（即 1/3/5/7 天）；文件不存在 = 使用全局默认
// Plain: true 表示纯文本模式（前端不解析 Markdown，适合贴日志/命令）
type noteMeta struct {
	TTLHours int  `json:"ttl_hours"`
	Plain    bool `json:"plain,omitempty"`
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
	return ttl // 全局默认 QN_TTL_HOURS
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

// touch 更新笔记的“最后使用时间”（以 note.md 的修改时间记账，重启不丢）
func touch(slug string) {
	p := notePath(slug)
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
}

// randSlug 生成随机短地址：rejection sampling 消除取模偏置（CWE-338/341）
func randSlug() (string, error) {
	out := make([]byte, 0, slugLen)
	buf := make([]byte, slugLen*2)
	max := byte(256 - 256%len(alphabet)) // 248，保证均匀
	for len(out) < slugLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, v := range buf {
			if v >= max {
				continue // 拒绝采样，避免前几个字符概率偏高
			}
			out = append(out, alphabet[int(v)%len(alphabet)])
			if len(out) == slugLen {
				break
			}
		}
	}
	return string(out), nil
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
	content, exists := "", false
	if b, err := os.ReadFile(notePath(slug)); err == nil {
		content, exists = string(b), true
		touch(slug)
	}
	ttlHours := int(ttlFor(slug).Hours())
	writeJSON(w, http.StatusOK, map[string]any{
		"slug":      slug,
		"exists":    exists,
		"content":   content,
		"files":     listFiles(slug),
		"ttl_hours": ttlHours,
		"plain":     plainFor(slug),
	})
}

// PUT /api/{slug} —— 保存笔记正文 {"content": "...", "ttl_hours": 24, "plain": false}
// ttl_hours: 只能是 24/72/120/168；缺省 = 不修改（新笔记用全局默认）
// plain: 缺省 = 不修改
func saveNote(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusNotFound, "无效的笔记地址")
		return
	}
	// JSON 转义会放大体积，请求体上限给正文上限的 2 倍 + 64KB 余量，
	// 真正的正文上限在解码后按字符数严格校验
	r.Body = http.MaxBytesReader(w, r.Body, maxNote*2+(1<<16))
	var body struct {
		Content  string `json:"content"`
		TTLHours *int   `json:"ttl_hours"`
		Plain    *bool  `json:"plain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusRequestEntityTooLarge, "请求体无效或超过大小限制")
		return
	}
	if int64(len(body.Content)) > maxNote {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("正文超过上限（%d MB）", maxNote>>20))
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
	dir := noteDir(slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(w, http.StatusInternalServerError, "无法创建笔记目录")
		return
	}
	tmp := notePath(slug) + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.Content), 0o600); err != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	if err := os.Rename(tmp, notePath(slug)); err != nil {
		fail(w, http.StatusInternalServerError, "写入失败")
		return
	}
	if body.TTLHours != nil || body.Plain != nil {
		m, _ := loadMeta(slug) // 合并写入，避免覆盖另一项设置
		if body.TTLHours != nil {
			m.TTLHours = *body.TTLHours
		}
		if body.Plain != nil {
			m.Plain = *body.Plain
		}
		if m.TTLHours == 0 { // 还没设过有效期：落成全局默认，避免 0 被当成无效值
			m.TTLHours = int(ttl.Hours())
		}
		if mb, err := json.Marshal(m); err == nil {
			_ = os.WriteFile(metaPath(slug), mb, 0o600)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	r.Body = http.MaxBytesReader(w, r.Body, maxFile+(1<<20))
	// 内存阈值固定 8MB，超出部分自动落临时文件，避免并发上传放大内存（CWE-400）
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

	// 单笔记总大小配额（防存储耗尽，CWE-400）
	if maxNoteTotal > 0 {
		var total int64
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					if info, err := e.Info(); err == nil {
						total += info.Size()
					}
				}
			}
		}
		if total+hdr.Size > maxNoteTotal {
			fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("附件总大小超过上限（%d MB）", maxNoteTotal>>20))
			return
		}
	}

	// 同名文件自动追加序号：a.png -> a-1.png -> a-2.png
	final := name
	for i := 1; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, final)); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(name)
		final = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
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
	touch(slug)

	log.Printf("上传: /%s -> %s", slug, final)
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
	rate    float64   // bytes/sec
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
	if rateKBPS <= 0 {
		return r
	}
	return &rateReader{r: r, rate: float64(rateKBPS) * 1024, last: time.Now()}
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
		if inlinePreviewKB > 0 && info.Size() <= int64(inlinePreviewKB)<<10 {
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
			log.Printf("配置: trusted_proxies 中无法解析 %q，已忽略", s)
		}
	}
	return nets
}

func ipTrusted(ip net.IP) bool {
	for _, n := range trustedNets {
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

// checkPassword 校验登录密码（优先 password_hash；明文先哈希再恒定比较）
func checkPassword(input string) bool {
	if passwordHash != "" {
		return verifyHash(input, passwordHash)
	}
	a := sha256.Sum256([]byte(input))
	b := sha256.Sum256([]byte(sitePassword))
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
	sessions[nonce] = exp
	sessMu.Unlock()
	return token, nil
}

func validSession(r *http.Request) bool {
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
	registered := sessions[parts[0]]
	sessMu.Unlock()
	// 会话必须存在于服务端表（退出/重启/清理后即吊销）
	return registered == expiry
}

// requireAuth 包一层站点密码校验（未设密码时直接放行）
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authOn || validSession(r) {
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
	failMu.Lock()
	defer failMu.Unlock()
	st := failIP[ip]
	if st == nil {
		st = &failState{}
		failIP[ip] = st
	}
	st.count++
	st.last = time.Now()
	if st.count >= maxLoginAttempts {
		st.until = time.Now().Add(time.Duration(lockMinutes) * time.Minute)
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
	if !authOn {
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
		w.Header().Set("Retry-After", strconv.Itoa(lockMinutes*60))
		fail(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试次数过多，请 %d 分钟后再试", lockMinutes))
		return
	}
	if !checkPassword(body.Password) {
		recordLoginFail(ip)
		log.Printf("登录失败: %s", ip)
		fail(w, http.StatusUnauthorized, "密码错误")
		return
	}
	clearLoginFails(ip)
	token, err := newSessionToken(time.Duration(sessionDays) * 24 * time.Hour)
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
		MaxAge:   sessionDays * 24 * 3600,
	})
	log.Printf("登录成功: %s", ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
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
	touch(slug)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", dispositionHeader("attachment", slug+".zip"))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// 正文
	if f, err := os.Open(np); err == nil {
		if zentry, err := zw.CreateHeader(&zip.FileHeader{Name: "note.md", Modified: time.Now()}); err == nil {
			_, _ = io.Copy(zentry, f)
		}
		f.Close()
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


// ---------- 页面路由 ----------

// GET / 与 GET /{slug} 都返回同一个前端页面（单页应用，由 JS 区分首页/编辑器）
func pageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		slug := strings.TrimPrefix(r.URL.Path, "/")
		if !validSlug(slug) {
			http.Error(w, "404 页面不存在（笔记地址仅允许字母、数字、- 和 _，最长 64 字符）", http.StatusNotFound)
			return
		}
	}
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// GET /api/health —— 健康检查（始终开放，auth 告诉前端是否需要登录）
func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"auth":        authOn,
		"max_note_mb": maxNote >> 20,
		"max_file_mb": maxFile >> 20,
	})
}

// ---------- 过期清理 ----------

func janitor() {
	cleanOnce() // 启动时先清一轮
	cleanupRuntime()
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		cleanOnce()
		cleanupRuntime()
	}
}

// cleanupRuntime 老化内存状态：过期会话、闲置令牌桶、过期登录失败记录
func cleanupRuntime() {
	now := time.Now().Unix()
	sessMu.Lock()
	for nonce, exp := range sessions {
		if now > exp {
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
			// 没有 note.md 的空壳目录：按目录自身 mtime 判断
			if di, err2 := e.Info(); err2 == nil && time.Since(di.ModTime()) > ttl {
				log.Printf("清理: 移除空目录 %s", slug)
				_ = os.RemoveAll(dir)
			}
			continue
		}
		noteTTL := ttlFor(slug)
		if time.Since(info.ModTime()) > noteTTL {
			// 与写入操作互斥，避免删到正在保存的笔记（CWE-362）
			writeMu.Lock()
			if fi, err := os.Stat(np); err == nil && time.Since(fi.ModTime()) > noteTTL {
				log.Printf("清理: 笔记 /%s 超过 %v 无人访问，已删除", slug, noteTTL)
				_ = os.RemoveAll(dir)
			}
			writeMu.Unlock()
		}
	}
}

// ---------- 安全响应头 & 全局限速中间件 ----------

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
		if strings.HasPrefix(p, "/api/") || !strings.HasPrefix(p, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimit 每 IP 全局令牌桶限速（防暴力扫描与资源耗尽）
func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		now := time.Now()
		limMu.Lock()
		b := limitBucks[ip]
		if b == nil {
			if len(limitBucks) > 100000 { // 兜底防内存膨胀
				limitBucks = map[string]*limiter{}
			}
			b = &limiter{tokens: rateRPS, last: now}
			limitBucks[ip] = b
		}
		b.tokens += now.Sub(b.last).Seconds() * rateRPS
		if b.tokens > rateRPS*2 { // 突发上限 2 秒量
			b.tokens = rateRPS * 2
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
			fmt.Fprintln(os.Stderr, "用法: sznote -hash <密码>")
			os.Exit(2)
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			log.Fatal("生成盐失败: ", err)
		}
		key := pbkdf2Key([]byte(os.Args[2]), salt, pbkdf2Iterations, 32)
		fmt.Printf("pbkdf2$%d$%s$%s\n", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key))
		return
	}

	loadConfig()
	authOn = sitePassword != "" || passwordHash != ""

	// 启动安全检查：监听全部网卡却不设密码时，必须显式声明接受匿名访问（CWE-306）
	if !authOn && !allowAnonymous {
		log.Printf("安全检查未通过：未设置 server.password / server.password_hash / SZN_PASSWORD")
		log.Fatal("拒绝启动：当前将监听所有网卡但无访问密码。请设置密码（推荐），" +
			"或在 config.json 的 security.allow_anonymous 设为 true（仅限完全可信的内网）")
	}

	if err := os.MkdirAll(notesRoot(), 0o700); err != nil {
		log.Fatalf("无法创建数据目录 %s: %v", notesRoot(), err)
	}
	if authOn {
		authSecret = make([]byte, 32)
		if _, err := rand.Read(authSecret); err != nil {
			log.Fatal("生成会话密钥失败: ", err)
		}
		if passwordHash != "" {
			log.Printf("站点密码保护已启用（PBKDF2 哈希校验，会话有效期 %d 天）", sessionDays)
		} else {
			log.Printf("站点密码保护已启用（会话有效期 %d 天）", sessionDays)
		}
	}
	if !authOn {
		log.Printf("!!! 匿名访问模式（allow_anonymous=true）：任何人可读写全部笔记，切勿暴露到公网 !!!")
	}

	mux := http.NewServeMux()
	// 静态文件：禁用目录索引（CWE-548），拒绝路径穿越
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
	mux.HandleFunc("GET /api/{slug}", requireAuth(getNote))
	mux.HandleFunc("PUT /api/{slug}", requireAuth(saveNote))
	mux.HandleFunc("POST /api/{slug}/upload", requireAuth(upload))
	mux.HandleFunc("GET /api/{slug}/export", requireAuth(exportZip))
	mux.HandleFunc("DELETE /api/{slug}/file/{name}", requireAuth(delFile))
	mux.HandleFunc("GET /f/{slug}/{name}", requireAuth(serveFile))
	mux.HandleFunc("GET /{$}", pageHandler)
	mux.HandleFunc("GET /{slug}", pageHandler)

	go janitor()

	log.Printf("szNote 已启动: http://0.0.0.0:%s  配置=%s", port, configPath)
	log.Printf("  数据目录=%s  默认有效期=%v  正文上限=%dMB  单文件上限=%dMB  笔记总上限=%dMB",
		dataDir, ttl, maxNote>>20, maxFile>>20, maxNoteTotal>>20)
	if rateKBPS > 0 {
		log.Printf("  附件下载限速=%d KB/s（0 = 不限速）  文本预览上限=%dKB", rateKBPS, inlinePreviewKB)
	} else {
		log.Printf("  附件下载限速=关闭  文本预览上限=%dKB", inlinePreviewKB)
	}
	log.Printf("  全局限速=%.0f req/s/IP  可信反代=%d 条", rateRPS, len(trustedNets))

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           securityHeaders(rateLimit(mux)),
		ReadHeaderTimeout: 10 * time.Second,   // 防 Slowloris（CWE-400）
		ReadTimeout:       10 * time.Minute,   // 覆盖大文件慢速上传
		WriteTimeout:      0,                  // 大文件限速下载可能很慢，交给 IdleTimeout
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
