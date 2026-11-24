/* szNote 前端逻辑（无框架，原生 JS） */
"use strict";

/* slug 解析容错：畸形转义序列不再抛错白屏 */
let slug = "";
try {
  slug = location.pathname === "/" ? "" : decodeURIComponent(location.pathname.slice(1));
} catch {
  slug = location.pathname.slice(1);
}
const $ = (id) => document.getElementById(id);

let dirty = false;
let previewMode = 0; // 0=仅编辑 1=分屏 2=全屏预览
let plainMode = false; // 纯文本模式：不解析 Markdown
let MAX_NOTE_MB = 5; // 由 /api/health 下发，仅用于报错提示
let HEALTH = {}; // /api/health 结果：transfer 配置等，boot 时填充
let saveTimer = null;
let renderTimer = null;

/* 单笔记密码：仅存 sessionStorage（标签页关闭即清），随请求头 X-Note-Password 发送 */
let notePwd = "";
try { notePwd = sessionStorage.getItem("sznote_pwd_" + slug) || ""; } catch { /* 隐私模式 */ }
let noteHasPwd = false;
const pwdHeader = () => notePwd ? { "X-Note-Password": notePwd } : {};

/* 附件短时令牌：受保护笔记的 <img>/下载链接无法带自定义请求头，
   解锁后由 getNote / /api/{slug}/ntok 签发 HMAC 令牌（15 分钟有效），访问 /f/ 附件时附带 */
let noteNtok = "";
let noteNtokAt = 0;
async function ensureNtok(force = false) {
  if (!noteHasPwd) return "";
  if (!force && noteNtok && Date.now() - noteNtokAt < 9 * 60 * 1000) return noteNtok;
  try {
    const r = await fetch("/api/" + encodeURIComponent(slug) + "/ntok", { headers: pwdHeader() });
    if (r.ok) { const d = await r.json(); noteNtok = d.ntok || ""; noteNtokAt = Date.now(); }
  } catch { /* 网络失败沿用旧令牌 */ }
  return noteNtok;
}
function tokUrl(u) {
  if (!noteHasPwd || !noteNtok || !u || !u.startsWith("/f/")) return u;
  return u + (u.includes("?") ? "&" : "?") + "ntok=" + encodeURIComponent(noteNtok);
}
/* 预览区附件链接改写（Markdown 里的 ![](/f/...) 与下载链接） */
function rewriteAttachUrls(root) {
  if (!noteHasPwd || !noteNtok) return;
  root.querySelectorAll('img[src^="/f/"], a[href^="/f/"]').forEach((el) => {
    const attr = el.tagName === "IMG" ? "src" : "href";
    el.setAttribute(attr, tokUrl(el.getAttribute(attr)));
  });
}

/* ---------- 通用 ---------- */

let toastTimer = null;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), 2200);
}

function setStatus(msg) { $("status").textContent = msg; }

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("已复制：" + text);
  } catch {
    // 兼容非 HTTPS 环境
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
    toast("已复制：" + text);
  }
}

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
  return (n / 1048576).toFixed(1) + " MB";
}

/* Markdown 渲染：marked + DOMPurify，链接一律新标签打开。
   两个库按 defer 加载（首页根本不加载），所以这里做惰性探测：
   库没到就先按纯文本显示，到了再补一次渲染，避免窄网下白屏好几秒。 */
let mdReady = false;

function ensureMdLibs() {
  if (mdReady) return true;
  if (typeof marked === "undefined" || typeof DOMPurify === "undefined") return false;
  DOMPurify.addHook("afterSanitizeAttributes", (node) => {
    if (node.tagName === "A") {
      node.setAttribute("target", "_blank");
      node.setAttribute("rel", "noopener noreferrer");
    }
  });
  mdReady = true;
  return true;
}
ensureMdLibs();

/* 库地址由服务端写进 meta[name="md-libs"]（带内容哈希版本号）；首页没有这个 meta。
   动态加载而不是写死在 HTML 里：同步的 27 KB 会和 app.js 抢同一条窄带，
   把「app.js → 接口 → 内容」这条关键链拖慢一两秒。 */
const MD_LIBS = (() => {
  const m = document.querySelector('meta[name="md-libs"]');
  return m && m.content ? m.content.split(",").map((s) => s.trim()).filter(Boolean) : [];
})();
let mdLoading = false;

function loadMdLibs() {
  if (ensureMdLibs() || mdLoading || !MD_LIBS.length) return;
  mdLoading = true;
  let i = 0;
  const next = () => {
    if (i >= MD_LIBS.length) {
      mdLoading = false;
      /* 库到齐：把先前按纯文本显示的预览升级成 Markdown 视图 */
      if (ensureMdLibs() && slug && !plainMode && $("content") && $("content").value) render();
      return;
    }
    const s = document.createElement("script");
    s.src = MD_LIBS[i++];
    s.onload = next;
    s.onerror = next;
    document.head.appendChild(s);
  };
  next();
}

/* 净化配置：禁掉表单/样式等标签，防止笔记内伪装登录框 */
const PURIFY_CFG = {
  FORBID_TAGS: ["form", "input", "button", "select", "textarea", "option", "optgroup",
    "style", "link", "meta", "base", "frame", "frameset", "object", "embed"],
  FORBID_ATTR: ["action", "formaction", "style", "srcset", "background", "poster"],
  ALLOW_DATA_ATTR: false,
};

function render() {
  const src = $("content").value;
  const preview = $("preview");
  /* 纯文本模式，或 Markdown 库还没下载完：按原文显示
     （textContent 天然转义，无 XSS 风险；库就绪后由 DOMContentLoaded 补渲染） */
  if (plainMode || !ensureMdLibs()) {
    const pre = document.createElement("pre");
    pre.className = "plaintext";
    pre.textContent = src || "（空笔记）";
    if (!src) pre.classList.add("empty");
    preview.innerHTML = "";
    preview.appendChild(pre);
    return;
  }
  const html = marked.parse(src, { gfm: true, breaks: true });
  preview.innerHTML = DOMPurify.sanitize(html, PURIFY_CFG);
  rewriteAttachUrls(preview);
  decorateCodeBlocks();
}

/* defer 脚本在 DOMContentLoaded 前执行完毕，此刻再补一次渲染即可升级为 Markdown 视图 */

/* 给每个代码块加「复制」按钮：复制内容保持原始缩进/换行，可直接粘进 Linux / Windows 终端 */
function decorateCodeBlocks() {
  $("preview").querySelectorAll("pre").forEach((pre) => {
    if (pre.parentElement && pre.parentElement.classList.contains("codewrap")) return;
    const wrap = document.createElement("div");
    wrap.className = "codewrap";
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);

    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copycode";
    btn.textContent = "复制";
    btn.title = "复制这段内容（保留原始缩进和换行）";
    btn.onclick = async () => {
      await copyText(pre.textContent.replace(/\n$/, ""));
      btn.textContent = "已复制";
      setTimeout(() => { btn.textContent = "复制"; }, 1200);
    };
    wrap.appendChild(btn);
  });
}

/* 光标处插入文本（自动补空行，保证 Markdown 块级元素独立成段） */
function insertAtCursor(text) {
  const ta = $("content");
  const s = ta.selectionStart, e = ta.selectionEnd;
  const before = ta.value.slice(0, s);
  const after = ta.value.slice(e);
  let pad0 = "";
  if (before.trim() !== "" && !before.endsWith("\n\n")) {
    pad0 = before.endsWith("\n") ? "\n" : "\n\n";
  }
  let pad1 = "";
  if (after.trim() !== "" && !after.startsWith("\n")) pad1 = "\n";
  ta.value = before + pad0 + text + pad1 + after;
  const pos = (before + pad0 + text).length;
  ta.focus();
  ta.selectionStart = ta.selectionEnd = pos;
  ta.dispatchEvent(new Event("input"));
}

/* ---------- 路由：登录门禁 → 首页 or 编辑器 ---------- */

/* 路由骨架同步显示：不依赖任何网络请求，首屏立即可见（LCP 不再被请求链拖住）。
   鉴权探测 / 配置下发在骨架显示后异步进行。 */
function boot() {
  if (!slug) {
    setupHome();          // #home 在 HTML 中默认可见
  } else {
    $("home").classList.add("hidden");
    $("editor").classList.remove("hidden");
    setupEditor();
    setStatus("加载中…");
    loadMdLibs();          // 按需加载 Markdown 库，不阻塞内容显示
  }
  refreshAuthAndConfig();
}

/* 异步：健康检查 → 会话探测 → 互传配置应用 */
async function refreshAuthAndConfig() {
  let needsLogin = false;
  try {
    const h = await (await fetch("/api/health")).json();
    HEALTH = h || {};
    if (HEALTH.max_note_mb) MAX_NOTE_MB = HEALTH.max_note_mb;
    if (HEALTH.auth) {
      // 站点密码已启用：探测会话是否有效（未登录时受保护 API 返回 401）
      const probe = await fetch("/api/" + (slug || "__ping"));
      needsLogin = probe.status === 401;
    }
  } catch { /* 探测失败不阻塞，后续真实请求会再次校验 */ }
  if (needsLogin) {
    showLoginScreen();
    return;
  }
  if (HEALTH.auth) startSessionWatch();
  if (slug && typeof applySwapConfig === "function") applySwapConfig();
}
/* async 脚本可能在 DOM 解析完成前执行：解析未完则挂到 DOMContentLoaded，
   避免在半成品 DOM 上初始化（也顺带让首绘完全不被脚本决策拖住） */
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", boot, { once: true });
} else {
  boot();
}

/* ---------- 登录 ---------- */

/* 弹回登录页的统一入口：任何地方发现会话失效都走这里 */
function showLoginScreen(msg) {
  $("home").classList.add("hidden");
  $("editor").classList.add("hidden");
  $("login").classList.remove("hidden");
  if (msg) {
    const el = $("loginError");
    el.textContent = msg;
    el.classList.remove("hidden");
  }
  setupLogin();
}

let loginBound = false;

function setupLogin() {
  const input = $("loginPassword");
  input.focus();

  const showLoginError = (msg) => {
    const el = $("loginError");
    el.textContent = msg;
    el.classList.remove("hidden");
  };

  const doLogin = async () => {
    if (!input.value) { showLoginError("请输入密码"); return; }
    const btn = $("btnLogin");
    btn.disabled = true;
    try {
      const r = await fetch("/api/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: input.value }),
      });
      if (r.ok) { location.reload(); return; }
      const d = await r.json().catch(() => ({}));
      showLoginError(d.error || "密码错误");
    } catch {
      showLoginError("网络错误，请重试");
    }
    btn.disabled = false;
  };

  $("btnLogin").onclick = doLogin;
  if (!loginBound) {          // 防止多次 setupLogin 重复绑定 Enter
    loginBound = true;
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") doLogin(); });
  }
}

/* ---------- 会话看门狗：空闲超时后主动弹回登录页 ----------
 * 每 60 秒 + 页面切回前台时探测一次 /api/session（该端点只读校验，
 * 不会刷新服务端的空闲计时，所以探测本身不会把会话续下去）。 */
let watchStarted = false;

function startSessionWatch() {
  if (watchStarted) return;
  watchStarted = true;
  const probe = async () => {
    if (document.hidden) return;          // 后台标签不探测，减少无谓请求
    try {
      const r = await fetch("/api/session");
      if (r.status === 401) showLoginScreen("登录已超时，请重新登录");
    } catch { /* 网络抖动忽略，下一个周期再查 */ }
  };
  setInterval(probe, 60000);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) probe(); });
}

/* ---------- 首页 ---------- */

function randomSlug() {
  const A = "abcdefghjkmnpqrstuvwxyz23456789"; // 31 字符表，10 位 ≈ 49.5 bit 熵
  const out = [];
  const need = 10;
  while (out.length < need) {
    const b = new Uint8Array(need * 2);
    crypto.getRandomValues(b);
    const max = 256 - (256 % A.length); // rejection sampling 消除取模偏置
    for (const v of b) {
      if (v >= max) continue;
      out.push(A[v % A.length]);
      if (out.length === need) break;
    }
  }
  return out.join("");
}

function setupHome() {
  const go = (s) => {
    s = (s || "").trim();
    if (!s) { toast("请输入自定义地址，或点「随机一个笔记」"); return; }
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{5,63}$/.test(s)) {
      toast("地址仅允许字母、数字、- 和 _，长度 6~64 字符");
      return;
    }
    location.href = "/" + s;
  };
  $("btnOpen").onclick = () => go($("customSlug").value);
  $("customSlug").addEventListener("keydown", (e) => { if (e.key === "Enter") go($("customSlug").value); });
  $("btnRandom").onclick = () => go(randomSlug());
}

/* ---------- 编辑器 ---------- */

function setupEditor() {
  const ta = $("content");
  document.title = "/" + slug + " · szNote";

  $("modeTag").onclick = togglePreview; // 顶栏中间的模式指示兼作预览切换按钮
  $("btnPlain").onclick = () => setPlain(!plainMode, true);
  $("btnPlainOff").onclick = () => setPlain(false, true);
  $("btnCopyAll").onclick = () => copyText($("content").value);
  $("btnUpload").onclick = () => $("fileInput").click();
  $("btnExport").onclick = exportNote;
  $("btnLock").onclick = () => openPwdModal("manage");
  $("btnLogout").onclick = async (e) => {
    e.preventDefault();
    await fetch("/api/logout", { method: "POST" });
    location.href = "/";
  };
  $("ttlSelect").addEventListener("change", save); // 改有效期即保存

  $("fileInput").addEventListener("change", (e) => {
    for (const f of e.target.files) uploadFile(f, f.type.startsWith("image/"));
    e.target.value = "";
  });

  /* 附件面板本身也是拖放区：拖进来只存储，不往正文插引用 */
  const panel = $("filesPanel");
  ["dragover", "dragenter"].forEach((ev) =>
    panel.addEventListener(ev, (e) => { e.preventDefault(); panel.classList.add("dragging"); }));
  ["dragleave", "drop"].forEach((ev) =>
    panel.addEventListener(ev, (e) => { e.preventDefault(); panel.classList.remove("dragging"); }));
  panel.addEventListener("drop", (e) => {
    for (const f of e.dataTransfer.files) uploadFile(f, f.type.startsWith("image/"), false);
  });

  /* 服务端已把已存在笔记的正文内嵌进 textarea：首屏立即渲染（库未就绪时按纯文本显示，
     Markdown 视图由 loadMdLibs + DOMContentLoaded 升级）。接口返回后由 load() 校正状态。 */
  if (ta.value) {
    render();
    setStatus("已加载");
  }

  ta.addEventListener("input", () => {
    dirty = true;
    setStatus("未保存…");
    clearTimeout(saveTimer);
    saveTimer = setTimeout(save, 2000);            // 2 秒无输入自动保存
    if (previewMode) {
      clearTimeout(renderTimer);
      renderTimer = setTimeout(render, 400);
    }
  });

  ta.addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") { e.preventDefault(); save(); }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "p") { e.preventDefault(); togglePreview(); }
    if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey) {
      const k = e.key.toLowerCase();
      if (k === "b") { e.preventDefault(); applyMd("bold"); }
      else if (k === "i") { e.preventDefault(); applyMd("italic"); }
      else if (k === "k") { e.preventDefault(); applyMd("link"); }
    }
    if (e.key === "Tab") { e.preventDefault(); insertAtCursor("  "); }
  });

  setupToolbar();
  setupPwdModal();
  $("mdToolbar").classList.toggle("hidden", plainMode);
  updateLockBtn();

  /* 悬停图片行：浮层直接显示图片和图片名 */
  const hover = document.createElement("div");
  hover.id = "imgHover";
  const hoverImg = document.createElement("img");
  const hoverName = document.createElement("span");
  hover.append(hoverImg, hoverName);
  document.body.appendChild(hover);
  const IMG_LINE = /^!\[(.*)\]\((.+?)\)\s*$/;
  const taCss = getComputedStyle(ta);
  const lineH = parseFloat(taCss.lineHeight);
  const padTop = parseFloat(taCss.paddingTop);

  ta.addEventListener("mousemove", (e) => {
    const rect = ta.getBoundingClientRect();
    const idx = Math.floor((e.clientY - rect.top + ta.scrollTop - padTop) / lineH);
    const lines = ta.value.split("\n");
    let hit = null;
    if (idx >= 0 && idx < lines.length) {
      const m = lines[idx].trim().match(IMG_LINE);
      if (m) hit = { alt: m[1], url: m[2] };
    }
    /* 只加载本站附件与 data: 图片，外链图片不请求 */
    const safeImg = hit && (hit.url.startsWith("/f/") || hit.url.startsWith("data:image/"));
    if (hit && safeImg) {
      const showUrl = tokUrl(hit.url);
      if (hoverImg.getAttribute("src") !== showUrl) {
        hoverImg.src = showUrl;
        hoverName.textContent = hit.alt || decodeURIComponent(hit.url.split("/").pop() || "");
      }
      hover.classList.add("show");
      let x = e.clientX + 16, y = e.clientY + 14;
      if (x + hover.offsetWidth > innerWidth - 8) x = e.clientX - hover.offsetWidth - 16;
      if (y + hover.offsetHeight > innerHeight - 8) y = e.clientY - hover.offsetHeight - 14;
      hover.style.left = x + "px";
      hover.style.top = y + "px";
    } else {
      hover.classList.remove("show");
    }
  });
  ta.addEventListener("mouseleave", () => hover.classList.remove("show"));

  /* 粘贴图片直接上传 */
  ta.addEventListener("paste", (e) => {
    for (const it of e.clipboardData?.items || []) {
      if (it.kind === "file" && it.type.startsWith("image/")) {
        e.preventDefault();
        const f = it.getAsFile();
        if (f) uploadFile(f, true);
        return;
      }
    }
  });

  /* 拖拽文件上传：整个编辑区（编辑器/预览）都接受拖放 */
  const panes = $("panes");
  ["dragover", "dragenter"].forEach((ev) =>
    panes.addEventListener(ev, (e) => { e.preventDefault(); panes.classList.add("dragging"); }));
  ["dragleave", "drop"].forEach((ev) =>
    panes.addEventListener(ev, (e) => { e.preventDefault(); panes.classList.remove("dragging"); }));
  panes.addEventListener("drop", (e) => {
    for (const f of e.dataTransfer.files) uploadFile(f, f.type.startsWith("image/"));
  });

  window.addEventListener("beforeunload", (e) => {
    if (dirty) { e.preventDefault(); e.returnValue = ""; }
  });

  setupSplitters();
  setupGutter(ta);
  applySavedWidths();
  setupSwap();

  /* 宽屏默认分屏：粘贴图片/文字当场就能看到效果 */
  setPreviewMode(window.innerWidth > 900 ? 1 : 0);

  load();
}

/* ---------- 三栏拖动：编辑区 | 预览区 | 附件面板 ---------- */

function setupSplitters() {
  const v = $("splitV");
  const h = $("splitH");

  /* 编辑/预览分界：按 #panes 宽度的百分比调整 */
  v.addEventListener("mousedown", (e) => {
    e.preventDefault();
    v.classList.add("active");
    const panes = $("panes");
    const move = (ev) => {
      const rect = panes.getBoundingClientRect();
      let pct = ((ev.clientX - rect.left) / rect.width) * 100;
      pct = Math.min(85, Math.max(15, pct));
      panes.style.setProperty("--edw", pct.toFixed(1) + "%");
      localStorage.setItem("szn.edw", pct.toFixed(1));
    };
    const up = () => {
      v.classList.remove("active");
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", up);
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", up);
  });

  /* 附件面板宽度：拖到 88px 以下进入 mini 模式（只显示序号和下载按钮） */
  h.addEventListener("mousedown", (e) => {
    e.preventDefault();
    h.classList.add("active");
    const move = (ev) => {
      let w = innerWidth - ev.clientX;
      w = Math.min(480, Math.max(44, w));
      $("filesPanel").style.setProperty("--filesw", w + "px");
      localStorage.setItem("szn.filesw", String(w));
      updateFilesMini(w);
    };
    const up = () => {
      h.classList.remove("active");
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", up);
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", up);
  });
}

function applySavedWidths() {
  const edw = parseFloat(localStorage.getItem("szn.edw"));
  if (edw >= 15 && edw <= 85) $("panes").style.setProperty("--edw", edw + "%");
  const fw = parseInt(localStorage.getItem("szn.filesw"), 10);
  if (fw >= 44 && fw <= 480) {
    $("filesPanel").style.setProperty("--filesw", fw + "px");
    updateFilesMini(fw);
  }
}

function updateFilesMini(w) {
  $("filesPanel").classList.toggle("mini", w < 88);
}

/* ---------- 编辑器行号：左侧 gutter，纯视觉层，不进正文 ----------
 * textarea 软换行下逻辑行 ≠ 显示行：用隐藏镜像层（与编辑区同字体、同内容宽度）
 * 实测每个逻辑行折行后的像素高度，行号按实测高度逐行排布；滚动时整列平移。
 * 超长笔记（>8000 行）退化为按单行高度估排，避免每次输入全量镜像重排。 */
function setupGutter(ta) {
  const g = $("gutter");
  const cs0 = getComputedStyle(ta);
  const LH = parseFloat(cs0.lineHeight) || 24.5;
  const mirror = document.createElement("div");
  const ms = mirror.style;
  /* CSP style-src 'self' 会拦截 cssText/内联 style 属性，逐条 CSSOM 赋值不受限 */
  ms.position = "absolute";
  ms.left = "-99999px";
  ms.top = "0";
  ms.visibility = "hidden";
  ms.whiteSpace = "pre-wrap";
  ms.overflowWrap = "break-word";
  ms.wordBreak = "normal";
  document.body.appendChild(mirror);
  const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  let timer = 0;
  function measure() {
    /* 编辑区不可见（全屏预览 / 未进入编辑页）时不绘制 */
    if (!ta.offsetParent || getComputedStyle(ta).display === "none") {
      g.style.display = "none";
      return;
    }
    g.style.display = "";
    const cs = getComputedStyle(ta);
    ms.fontSize = cs.fontSize;
    ms.fontFamily = cs.fontFamily;
    ms.fontWeight = cs.fontWeight;
    ms.lineHeight = cs.lineHeight;
    ms.tabSize = cs.tabSize;
    ms.letterSpacing = cs.letterSpacing;
    const scrollW = ta.offsetWidth - ta.clientWidth;      // 竖向滚动条宽度
    ms.width = (ta.clientWidth - scrollW -
      parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)) + "px";
    g.style.paddingTop = cs.paddingTop;                   // 与正文首行对齐
    const lines = ta.value.split("\n");
    /* 行高用 CSSOM 赋值（createElement + style.height）——页面 CSP 禁止
       style 属性，innerHTML 拼 style="..." 会被整体丢弃 */
    const frag = document.createDocumentFragment();
    if (lines.length <= 8000) {
      let html = "";
      for (let i = 0; i < lines.length; i++) {
        html += lines[i] ? "<div>" + esc(lines[i]) + "</div>" : "<div>\u200b</div>";
      }
      mirror.innerHTML = html;
      const rows = mirror.children;
      for (let i = 0; i < rows.length; i++) {
        addRow(frag, i + 1, rows[i].offsetHeight || LH);
      }
    } else {
      for (let i = 1; i <= lines.length; i++) addRow(frag, i, LH);
    }
    g.replaceChildren(frag);
    sync();
  }
  function addRow(frag, num, h) {
    const d = document.createElement("div");
    d.className = "ln";
    d.style.height = h + "px";
    d.style.lineHeight = LH + "px";
    d.textContent = num;
    frag.appendChild(d);
  }
  function sync() { g.style.transform = "translateY(" + -ta.scrollTop + "px)"; }
  function schedule() { clearTimeout(timer); timer = setTimeout(measure, 120); }

  ta.addEventListener("input", schedule);
  ta.addEventListener("scroll", sync, { passive: true });
  new ResizeObserver(schedule).observe(ta);
  ta.__gutter = schedule;      // load() 等直接赋值 ta.value 的路径调用它重排
  measure();
}

async function load() {
  const r = await fetch("/api/" + encodeURIComponent(slug), { headers: pwdHeader() });
  if (r.status === 401) {
    const d = await r.json().catch(() => ({}));
    if (d.protected) { openPwdModal("unlock"); setStatus("该笔记已设密码"); return; }
    showLoginScreen("登录已超时，请重新登录");  // 会话过期 → 回到登录页
    return;
  }
  try {
    const d = await r.json();
    if (d.exists) $("content").value = d.content;
    if ($("content").__gutter) $("content").__gutter();   // 行号列随内容重排
    setPlain(!!d.plain, false);
    noteHasPwd = !!d.pwd;
    if (d.pwd) { noteNtok = d.ntok || ""; noteNtokAt = Date.now(); }
    updateLockBtn();
    renderFiles(d.files || []);
    setStatus(d.exists ? "已加载" : "新笔记，首次保存后生效");
    if (d.exists) render();
    if (typeof d.ttl_hours === "number") applyTTL(d.ttl_hours);
  } catch {
    setStatus("加载失败，请刷新重试");
  }
}

const TTL_DAYS = [24, 72, 120, 168]; // 1 / 3 / 5 / 7 天

function applyTTL(hours) {
  // 只允许 1/3/5/7 天，历史数据（0=永久或其它值）统一落到 7 天
  if (!TTL_DAYS.includes(hours)) hours = 168;
  $("ttlSelect").value = String(hours);
}

async function save() {
  clearTimeout(saveTimer);
  try {
    // ttl_hours 只在有合法值时携带（缺省 = 不修改，服务端把 0 当 400 拒掉）
    const ttl = parseInt($("ttlSelect").value, 10);
    const payload = { content: $("content").value, plain: plainMode };
    if (TTL_DAYS.includes(ttl)) payload.ttl_hours = ttl;
    const r = await fetch("/api/" + encodeURIComponent(slug), {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...pwdHeader() },
      body: JSON.stringify(payload),
    });
    if (r.status === 401) {
      const d = await r.json().catch(() => ({}));
      if (d.protected) { openPwdModal("unlock", "笔记密码不正确或已变更，请重新解锁"); return; }
      showLoginScreen("登录已超时，请重新登录");
      return;
    }
    if (!r.ok) {
      const d = await r.json().catch(() => ({}));
      throw new Error(d.error || "http " + r.status);
    }
    dirty = false;
    setStatus("已保存 " + new Date().toLocaleTimeString());
    refreshFiles();
  } catch (err) {
    setStatus("保存失败！");
    const msg = err.message || "未知错误";
    toast("保存失败：" + msg + (/上限|大小/.test(msg) ? "" : "（单条笔记上限 " + MAX_NOTE_MB + " MB）"));
  }
}

async function exportNote() {
  try {
    const r = await fetch("/api/" + encodeURIComponent(slug) + "/export", { headers: pwdHeader() });
    if (r.status === 401) {
      const d = await r.json().catch(() => ({}));
      if (d.protected) { openPwdModal("unlock", "受保护笔记需先解锁才能导出"); return; }
      showLoginScreen("登录已超时，请重新登录");
      return;
    }
    if (!r.ok) throw new Error("http " + r.status);
    const blob = await r.blob();
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = slug + ".zip";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 3000);
  } catch {
    toast("导出失败");
  }
}

/* ---------- Markdown 工具栏（按钮与 Ctrl+B/I/K 共用同一 applyMd） ---------- */

function applyMd(action) {
  const ta = $("content");
  const s = ta.selectionStart, e = ta.selectionEnd, v = ta.value;
  const sel = v.slice(s, e);

  if (action === "heading") { // 标题循环切换：无级别 → h1 → … → h6 → 无级别
    const ls = v.lastIndexOf("\n", s - 1) + 1;
    let le = v.indexOf("\n", e);
    if (le === -1) le = v.length;
    const line = v.slice(ls, le);
    const m = line.match(/^(#{1,6})\s/);
    const level = m ? m[1].length : 0;
    const next = level >= 6 ? 0 : level + 1;
    const body = line.replace(/^#{1,6}\s+/, "");
    const text = next === 0 ? body : "#".repeat(next) + " " + body;
    ta.value = v.slice(0, ls) + text + v.slice(le);
    ta.selectionStart = ls;
    ta.selectionEnd = ls + text.length;
    ta.focus();
    ta.dispatchEvent(new Event("input"));
    return;
  }

  let text = "", cs = 0, ce = 0; // 插入文本 + 相对选中区（占位文本直接处于选中态，输入即替换）
  if (action === "bold") {
    const inner = sel || "加粗文本";
    text = "**" + inner + "**";
    cs = 2; ce = 2 + inner.length;
  } else if (action === "italic") {
    const inner = sel || "斜体文本";
    text = "*" + inner + "*";
    cs = 1; ce = 1 + inner.length;
  } else if (action === "link") {
    const label = sel || "链接文本";
    text = "[" + label + "](https://)";
    cs = label.length + 3; ce = text.length;
  } else if (action === "image") {
    text = "![" + (sel || "图片描述") + "](图片地址)";
    cs = text.lastIndexOf("(") + 1; ce = text.length - 1;
  } else {
    return;
  }
  ta.value = v.slice(0, s) + text + v.slice(e);
  ta.selectionStart = s + cs;
  ta.selectionEnd = s + ce;
  ta.focus();
  ta.dispatchEvent(new Event("input"));
}

function setupToolbar() {
  $("mdToolbar").addEventListener("click", (e) => {
    const btn = e.target.closest("button[data-md]");
    if (btn) applyMd(btn.dataset.md);
  });
}

/* ---------- 单笔记密码：解锁弹窗 + 设置/移除 ---------- */

function updateLockBtn() {
  const b = $("btnLock");
  if (!b) return;
  b.classList.toggle("active", noteHasPwd);
  b.title = noteHasPwd ? "笔记密码：已设密码（正文加密落盘），点击管理" : "笔记密码：给这条笔记单独加锁";
}

function pwdFail(msg) {
  const el = $("pwdError");
  el.textContent = msg;
  el.classList.remove("hidden");
}

function openPwdModal(mode, msg) {
  $("pwdUnlock").classList.toggle("hidden", mode !== "unlock");
  $("pwdManage").classList.toggle("hidden", mode !== "manage");
  $("pwdError").classList.add("hidden");
  $("pwdState").textContent = noteHasPwd
    ? "当前状态：已设密码（正文已加密落盘）"
    : "当前状态：未设密码";
  if (msg) pwdFail(msg);
  $("pwdMask").classList.remove("hidden");
  (mode === "unlock" ? $("pwdInput") : $("pwdNew")).focus();
}

function closePwdModal() { $("pwdMask").classList.add("hidden"); }

function storePwd(pw) {
  notePwd = pw;
  try {
    if (pw) sessionStorage.setItem("sznote_pwd_" + slug, pw);
    else sessionStorage.removeItem("sznote_pwd_" + slug);
  } catch { /* 隐私模式 */ }
}

function setupPwdModal() {
  $("pwdClose").onclick = closePwdModal;
  $("pwdMask").addEventListener("click", (e) => { if (e.target === $("pwdMask")) closePwdModal(); });

  const unlock = async () => {
    const pw = $("pwdInput").value;
    if (!pw) return;
    const r = await fetch("/api/" + encodeURIComponent(slug), { headers: { "X-Note-Password": pw } });
    const d = await r.json().catch(() => ({}));
    if (r.ok) {
      storePwd(pw);
      noteHasPwd = !!d.pwd;
      $("pwdInput").value = "";
      closePwdModal();
      load();
    } else {
      pwdFail(d.error || "密码错误");
    }
  };
  $("btnPwdUnlock").onclick = unlock;
  $("pwdInput").addEventListener("keydown", (e) => { if (e.key === "Enter") unlock(); });

  $("btnPwdSet").onclick = async () => {
    const np = $("pwdNew").value;
    if (np.length < 4 || np.length > 128) { pwdFail("密码长度需 4–128 位"); return; }
    const body = { action: "set", password: np };
    if (noteHasPwd) body.current = $("pwdOld").value || notePwd;
    const r = await fetch("/api/" + encodeURIComponent(slug) + "/password", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...pwdHeader() },
      body: JSON.stringify(body),
    });
    const d = await r.json().catch(() => ({}));
    if (r.ok && d.ok) {
      storePwd(np);
      noteHasPwd = true;
      $("pwdNew").value = "";
      $("pwdOld").value = "";
      updateLockBtn();
      closePwdModal();
      setStatus("已保存 " + new Date().toLocaleTimeString());
      toast("已为该笔记设置密码，正文加密落盘");
    } else {
      pwdFail(d.error || "设置失败（http " + r.status + "）");
    }
  };

  $("btnPwdRemove").onclick = async () => {
    const cur = $("pwdOld").value || notePwd;
    if (!cur) { pwdFail("请输入当前密码"); return; }
    const r = await fetch("/api/" + encodeURIComponent(slug) + "/password", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...pwdHeader() },
      body: JSON.stringify({ action: "remove", current: cur }),
    });
    const d = await r.json().catch(() => ({}));
    if (r.ok && d.ok) {
      storePwd("");
      noteHasPwd = false;
      $("pwdOld").value = "";
      updateLockBtn();
      closePwdModal();
      load();
      toast("已移除该笔记密码，正文恢复明文");
    } else {
      pwdFail(d.error || "移除失败（http " + r.status + "）");
    }
  };
}

async function refreshFiles() {
  try {
    const r = await fetch("/api/" + encodeURIComponent(slug));
    const d = await r.json();
    renderFiles(d.files || []);
  } catch { /* 静默失败 */ }
}

function togglePreview() {
  setPreviewMode((previewMode + 1) % 3);
  if (previewMode === 0) $("content").focus();
}

/* 预览三态：仅编辑 → 分屏 → 全屏预览 → 仅编辑…（Ctrl+P 同样循环） */
function setPreviewMode(mode) {
  previewMode = mode;
  const panes = $("panes");
  $("preview").classList.toggle("hidden", mode === 0);
  panes.classList.toggle("previewing", mode > 0);
  panes.classList.toggle("fullpreview", mode === 2);
  $("mdToolbar").classList.toggle("hidden", mode === 2 || plainMode); // 全屏预览/纯文本时隐藏
  /* 顶栏中间的模式指示（兼作切换按钮），颜色随状态变化 */
  const tag = $("modeTag");
  tag.title = mode === 2 ? "返回编辑 (Ctrl+P)" : "预览：点一下分屏，再点一下全屏 (Ctrl+P)";
  tag.classList.remove("hidden");
  tag.classList.remove("mode-edit", "mode-split", "mode-preview");
  if (mode === 0) { tag.textContent = "编辑"; tag.classList.add("mode-edit"); }
  else if (mode === 1) { tag.textContent = "分屏"; tag.classList.add("mode-split"); }
  else { tag.textContent = "预览"; tag.classList.add("mode-preview"); }
  if (mode > 0) render();
}

/* 纯文本模式：不解析 Markdown，日志/命令原样显示；该状态随笔记存进 meta.json */
function setPlain(on, persist = true) {
  plainMode = on;
  const btn = $("btnPlain");
  btn.textContent = on ? "纯文本" : "Markdown";
  btn.classList.toggle("active", on);
  btn.title = on
    ? "当前：纯文本模式（不解析 Markdown）。点击切回 Markdown"
    : "当前：Markdown 模式。点击切到纯文本模式（适合贴日志 / 命令 / 配置）";
  $("plainHint").classList.toggle("hidden", !on);
  $("mdToolbar").classList.toggle("hidden", !!on); // 纯文本模式下工具栏没有意义
  $("preview").classList.toggle("plainmode", on);
  $("content").placeholder = on
    ? "纯文本模式：直接粘贴日志、命令或配置…（不解析 Markdown，原样保存）"
    : "在这里输入 Markdown…\n\n支持直接 Ctrl+V 粘贴截图，或把文件拖进来。";
  if (previewMode > 0) render();
  if (persist) { dirty = true; save(); }
}

async function uploadFile(file, isImage, insert = true) {
  setStatus("上传中 " + file.name + "…");
  const fd = new FormData();
  fd.append("file", file);
  try {
    const r = await fetch("/api/" + encodeURIComponent(slug) + "/upload", { method: "POST", headers: pwdHeader(), body: fd });
    if (r.status === 401) {
      const d = await r.json().catch(() => ({}));
      if (d.protected) { openPwdModal("unlock", "受保护笔记需先解锁才能上传附件"); return; }
      showLoginScreen("登录已超时，请重新登录");
      return;
    }
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || "上传失败");
    if (insert) {
      /* 纯文本模式不插入 Markdown 语法，只贴裸链接，避免污染日志/命令 */
      const md = plainMode
        ? d.url
        : (isImage ? "!" : "") + "[" + d.name + "](" + d.url + ")";
      insertAtCursor(md + "\n");
      dirty = true;
      save();
    }
    setStatus("上传完成");
    refreshFiles();
  } catch (err) {
    setStatus("上传失败");
    toast("上传失败：" + (err.message || "未知错误"));
  }
}

const IMG_EXT = /\.(png|jpe?g|gif|webp|bmp)$/i;

function renderFiles(files) {
  const ul = $("fileList");
  ul.innerHTML = "";
  $("filesEmpty").style.display = files.length ? "none" : "";
  for (let i = 0; i < files.length; i++) {
    const f = files[i];
    const li = document.createElement("li");
    li.title = f.name; // 悬停显示完整文件名（mini 模式下唯一的文件名入口）

    /* 序号：面板拖到最窄时替代文件名显示 */
    const idx = document.createElement("span");
    idx.className = "fidx";
    idx.textContent = (i + 1) + ".";

    let thumb;
    if (IMG_EXT.test(f.name)) {
      thumb = document.createElement("img");
      thumb.className = "fthumb";
      thumb.src = tokUrl(f.url);
      thumb.loading = "lazy";
      thumb.alt = f.name;
      thumb.title = "点击放大预览";
      thumb.onclick = () => window.open(tokUrl(f.url), "_blank");
    } else {
      thumb = document.createElement("span");
      thumb.className = "ficon";
      thumb.innerHTML = '<svg class="icon" aria-hidden="true"><use href="#icon-note"></use></svg>';
    }

    /* 文件名：点击直接下载 */
    const name = document.createElement("a");
    name.className = "fname";
    name.textContent = f.name;
    name.href = tokUrl(f.url);
    name.setAttribute("download", f.name);
    name.title = "点击下载 " + f.name;

    const size = document.createElement("span");
    size.className = "fsize";
    size.textContent = fmtSize(f.size);

    /* 下载按钮 */
    const dl = document.createElement("a");
    dl.className = "fbtn";
    dl.textContent = "⬇";
    dl.href = tokUrl(f.url);
    dl.setAttribute("download", f.name);
    dl.title = "下载 " + f.name;

    const del = document.createElement("button");
    del.className = "fbtn del";
    del.textContent = "✕";
    del.title = "删除此附件";
    del.onclick = async () => {
      if (!confirm("删除附件 " + f.name + "？笔记正文中的引用会变成死链。")) return;
      const resp = await fetch("/api/" + encodeURIComponent(slug) + "/file/" + encodeURIComponent(f.name), { method: "DELETE", headers: pwdHeader() });
      if (resp.status === 401) {
        const d = await resp.json().catch(() => ({}));
        if (d.protected) { openPwdModal("unlock", "受保护笔记需先解锁才能管理附件"); return; }
        showLoginScreen("登录已超时，请重新登录");
        return;
      }
      refreshFiles();
    };

    li.append(idx, thumb, name, size, dl, del);
    ul.appendChild(li);
  }
}

/* ---------- 文件互传（服务器中转） ---------- */

/* 互传配置应用：HEALTH 就绪后调用（按钮显隐 / accept / 限速提示） */
function applySwapConfig() {
  const T = HEALTH.transfer || {};
  if (T.enabled === false) { // 配置里关掉了：隐藏入口
    $("btnSwap").classList.add("hidden");
    return;
  }
  if (Array.isArray(T.exts) && T.exts.length) {
    $("swapFile").accept = T.exts.join(",");
    const parts = ["允许的类型：" + T.exts.join(" ")];
    if (T.max_file_mb) parts.push("单文件上限 " + T.max_file_mb + " MB");
    if (T.up_kbps) parts.push("上传限速 " + T.up_kbps + " KB/s");
    if (T.down_kbps) parts.push("下载限速 " + T.down_kbps + " KB/s");
    $("sendHint").textContent = parts.join(" · ");
  }
}

/* 弹窗开关 / 拖放上传。依赖 /api/health 的配置应用拆到 applySwapConfig()，
   由 boot 的异步阶段在 HEALTH 就绪后调用（首屏显示不再等待网络）。
   无取件码：上传完成后文件直接出现在列表里，下载 / 撤回都在行内。 */
let swapPoll = null;

function setupSwap() {
  const open = () => {
    $("swapMask").classList.remove("hidden");
    loadRecvList();
    clearInterval(swapPoll);
    swapPoll = setInterval(() => {           // 弹窗开着时自动刷新，对方上传的文件会自己出现
      if (!$("swapMask").classList.contains("hidden")) loadRecvList();
    }, 5000);
  };
  const close = () => {
    $("swapMask").classList.add("hidden");
    clearInterval(swapPoll);
    swapPoll = null;
  };
  $("btnSwap").onclick = open;
  $("swapClose").onclick = close;
  $("swapMask").addEventListener("click", (e) => { if (e.target === $("swapMask")) close(); });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !$("swapMask").classList.contains("hidden")) close();
  });

  $("btnPickFile").onclick = () => $("swapFile").click();
  $("swapFile").addEventListener("change", (e) => {
    const files = Array.from(e.target.files || []);
    e.target.value = "";
    queueSend(files);
  });
  $("btnRefreshList").onclick = () => loadRecvList();

  /* 拖入上传：整块上传区都是投放目标 */
  const dz = $("swapDrop");
  const stop = (e) => { e.preventDefault(); e.stopPropagation(); };
  ["dragenter", "dragover"].forEach((ev) =>
    dz.addEventListener(ev, (e) => { stop(e); dz.classList.add("dragging"); }));
  ["dragleave", "dragend"].forEach((ev) =>
    dz.addEventListener(ev, (e) => { stop(e); dz.classList.remove("dragging"); }));
  dz.addEventListener("drop", (e) => {
    stop(e);
    dz.classList.remove("dragging");
    const files = Array.from((e.dataTransfer && e.dataTransfer.files) || []);
    queueSend(files);
  });
}

/* 多个文件按顺序上传，避免并发抢同一条上行链路 */
let sendQueue = Promise.resolve();
function queueSend(files) {
  if (!files.length) return;
  sendQueue = sendQueue.then(async () => {
    for (const f of files) await startSend(f);
    await loadRecvList();
  }).catch(() => {});
}

function randomHex(nBytes) {
  const b = new Uint8Array(nBytes);
  crypto.getRandomValues(b);
  return Array.from(b, (v) => v.toString(16).padStart(2, "0")).join("");
}

async function postJSON(url, body) {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || "http " + r.status);
  return d;
}

function fmtEta(sec) {
  if (!isFinite(sec) || sec < 0) return "--";
  sec = Math.ceil(sec);
  if (sec >= 3600) return Math.floor(sec / 3600) + "h" + Math.ceil((sec % 3600) / 60) + "m";
  if (sec >= 60) return Math.floor(sec / 60) + "m" + (sec % 60) + "s";
  return sec + "s";
}

/* 速度估计：按已传输字节的时间序列做 EMA 平滑，进度条不抖 */
function speedTracker() {
  let lastBytes = null, lastT = 0, ema = 0;
  return (bytes) => {
    const now = Date.now();
    if (lastBytes !== null && now > lastT && bytes >= lastBytes) {
      const inst = (bytes - lastBytes) / ((now - lastT) / 1000);
      ema = ema ? ema * 0.7 + inst * 0.3 : inst;
    }
    lastBytes = bytes;
    lastT = now;
    return ema;
  };
}

function setProgress(bar, statEl, got, total, speed) {
  const pct = total > 0 ? Math.min(100, (got / total) * 100) : 0;
  bar.style.width = pct.toFixed(1) + "%";
  const parts = [fmtSize(got) + " / " + fmtSize(total), pct.toFixed(1) + "%"];
  if (speed > 0) {
    parts.push((speed / 1024).toFixed(1) + " KB/s");
    parts.push("剩余 " + fmtEta((total - got) / speed));
  }
  statEl.textContent = parts.join(" · ");
}

/* 发送：分块上传，断线自动重试并按服务端进度续传；
   同一文件（名字+大小+修改时间一致）跨刷新也能续传（令牌存 localStorage） */
async function startSend(file) {
  const T = HEALTH.transfer || {};
  const ext = ("." + (file.name.split(".").pop() || "")).toLowerCase();
  if (Array.isArray(T.exts) && T.exts.length && !T.exts.includes(ext)) {
    toast("仅允许发送：" + T.exts.join(" "));
    return;
  }
  if (T.max_file_mb && file.size > T.max_file_mb * 1048576) {
    toast("文件超过 " + T.max_file_mb + " MB 上限");
    return;
  }
  let prev = null;
  try { prev = JSON.parse(localStorage.getItem("szn.tr.resume") || "null"); } catch {}
  const same = prev && prev.name === file.name && prev.size === file.size && prev.mtime === file.lastModified;
  const token = (same && prev.token) || randomHex(32);

  $("sendBusy").classList.remove("hidden");
  $("sendName").textContent = file.name;
  const bar = $("sendBar"), stat = $("sendStat");
  const speed = speedTracker();
  const setProg = (got) => setProgress(bar, stat, got, file.size, speed(got));

  try {
    let init = await postJSON("/api/transfer/init", { name: file.name, size: file.size, token });
    const code = init.code;
    const CHUNK = init.chunk || 262144;
    localStorage.setItem("szn.tr.resume",
      JSON.stringify({ token, name: file.name, size: file.size, mtime: file.lastModified }));
    let offset = init.offset || 0;
    setProg(offset);
    let fails = 0;
    while (offset < file.size) {
      const end = Math.min(offset + CHUNK, file.size);
      try {
        const r = await fetch(`/api/transfer/${code}/chunk?offset=${offset}`, {
          method: "PUT",
          body: file.slice(offset, end),
        });
        if (r.status === 401) { showLoginScreen("登录已超时，请重新登录"); return; }
        const d = await r.json().catch(() => ({}));
        // 4xx 是确定性失败（类型不符、超限、PK 头校验不过），重试没有意义
        if (r.status >= 400 && r.status < 500) throw Object.assign(new Error(d.error || "http " + r.status), { fatal: true });
        if (!r.ok) throw new Error(d.error || "http " + r.status);
        offset = typeof d.offset === "number" ? d.offset : end;
        fails = 0;
      } catch (err) {
        if (err.fatal) throw err;
        fails++;
        if (fails >= 5) throw err;
        await new Promise((res) => setTimeout(res, 1200 * fails));
        // 网络恢复后先向服务端核实真实进度，再从那里继续
        try {
          const re = await postJSON("/api/transfer/init", { name: file.name, size: file.size, token });
          if (re.code === code && typeof re.offset === "number") offset = re.offset;
        } catch { /* 下一轮重试再同步 */ }
      }
      setProg(offset);
    }
    await postJSON(`/api/transfer/${code}/complete`, {});
    localStorage.removeItem("szn.tr.resume");
    $("sendBusy").classList.add("hidden");
    toast("已上传，可直接在列表里下载或撤回");
  } catch (err) {
    $("sendBusy").classList.add("hidden");
    toast("发送中断：" + (err.message || "网络错误") + "（进度已保留，重选同一文件可续传）");
  }
}

/* 文件列表：无取件码，直接列出中转文件（下载 / 撤回） */
async function loadRecvList() {
  const ul = $("recvList");
  ul.innerHTML = "";
  try {
    const r = await fetch("/api/transfer");
    if (r.status === 401) { showLoginScreen("登录已超时，请重新登录"); return; }
    const d = await r.json();
    const items = d.items || [];
    $("recvEmpty").style.display = items.length ? "none" : "";
    for (const it of items) {
      const li = document.createElement("li");
      li.className = "recv-item";
      const info = document.createElement("span");
      info.className = "recv-info";
      info.textContent = it.name + "（" + fmtSize(it.size) + "）";
      info.title = it.name;
      const dl = document.createElement("button");
      dl.className = "mini primary";
      dl.textContent = "下载";
      dl.onclick = () => downloadTransfer(it.code, it.name, it.size);
      const del = document.createElement("button");
      del.className = "mini";
      del.textContent = "撤回";
      del.title = "删除该中转文件，接收方将无法再下载";
      del.onclick = async () => {
        if (!confirm("撤回 " + it.name + "？")) return;
        await fetch("/api/transfer/" + encodeURIComponent(it.code), { method: "DELETE" });
        loadRecvList();
      };
      li.append(info, dl, del);
      ul.appendChild(li);
    }
  } catch { /* 列表拉取失败不打扰用户 */ }
}

/* 下载：ReadableStream 逐块计数展示进度；中断后用 Range 从已收字节续传 */
async function downloadTransfer(code, name, size) {
  $("recvBusy").classList.remove("hidden");
  $("recvName").textContent = name;
  const bar = $("recvBar"), stat = $("recvStat");
  const speed = speedTracker();
  const setProg = (got) => setProgress(bar, stat, got, size, speed(got));
  let got = 0;
  const parts = [];
  try {
    for (let attempt = 0;;) {
      let r;
      try {
        r = await fetch("/api/transfer/download?code=" + encodeURIComponent(code),
          got > 0 ? { headers: { Range: "bytes=" + got + "-" } } : {});
      } catch (netErr) {
        throw netErr;
      }
      if (r.status === 401) { showLoginScreen("登录已超时，请重新登录"); return; }
      if (!r.ok && r.status !== 206) {
        const d = await r.json().catch(() => ({}));
        throw new Error(d.error || "http " + r.status);
      }
      if (attempt === 0) {
        // 从 Content-Disposition 解析真实文件名（取件码查询场景）
        const cd = r.headers.get("Content-Disposition") || "";
        const m = cd.match(/filename\*=UTF-8''([^;]+)/i);
        if (m) { try { name = decodeURIComponent(m[1]); } catch { /* 保持原名 */ } }
        else if ((m = cd.match(/filename="([^"]+)"/))) name = m[1];
        $("recvName").textContent = name;
        const cl = parseInt(r.headers.get("Content-Length"), 10);
        if (cl > 0) size = got + cl;
      } else if (r.status === 200 && got > 0) {
        parts.length = 0; got = 0; // 服务端未续传：从头收
      }
      const reader = r.body.getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        parts.push(value);
        got += value.length;
        setProg(got);
      }
      break;
    }
    const blob = new Blob(parts);
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
    toast("下载完成：" + name);
  } catch (err) {
    toast("下载中断：" + (err.message || "网络错误") + "（已收 " + fmtSize(got) + "，重新下载可续传）");
  } finally {
    $("recvBusy").classList.add("hidden");
    loadRecvList();
  }
}
