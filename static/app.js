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

/* 弹回登录页的统一入口：任何地方发现会话失效都走这里 */
function showLoginScreen(msg) {
  $("home").classList.add("hidden");
  $("editor").classList.add("hidden");
  $("login").classList.remove("hidden");
  if (msg) {
    const el = $("loginError");
    el.textContent = msg;
    el.style.display = "";
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
    /* 只加载本站附件与 data: 图片，外链图片不请求 */
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

  setupSplitters();
  applySavedWidths();
  setupSwap();

  /* 宽屏默认分屏：粘贴图片/文字当场就能看到效果 */
  setPreviewMode(window.innerWidth > 860 ? 1 : 0);

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

async function load() {
  const r = await fetch("/api/" + encodeURIComponent(slug));
  if (r.status === 401) {  // 会话过期 → 回到登录页
    showLoginScreen("登录已超时，请重新登录");
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
    if (r.status === 401) {
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
      const resp = await fetch("/api/" + encodeURIComponent(slug) + "/file/" + encodeURIComponent(f.name), { method: "DELETE" });
      if (resp.status === 401) { showLoginScreen("登录已超时，请重新登录"); return; }
      refreshFiles();
    };

    li.append(idx, thumb, name, size, dl, del);
    ul.appendChild(li);
  }
}

/* ---------- 文件互传（服务器中转） ---------- */

/* 弹窗开关 / 标签页 */
function setupSwap() {
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

  $("btnSwap").onclick = () => { $("swapMask").classList.remove("hidden"); loadRecvList(); };
  $("swapClose").onclick = () => $("swapMask").classList.add("hidden");
  $("swapMask").addEventListener("click", (e) => {
    if (e.target === $("swapMask")) $("swapMask").classList.add("hidden");
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !$("swapMask").classList.contains("hidden")) {
      $("swapMask").classList.add("hidden");
    }
  });

  const setTab = (send) => {
    $("tabSend").classList.toggle("active", send);
    $("tabRecv").classList.toggle("active", !send);
    $("paneSend").classList.toggle("hidden", !send);
    $("paneRecv").classList.toggle("hidden", send);
    if (!send) loadRecvList();
  };
  $("tabSend").onclick = () => setTab(true);
  $("tabRecv").onclick = () => setTab(false);

  $("btnPickFile").onclick = () => $("swapFile").click();
  $("swapFile").addEventListener("change", (e) => {
    const f = e.target.files[0];
    e.target.value = "";
    if (f) startSend(f);
  });
  $("btnCopyCode").onclick = () => copyText($("sendCode").textContent);
  $("btnQueryCode").onclick = queryCode;
  $("swapCode").addEventListener("keydown", (e) => { if (e.key === "Enter") queryCode(); });
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

  $("sendIdle").classList.add("hidden");
  $("sendDone").classList.add("hidden");
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
        if (!r.ok) throw new Error(d.error || "http " + r.status);
        offset = typeof d.offset === "number" ? d.offset : end;
        fails = 0;
      } catch (err) {
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
    $("sendDone").classList.remove("hidden");
    $("sendCode").textContent = code;
    $("sendExpires").textContent = "接收方可在「接收文件」列表里直接看到该文件；到期后服务器自动清理。";
  } catch (err) {
    $("sendBusy").classList.add("hidden");
    $("sendIdle").classList.remove("hidden");
    toast("发送中断：" + (err.message || "网络错误") + "（进度已保留，重选同一文件可续传）");
  }
}

/* 接收：待取列表 + 取件码查询 */
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
      info.title = "取件码 " + it.code;
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

async function queryCode() {
  const code = $("swapCode").value.trim();
  if (!code) { toast("请输入取件码"); return; }
  try {
    const r = await fetch("/api/transfer/info?code=" + encodeURIComponent(code));
    if (r.status === 401) { showLoginScreen("登录已超时，请重新登录"); return; }
    const d = await r.json();
    if (!r.ok) { toast(d.error || "取件码不存在"); return; }
    if (!d.ready) { toast("该文件尚未发送完成"); return; }
    downloadTransfer(d.code, d.name, d.size);
  } catch {
    toast("查询失败，请重试");
  }
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
