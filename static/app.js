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
let saveTimer = null;
let renderTimer = null;

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

/* Markdown 渲染：marked + DOMPurify，链接一律新标签打开 */
DOMPurify.addHook("afterSanitizeAttributes", (node) => {
  if (node.tagName === "A") {
    node.setAttribute("target", "_blank");
    node.setAttribute("rel", "noopener noreferrer");
  }
});

/* 净化配置：默认白名单会放行 form/input/style 等标签，可被用于同源钓鱼与界面伪装，
   这里显式禁掉主动内容标签与危险属性（服务端 CSP form-action 'none' 是第二道防线） */
const PURIFY_CFG = {
  FORBID_TAGS: ["form", "input", "button", "select", "textarea", "option", "optgroup",
    "style", "link", "meta", "base", "frame", "frameset", "object", "embed"],
  FORBID_ATTR: ["action", "formaction", "style", "srcset", "background", "poster"],
  ALLOW_DATA_ATTR: false,
};

function render() {
  const src = $("content").value;
  const preview = $("preview");
  /* 纯文本模式：不走 Markdown，直接按原文显示（textContent 天然转义，无 XSS 风险） */
  if (plainMode) {
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
  decorateCodeBlocks();
}

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

async function boot() {
  let needsLogin = false;
  try {
    const h = await (await fetch("/api/health")).json();
    if (h.max_note_mb) MAX_NOTE_MB = h.max_note_mb;
    if (h.auth) {
      // 站点密码已启用：探测会话是否有效（未登录时受保护 API 返回 401）
      const probe = await fetch("/api/" + (slug || "__ping"));
      needsLogin = probe.status === 401;
    }
  } catch { /* 探测失败不阻塞，后续真实请求会再次校验 */ }
  if (needsLogin) {
    $("login").classList.remove("hidden");
    setupLogin();
    return;
  }
  if (!slug) {
    $("home").classList.remove("hidden");
    setupHome();
  } else {
    $("editor").classList.remove("hidden");
    setupEditor();
  }
}
boot();

/* ---------- 登录 ---------- */

function setupLogin() {
  const input = $("loginPassword");
  input.focus();

  const showLoginError = (msg) => {
    const el = $("loginError");
    el.textContent = msg;
    el.style.display = "";
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
  input.addEventListener("keydown", (e) => { if (e.key === "Enter") doLogin(); });
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
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(s)) {
      toast("地址仅允许字母、数字、- 和 _，最长 64 字符");
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

  $("btnCopyLink").onclick = copyLink;
  $("btnSave").onclick = save;
  $("btnPreview").onclick = togglePreview;
  $("btnPlain").onclick = () => setPlain(!plainMode, true);
  $("btnPlainOff").onclick = () => setPlain(false, true);
  $("btnCopyAll").onclick = () => copyText($("content").value);
  $("btnUpload").onclick = () => $("fileInput").click();
  $("btnExport").onclick = () => window.open("/api/" + encodeURIComponent(slug) + "/export", "_blank");
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
    if (e.key === "Tab") { e.preventDefault(); insertAtCursor("  "); }
  });

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
    /* 只预览本站附件（/f/）与 data: 图片；笔记里的任意外链不加载，
       防止第三方追踪与浏览器侧内网探测 */
    const safeImg = hit && (hit.url.startsWith("/f/") || hit.url.startsWith("data:image/"));
    if (hit && safeImg) {
      if (hoverImg.getAttribute("src") !== hit.url) {
        hoverImg.src = hit.url;
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

  /* 宽屏默认分屏：粘贴图片/文字当场就能看到效果 */
  setPreviewMode(window.innerWidth > 860 ? 1 : 0);

  load();
}

function copyLink() { copyText(location.origin + "/" + slug); }

async function load() {
  const r = await fetch("/api/" + encodeURIComponent(slug));
  if (r.status === 401) {  // 会话过期 → 回到登录页
    $("editor").classList.add("hidden");
    $("login").classList.remove("hidden");
    setupLogin();
    return;
  }
  try {
    const d = await r.json();
    if (d.exists) $("content").value = d.content;
    setPlain(!!d.plain, false);
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
    const ttl = parseInt($("ttlSelect").value, 10) || 0;
    const r = await fetch("/api/" + encodeURIComponent(slug), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: $("content").value, ttl_hours: ttl, plain: plainMode }),
    });
    if (r.status === 401) {
      setStatus("登录已过期");
      $("editor").classList.add("hidden");
      $("login").classList.remove("hidden");
      setupLogin();
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
  const btn = $("btnPreview");
  btn.textContent = mode === 2 ? "全屏" : mode === 1 ? "分屏" : "预览";
  btn.title = mode === 2 ? "返回编辑 (Ctrl+P)" : "预览：点一下分屏，再点一下全屏 (Ctrl+P)";
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
    const r = await fetch("/api/" + encodeURIComponent(slug) + "/upload", { method: "POST", body: fd });
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
  for (const f of files) {
    const li = document.createElement("li");
    li.title = f.name; // 悬停显示完整文件名
    let thumb;
    if (IMG_EXT.test(f.name)) {
      thumb = document.createElement("img");
      thumb.className = "fthumb";
      thumb.src = f.url;
      thumb.loading = "lazy";
      thumb.alt = f.name;
      thumb.title = "点击放大预览";
      thumb.onclick = () => window.open(f.url, "_blank");
    } else {
      thumb = document.createElement("span");
      thumb.className = "ficon";
      thumb.textContent = "📄";
    }

    /* 文件名：点击直接下载 */
    const name = document.createElement("a");
    name.className = "fname";
    name.textContent = f.name;
    name.href = f.url;
    name.setAttribute("download", f.name);
    name.title = "点击下载 " + f.name;

    const size = document.createElement("span");
    size.className = "fsize";
    size.textContent = fmtSize(f.size);

    /* 下载按钮 */
    const dl = document.createElement("a");
    dl.className = "fbtn";
    dl.textContent = "⬇";
    dl.href = f.url;
    dl.setAttribute("download", f.name);
    dl.title = "下载 " + f.name;

    const del = document.createElement("button");
    del.className = "fbtn del";
    del.textContent = "✕";
    del.title = "删除此附件";
    del.onclick = async () => {
      if (!confirm("删除附件 " + f.name + "？笔记正文中的引用会变成死链。")) return;
      await fetch("/api/" + encodeURIComponent(slug) + "/file/" + encodeURIComponent(f.name), { method: "DELETE" });
      refreshFiles();
    };

    li.append(thumb, name, size, dl, del);
    ul.appendChild(li);
  }
}
