const STORAGE_KEY = "ebook_api_tester_v1";

const state = {
  config: loadConfig(),
  providers: [],
  currentPage: 1,
  totalPages: 1,
  lastQuery: null,
};

function loadConfig() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw);
      return {
        apiKey: parsed.apiKey || "",
        provider: parsed.provider || "zlib",
        domains: parsed.domains || {},
        tokens: parsed.tokens || {},
        accounts: parsed.accounts || {},
      };
    }
  } catch (_) {}
  return { apiKey: "", provider: "zlib", domains: {}, tokens: {}, accounts: {} };
}

function saveConfig() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(state.config));
}

function $(id) {
  return document.getElementById(id);
}

function currentProviderID() {
  return state.config.provider || "zlib";
}

function currentProviderInfo() {
  return state.providers.find((p) => p.id === currentProviderID()) || null;
}

function showToast(msg, type = "info") {
  const el = $("toast");
  el.textContent = msg;
  el.className = `toast ${type}`;
  el.classList.remove("hidden");
}

function hideToast() {
  $("toast").classList.add("hidden");
}

function setDebug(title, data) {
  const text = typeof data === "string" ? data : JSON.stringify(data, null, 2);
  $("debugOutput").textContent = `// ${title}\n${text}`;
}

function buildHeaders(providerID, includeToken = true) {
  const headers = {};
  if (state.config.apiKey) {
    headers["X-API-Key"] = state.config.apiKey;
  }
  if (providerID) {
    const domain = state.config.domains[providerID];
    if (domain) headers["X-Provider-Domain"] = domain;
    if (includeToken && state.config.tokens[providerID]) {
      headers["X-Provider-Token"] = state.config.tokens[providerID];
    }
  }
  return headers;
}

async function apiJSON(method, path, { body, providerID, includeToken = true } = {}) {
  const headers = buildHeaders(providerID, includeToken);
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  const resp = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await resp.text();
  let data;
  try {
    data = JSON.parse(text);
  } catch {
    data = { raw: text };
  }
  setDebug(`${method} ${path} -> HTTP ${resp.status}`, data);
  if (!resp.ok) {
    const msg = data?.error?.message || `HTTP ${resp.status}`;
    throw new Error(msg);
  }
  return data;
}

async function checkHealth() {
  const badge = $("healthBadge");
  try {
    const data = await apiJSON("GET", "/health", { includeToken: false });
    badge.textContent = `在线 · v${data.version} (${(data.providers || []).join(", ")})`;
    badge.className = "badge badge-ok";
  } catch (err) {
    badge.textContent = "服务不可达";
    badge.className = "badge badge-err";
    showToast(`健康检查失败：${err.message}`, "err");
  }
}

async function loadProviders() {
  try {
    const data = await apiJSON("GET", "/v1/providers", { includeToken: false });
    state.providers = data.providers || [];
    if (!state.providers.some((p) => p.id === state.config.provider) && state.providers.length > 0) {
      state.config.provider = data.default_provider || state.providers[0].id;
      saveConfig();
    }
    renderProviders();
  } catch (err) {
    showToast(`加载书源失败：${err.message}`, "err");
  }
}

function renderProviders() {
  const sel = $("providerSelect");
  sel.innerHTML = "";
  for (const p of state.providers) {
    const opt = document.createElement("option");
    opt.value = p.id;
    opt.textContent = `${p.name} (${p.id})`;
    sel.appendChild(opt);
  }
  sel.value = currentProviderID();
  renderProviderDetail();
}

function renderProviderDetail() {
  const info = currentProviderInfo();
  const metaEl = $("providerMeta");
  const domSel = $("domainSelect");
  const sealBox = $("sealDynamicFields");

  if (!info) {
    metaEl.textContent = "无可用书源";
    return;
  }

  metaEl.innerHTML = `
    <div><strong>${escapeHTML(info.description || info.name)}</strong></div>
    <div>能力：${(info.capabilities || []).join(" · ")}</div>
    <div>需要凭据：${info.auth_required ? "是" : "否"} ｜ 服务端默认凭据：${info.default_configured ? "已配置" : "未配置"}</div>
  `;

  // Domains
  domSel.innerHTML = "";
  const domains = info.domains || [];
  if (domains.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = "（无需配置域名）";
    domSel.appendChild(opt);
  } else {
    for (const d of domains) {
      const opt = document.createElement("option");
      opt.value = d;
      opt.textContent = d;
      domSel.appendChild(opt);
    }
    domSel.value = state.config.domains[info.id] || info.default_domain || domains[0];
  }

  // Dynamic seal fields
  sealBox.innerHTML = "";
  const keys = (info.credential_keys || []).filter((k) => k !== "domain");
  if (keys.length === 0) {
    sealBox.innerHTML = `<div class="hint">该书源无需凭据</div>`;
  } else {
    for (const k of keys) {
      const wrap = document.createElement("div");
      wrap.className = "field";
      wrap.innerHTML = `
        <label for="seal_${k}">${escapeHTML(k)}</label>
        <input id="seal_${k}" data-cred-key="${escapeHTML(k)}" type="${k.includes("key") || k.includes("pass") ? "password" : "text"}" placeholder="输入 ${escapeHTML(k)}" required />
      `;
      sealBox.appendChild(wrap);
    }
  }

  renderTokenStatus();
}

function renderTokenStatus() {
  const pid = currentProviderID();
  const token = state.config.tokens[pid] || "";
  const badge = $("tokenBadge");
  $("manualToken").value = token;

  if (token) {
    badge.textContent = "已保存加密令牌";
    badge.className = "badge badge-ok";
  } else {
    badge.textContent = "未配置";
    badge.className = "badge badge-muted";
  }

  const acc = state.config.accounts[pid];
  const accBox = $("accountBox");
  if (acc) {
    accBox.classList.remove("hidden");
    const quota = acc.quota_known
      ? `今日已用 ${acc.daily_used} / ${acc.daily_limit}（剩余 ${acc.daily_remaining}）`
      : "配额未知";
    accBox.innerHTML = `
      <div><strong>账号</strong>：${escapeHTML(acc.email || "—")} ${acc.premium ? '<span class="pill">PRO</span>' : ""}</div>
      <div><strong>额度</strong>：${escapeHTML(quota)}</div>
    `;
  } else {
    accBox.classList.add("hidden");
  }
}

async function handleProbe() {
  const pid = currentProviderID();
  const domain = $("domainSelect").value;
  $("probeResult").textContent = "探测中...";
  try {
    const data = await apiJSON("POST", `/v1/providers/${encodeURIComponent(pid)}/probe`, {
      body: { domain },
      includeToken: false,
    });
    $("probeResult").textContent = data.healthy
      ? `✅ ${data.domain} 可正常访问`
      : `❌ ${data.domain} 不可用或被拦截`;
  } catch (err) {
    $("probeResult").textContent = `探测失败：${err.message}`;
  }
}

async function handleLogin(e) {
  e.preventDefault();
  const pid = currentProviderID();
  const domain = $("domainSelect").value;
  const email = $("loginEmail").value.trim();
  const password = $("loginPassword").value;
  hideToast();
  try {
    const data = await apiJSON("POST", `/v1/providers/${encodeURIComponent(pid)}/login`, {
      body: { domain, email, password },
      includeToken: false,
    });
    state.config.tokens[pid] = data.credential_token;
    if (data.account) state.config.accounts[pid] = data.account;
    saveConfig();
    $("loginPassword").value = "";
    renderTokenStatus();
    showToast("登录成功！加密令牌已保存到 localStorage。", "ok");
  } catch (err) {
    showToast(`登录失败：${err.message}`, "err");
  }
}

async function handleSeal(e) {
  e.preventDefault();
  const pid = currentProviderID();
  const domain = $("domainSelect").value;
  const credentials = {};
  if (domain) credentials.domain = domain;
  document.querySelectorAll("#sealDynamicFields input[data-cred-key]").forEach((input) => {
    const val = input.value.trim();
    if (val) credentials[input.dataset.credKey] = val;
  });
  hideToast();
  try {
    const data = await apiJSON("POST", `/v1/providers/${encodeURIComponent(pid)}/seal`, {
      body: { credentials, verify: $("sealVerify").checked },
      includeToken: false,
    });
    state.config.tokens[pid] = data.credential_token;
    if (data.account) state.config.accounts[pid] = data.account;
    saveConfig();
    document.querySelectorAll("#sealDynamicFields input").forEach((i) => (i.value = ""));
    renderTokenStatus();
    showToast("凭据已加密封包并保存到 localStorage。", "ok");
  } catch (err) {
    showToast(`封包失败：${err.message}`, "err");
  }
}

async function handleVerify() {
  const pid = currentProviderID();
  hideToast();
  try {
    const data = await apiJSON("POST", `/v1/providers/${encodeURIComponent(pid)}/verify`, {
      body: {},
      providerID: pid,
    });
    state.config.accounts[pid] = data.account;
    saveConfig();
    renderTokenStatus();
    showToast("令牌有效，已刷新账号配额。", "ok");
  } catch (err) {
    showToast(`校验失败：${err.message}`, "err");
  }
}

async function runSearch(page = 1) {
  const q = $("searchQ").value.trim();
  if (!q) return;
  hideToast();

  const scope = $("searchScope").value;
  const pid = currentProviderID();
  const ext = splitCSV($("searchExt").value);
  const lang = splitCSV($("searchLang").value);
  const yearFrom = parseInt($("searchYearFrom").value, 10) || 0;
  const yearTo = parseInt($("searchYearTo").value, 10) || 0;
  const limit = parseInt($("searchLimit").value, 10) || 20;

  const payload = {
    q,
    provider: scope === "all" ? "all" : pid,
    page,
    limit,
    ext,
    lang,
    year_from: yearFrom,
    year_to: yearTo,
  };
  if (scope === "all") {
    payload.credential_tokens_by_provider = state.config.tokens;
  } else if (state.config.tokens[pid]) {
    payload.credential_token = state.config.tokens[pid];
  }

  $("resultsContainer").innerHTML = `<div class="results-empty">搜索中...</div>`;
  try {
    const data = await apiJSON("POST", "/v1/search", {
      body: payload,
      providerID: scope === "all" ? null : pid,
      includeToken: false,
    });
    state.currentPage = data.page || page;
    state.totalPages = data.total_pages || 1;
    state.lastQuery = payload;
    renderResults(data);
  } catch (err) {
    $("resultsContainer").innerHTML = `<div class="results-empty">搜索失败：${escapeHTML(err.message)}</div>`;
    showToast(`搜索失败：${err.message}`, "err");
  }
}

function renderResults(data) {
  const books = data.books || [];
  const container = $("resultsContainer");
  $("resultHeading").textContent = `搜索结果（本页 ${books.length} 本）`;

  if (data.total_pages && data.total_pages > 1) {
    $("pager").classList.remove("hidden");
    $("pageInfo").textContent = `${state.currentPage} / ${state.totalPages}`;
    $("btnPrevPage").disabled = state.currentPage <= 1;
    $("btnNextPage").disabled = state.currentPage >= state.totalPages;
  } else {
    $("pager").classList.add("hidden");
  }

  if (books.length === 0) {
    container.innerHTML = `<div class="results-empty">未找到匹配的图书。</div>`;
    return;
  }

  const list = document.createElement("div");
  list.className = "book-list";

  for (const b of books) {
    const item = document.createElement("div");
    item.className = "book-item";
    const coverHTML = b.cover_url
      ? `<img class="book-cover" src="${escapeAttr(b.cover_url)}" alt="" loading="lazy" onerror="this.outerHTML='<div class=book-cover>📖</div>'" />`
      : `<div class="book-cover">📖</div>`;

    const authors = (b.authors || []).join(", ") || "未知作者";
    item.innerHTML = `
      ${coverHTML}
      <div>
        <div class="book-title">${escapeHTML(b.title || "无标题")}</div>
        <div class="book-meta">
          <span class="pill">${escapeHTML(b.provider)}</span>
          ${b.format ? `<span class="pill">${escapeHTML(b.format)}</span>` : ""}
          <span>${escapeHTML(authors)}</span>
          ${b.year ? `<span>${escapeHTML(b.year)}</span>` : ""}
          ${b.size ? `<span>${escapeHTML(b.size)}</span>` : ""}
          ${b.language ? `<span>${escapeHTML(b.language)}</span>` : ""}
        </div>
      </div>
      <div class="book-actions">
        <button class="btn btn-secondary btn-sm" data-act="detail">详情</button>
        <button class="btn btn-primary btn-sm" data-act="download">下载</button>
      </div>
    `;

    item.querySelector('[data-act="detail"]').addEventListener("click", () => openDetail(b));
    item.querySelector('[data-act="download"]').addEventListener("click", (ev) => downloadBook(b, ev.currentTarget));
    list.appendChild(item);
  }

  container.innerHTML = "";
  container.appendChild(list);
}

async function openDetail(book) {
  const dlg = $("detailDialog");
  $("detailTitle").textContent = book.title || "图书详情";
  $("detailBody").innerHTML = "加载中...";
  dlg.showModal();

  try {
    const pid = book.provider;
    const data = await apiJSON(
      "GET",
      `/v1/providers/${encodeURIComponent(pid)}/books/${encodeURIComponent(book.ref)}`,
      { providerID: pid }
    );
    const d = data.book || book;
    $("detailBody").innerHTML = `
      <div class="detail-grid">
        ${d.cover_url ? `<img class="detail-cover" src="${escapeAttr(d.cover_url)}" alt="" />` : `<div class="book-cover">📖</div>`}
        <div>
          <p><strong>作者：</strong>${escapeHTML((d.authors || []).join(", ") || "—")}</p>
          <p><strong>出版社：</strong>${escapeHTML(d.publisher || "—")}</p>
          <p><strong>年份 / 语言：</strong>${escapeHTML(d.year || "—")} / ${escapeHTML(d.language || "—")}</p>
          <p><strong>格式 / 大小：</strong>${escapeHTML(d.format || "—")} / ${escapeHTML(d.size || "—")}</p>
          <p><strong>ISBN：</strong>${escapeHTML(d.isbn || "—")}</p>
          <p><strong>Ref：</strong><code>${escapeHTML(d.ref)}</code></p>
        </div>
      </div>
      ${d.description ? `<div class="detail-desc">${escapeHTML(d.description)}</div>` : ""}
    `;
  } catch (err) {
    $("detailBody").innerHTML = `<p style="color:var(--err)">加载详情失败：${escapeHTML(err.message)}</p>`;
  }
}

async function downloadBook(book, btn) {
  const pid = book.provider;
  const origText = btn.textContent;
  btn.disabled = true;
  btn.textContent = "下载中...";
  hideToast();

  try {
    const resp = await fetch(
      `/v1/providers/${encodeURIComponent(pid)}/books/${encodeURIComponent(book.ref)}/download`,
      { method: "GET", headers: buildHeaders(pid, true) }
    );
    if (!resp.ok) {
      const errData = await resp.json().catch(() => ({}));
      setDebug(`GET download -> HTTP ${resp.status}`, errData);
      throw new Error(errData?.error?.message || `HTTP ${resp.status}`);
    }

    const blob = await resp.blob();
    let filename = `${book.title || book.ref}.${book.format || "bin"}`;
    const xName = resp.headers.get("X-Book-Filename");
    if (xName) {
      try {
        filename = decodeURIComponent(xName);
      } catch (_) {}
    }

    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    showToast(`下载完成：${filename}（${formatBytes(blob.size)}）`, "ok");
  } catch (err) {
    showToast(`下载失败：${err.message}`, "err");
  } finally {
    btn.disabled = false;
    btn.textContent = origText;
  }
}

function splitCSV(str) {
  return str
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MB`;
}

function escapeHTML(str) {
  return String(str)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function escapeAttr(str) {
  return escapeHTML(str).replaceAll("'", "&#39;");
}

function initEvents() {
  $("apiKeyInput").value = state.config.apiKey;
  $("apiKeyInput").addEventListener("input", (e) => {
    state.config.apiKey = e.target.value.trim();
    saveConfig();
  });

  $("providerSelect").addEventListener("change", (e) => {
    state.config.provider = e.target.value;
    saveConfig();
    renderProviderDetail();
  });

  $("domainSelect").addEventListener("change", (e) => {
    state.config.domains[currentProviderID()] = e.target.value;
    saveConfig();
  });

  $("btnRefresh").addEventListener("click", async () => {
    await checkHealth();
    await loadProviders();
  });

  $("btnProbe").addEventListener("click", handleProbe);
  $("loginForm").addEventListener("submit", handleLogin);
  $("sealForm").addEventListener("submit", handleSeal);
  $("btnVerify").addEventListener("click", handleVerify);

  $("btnSaveManualToken").addEventListener("click", () => {
    const pid = currentProviderID();
    const tok = $("manualToken").value.trim();
    if (!tok) {
      showToast("请输入加密令牌", "err");
      return;
    }
    state.config.tokens[pid] = tok;
    saveConfig();
    renderTokenStatus();
    showToast("令牌已保存到 localStorage。", "ok");
  });

  $("btnCopyToken").addEventListener("click", async () => {
    const tok = state.config.tokens[currentProviderID()];
    if (!tok) {
      showToast("当前书源没有可复制的令牌", "err");
      return;
    }
    await navigator.clipboard.writeText(tok);
    showToast("已复制加密令牌到剪贴板。", "ok");
  });

  $("btnClearToken").addEventListener("click", () => {
    const pid = currentProviderID();
    delete state.config.tokens[pid];
    delete state.config.accounts[pid];
    saveConfig();
    renderTokenStatus();
    showToast("已清除当前书源本地令牌。", "info");
  });

  // Tabs
  document.querySelectorAll(".tab").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".tab").forEach((b) => b.classList.remove("active"));
      document.querySelectorAll(".tab-panel").forEach((p) => p.classList.remove("active"));
      btn.classList.add("active");
      const name = btn.dataset.tab;
      const panelId = name === "login" ? "loginForm" : name === "seal" ? "sealForm" : "manualPanel";
      $(panelId).classList.add("active");
    });
  });

  $("searchForm").addEventListener("submit", (e) => {
    e.preventDefault();
    runSearch(1);
  });

  $("btnPrevPage").addEventListener("click", () => {
    if (state.currentPage > 1) runSearch(state.currentPage - 1);
  });
  $("btnNextPage").addEventListener("click", () => {
    if (state.currentPage < state.totalPages) runSearch(state.currentPage + 1);
  });

  $("btnCloseDialog").addEventListener("click", () => $("detailDialog").close());
}

(async function boot() {
  initEvents();
  await checkHealth();
  await loadProviders();
})();
