const tg = window.Telegram.WebApp;
tg.ready();
tg.expand();
// ── Темы оформления ──────────────────────────────────────────────────────────
// Тема — класс на <body> (стили в style.css) + подстройка фона/шапки Telegram.

const THEMES = {
  obsidian: { bg: "#07080b" },
  neo:      { bg: "#1a1c22" },
};
let theme = localStorage.getItem("theme");
if (!THEMES[theme]) theme = "obsidian";

// Фон под нативные элементы Telegram. На старых клиентах hex не
// поддерживается — падаем в try/catch на безопасный ключ темы.
function applyTgColors() {
  const c = THEMES[theme].bg;
  try { tg.setBackgroundColor(c); } catch (e) { /* старый клиент */ }
  try { tg.setHeaderColor(c); } catch (e) {
    try { tg.setHeaderColor("secondary_bg_color"); } catch (e2) { /* старый клиент */ }
  }
}

function applyTheme() {
  document.body.classList.toggle("neo", theme === "neo");
  document.querySelectorAll(".theme-opt").forEach((b) =>
    b.classList.toggle("active", b.dataset.theme === theme));
  applyTgColors();
  // ширины элементов зависят от шрифта темы
  moveChipGlider();
  moveTabGlider();
}

// В полноэкранном режиме контент уходит под статус-бар и кнопки Telegram.
// Складываем safeAreaInset (чёлка/часы) + contentSafeAreaInset (кнопки
// Telegram) в CSS-переменную --safe-top. В обычном режиме оба нулевые.
function applySafeArea() {
  const sa = tg.safeAreaInset, csa = tg.contentSafeAreaInset;
  if (!sa && !csa) return; // старый клиент — остаётся env() из CSS
  const top = ((sa && sa.top) || 0) + ((csa && csa.top) || 0);
  document.documentElement.style.setProperty("--safe-top", top + "px");
}
try {
  tg.onEvent("safeAreaChanged", applySafeArea);
  tg.onEvent("contentSafeAreaChanged", applySafeArea);
  tg.onEvent("fullscreenChanged", applySafeArea);
  applySafeArea();
} catch (e) { /* старые клиенты без safe-area API */ }

const $ = (id) => document.getElementById(id);
const haptic = (style = "light") => {
  try { tg.HapticFeedback.impactOccurred(style); } catch (e) { /* не критично */ }
};

async function api(path, body) {
  const opts = { headers: { Authorization: "tma " + tg.initData } };
  if (body !== undefined) {
    opts.method = "POST";
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  if (!resp.ok) {
    const respBody = await resp.json().catch(() => ({}));
    throw new Error(respBody.error || resp.status);
  }
  return resp.json();
}

// ── Дзен-режим ───────────────────────────────────────────────────────────────
// Прячет деньги и нереализованный PnL, приглушает цвета, тормозит live-поток.
// Информация о здоровье системы (винрейт vs бэктест) остаётся всегда.

let zen = localStorage.getItem("zen") === "1";
const ZEN_RENDER_EVERY = 60000;

// ── Форматирование ───────────────────────────────────────────────────────────

const fmt = (n, d = 2) =>
  n == null ? "—" : n.toLocaleString("ru-RU", { minimumFractionDigits: d, maximumFractionDigits: d });

const fmtPrice = (n) => {
  if (n == null || n === 0) return "—";
  const d = n >= 100 ? 2 : n >= 1 ? 4 : 6;
  return n.toLocaleString("ru-RU", { maximumFractionDigits: d });
};

const pnlSpan = (v, suffix = "$", d = 2) => {
  const cls = v >= 0 ? "pnl-pos" : "pnl-neg";
  const sign = v >= 0 ? "+" : "";
  return `<span class="${cls}">${sign}${fmt(v, d)}${suffix}</span>`;
};

const fmtTime = (iso) => {
  const d = new Date(iso);
  return d.toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit" }) +
    " " + d.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" });
};

const fmtDate = (iso) =>
  new Date(iso).toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit" });

const fmtDur = (a, b) => {
  const mins = Math.round((new Date(b) - new Date(a)) / 60000);
  if (mins < 60) return mins + " мин";
  if (mins < 48 * 60) return (mins / 60).toFixed(1).replace(".0", "") + " ч";
  return Math.round(mins / 1440) + " дн";
};

const isLong = (dir) => {
  const d = (dir || "").toLowerCase();
  return d === "long" || d === "buy";
};
const dirBadge = (dir) => {
  const long = isLong(dir);
  return `<span class="dir ${long ? "long" : "short"}">${long ? "LONG" : "SHORT"}</span>`;
};

// ── Обзор ────────────────────────────────────────────────────────────────────

let lastOv = null;
let lastStatsData = null;
let lastEquityPts = null;
let lastZenRender = 0;
let lastEquityVal = null; // для подсветки изменения equity в hero

function renderOverview(ov) {
  lastOv = ov;
  lastZenRender = Date.now();
  if (zen) {
    renderHeroZen(ov);
  } else {
    renderBalance(ov.balance);
  }
  renderPositions(ov.positions);
  $("updated-text").textContent = "Обновлено " + new Date(ov.updatedAt).toLocaleTimeString("ru-RU");
}

function renderBalance(b) {
  // Главная цифра под Equity — реализованный PnL (свершившийся итог).
  // Нереализованный демотирован в сетку: это живой колеблющийся показатель.
  $("balance-card").innerHTML = `
    <div class="hero-label">Equity</div>
    <div class="hero-equity">$${fmt(b.equity)}</div>
    ${realizedBadge()}
    <div class="hero-grid">
      <div><div class="lbl">Баланс кошелька</div><div class="val">$${fmt(b.walletBalance)}</div></div>
      <div><div class="lbl">Доступно</div><div class="val">$${fmt(b.available)}</div></div>
      <div><div class="lbl">Нереализ. PnL</div><div class="val">${pnlSpan(b.unrealisedPnl)}</div></div>
      <div><div class="lbl">В позициях</div><div class="val">$${fmt(b.walletBalance - b.available)}</div></div>
    </div>`;
  flashEquity(b.equity);
}

// flashEquity — короткая зелёная/красная вспышка большой цифры equity, когда
// она изменилась между обновлениями (живой WS-тик). В дзене не мигаем.
function flashEquity(val) {
  if (zen) { lastEquityVal = val; return; }
  const el = $("balance-card").querySelector(".hero-equity");
  if (el && lastEquityVal != null && val !== lastEquityVal) {
    const cls = val > lastEquityVal ? "flash-up" : "flash-down";
    el.classList.remove("flash-up", "flash-down");
    void el.offsetWidth; // рестарт анимации
    el.classList.add(cls);
  }
  lastEquityVal = val;
}

// realizedBadge — крупный «герой»-бейдж с реализованным PnL из последней
// загруженной статистики. Возвращает "" пока статистики нет.
function realizedBadge() {
  const rp = lastStatsData ? lastStatsData.realizedPnl : null;
  if (rp == null) return "";
  const up = rp >= 0;
  return `<span class="hero-pnl ${up ? "up" : "down"}">` +
    `Реализ. PnL ${up ? "▲" : "▼"} ${fmt(Math.abs(rp))}$</span>`;
}

function renderHeroZen(ov) {
  const fresh = Date.now() - new Date(ov.updatedAt) < 5 * 60 * 1000;
  $("balance-card").innerHTML = `
    <div class="hero-label">Дзен-режим</div>
    <div class="hero-equity zen-title">${fresh ? "Алгоритм работает" : "Нет свежих данных"}</div>
    ${realizedBadge()}
    <div class="hero-grid">
      <div><div class="lbl">Открытых позиций</div><div class="val">${ov.positions.length}</div></div>
      <div><div class="lbl">Данные</div><div class="val">${new Date(ov.updatedAt).toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" })}</div></div>
    </div>`;
}

function renderPositions(positions) {
  $("pos-count").textContent = positions.length || "";
  if (!positions.length) {
    $("positions").innerHTML = `<div class="muted">Нет открытых позиций</div>`;
    return;
  }
  $("positions").innerHTML = positions.map((p) => {
    const long = isLong(p.side);
    const margin = p.leverage > 0 ? (p.size * p.entryPrice) / p.leverage : 0;
    const roe = margin > 0 ? (p.unrealisedPnl / margin) * 100 : 0;
    const value = p.size * p.markPrice;

    let range = "";
    if (p.stopLoss > 0 && p.takeProfit > 0 && p.stopLoss !== p.takeProfit) {
      const frac = Math.min(1, Math.max(0,
        (p.markPrice - p.stopLoss) / (p.takeProfit - p.stopLoss)));
      range = `
        <div class="range">
          <div class="range-bar"><div class="range-mark" style="left:${(frac * 100).toFixed(1)}%"></div></div>
          <div class="range-labels">
            <span>SL ${fmtPrice(p.stopLoss)}</span>
            <span>TP ${fmtPrice(p.takeProfit)}</span>
          </div>
        </div>`;
    } else {
      range = `
        <div class="pos-detail">
          <span>SL <b>${fmtPrice(p.stopLoss)}</b></span>
          <span>TP <b>${fmtPrice(p.takeProfit)}</b></span>
        </div>`;
    }

    // В дзене — без нереализованного PnL и денежных сумм: позиция закроется
    // по SL/TP сама, эта цифра не требует решений.
    const pnlBlock = zen ? "" : `
      <span class="pos-pnl">${pnlSpan(p.unrealisedPnl)}
        <span class="roe">ROE ${pnlSpan(roe, "%", 1)}</span>
      </span>`;
    const valueBlock = zen ? "" : `<span>Объём <b>$${fmt(value, 0)}</b></span>`;

    return `
      <div class="pos ${long ? "long" : "short"}">
        <div class="pos-head">
          <span class="pos-symbol">${p.symbol}${dirBadge(p.side)}
            <span class="badge">${fmt(p.leverage, 0)}x</span>
          </span>
          ${pnlBlock}
        </div>
        <div class="pos-detail">
          <span>Вход <b>$${fmtPrice(p.entryPrice)}</b></span>
          <span>Марк <b>$${fmtPrice(p.markPrice)}</b></span>
          ${valueBlock}
        </div>
        ${range}
      </div>`;
  }).join("");
}

const statCell = (v, l) => `<div class="stat"><div class="v">${v}</div><div class="l">${l}</div></div>`;

function renderStats(st) {
  $("stat-grid").innerHTML =
    statCell(st.winRate != null ? fmt(st.winRate, 0) + "%" : "—", "Винрейт") +
    statCell(`${st.closed}<small style="color:var(--hint)">+${st.open}</small>`, "Сделок") +
    statCell(pnlSpan(st.totalPct, "%", 1), "Σ PnL") +
    statCell(pnlSpan(st.avgPct, "%", 1), "Сред.");
  renderExpectation(st);
}

// renderExpectation compares the live winrate with the backtest expectation.
// This stays visible in zen mode: it is the rational "do I need to look at
// the system" signal, not an emotional one.
function renderExpectation(st) {
  const el = $("expectation");
  if (st.expWinRate == null || !st.closed) {
    el.style.display = "none";
    return;
  }
  const ok = st.winRateP == null || st.winRateP >= 0.05;
  el.style.display = "";
  el.className = "expectation " + (ok ? "calm" : "warn");
  const exp = `Бэктест: винрейт ${fmt(st.expWinRate, 0)}%` +
    (st.expAvgPnl != null ? `, сред. ${fmt(st.expAvgPnl, 1)}%` : "");
  const now = `Сейчас ${fmt(st.winRate, 0)}% на ${st.closed} сделках`;
  const verdict = ok
    ? "отклонение в пределах случайности — вмешательство не требуется"
    : "ниже ожидаемого диапазона — стоит проверить систему";
  el.innerHTML = `<b>${exp}.</b> ${now} — ${verdict}.`;
}

function renderTradeStats(st) {
  $("trade-stats").innerHTML =
    statCell(st.winRate != null ? fmt(st.winRate, 0) + "%" : "—", "Винрейт") +
    statCell(String(st.closed), "Закрыто") +
    statCell(pnlSpan(st.bestPct, "%", 1), "Лучшая") +
    statCell(pnlSpan(st.worstPct, "%", 1), "Худшая");
}

async function loadOverview() {
  try {
    renderOverview(await api("/api/overview"));
  } catch (e) {
    $("balance-card").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

async function loadStats() {
  try {
    const st = await api("/api/stats");
    lastStatsData = st;
    renderStats(st);
    if (loaded.trades) renderTradeStats(st);
    // Реализованный PnL живёт в герое — перерисуем его свежими данными.
    if (lastOv) renderOverview(lastOv);
  } catch (e) { /* статистика не критична */ }
}

// ── Живые обновления по WebSocket ────────────────────────────────────────────

let wsLive = false;
let wsRetry = 2000;

function connectLive() {
  if (!tg.initData) return; // вне Telegram остаёмся на поллинге
  let ws;
  try {
    ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
  } catch (e) { return; }

  ws.onopen = () => ws.send(JSON.stringify({ initData: tg.initData }));
  ws.onmessage = (ev) => {
    let ov;
    try { ov = JSON.parse(ev.data); } catch (e) { return; }
    wsLive = true;
    wsRetry = 2000;
    $("live-dot").classList.toggle("on", !zen);
    lastOv = ov;
    // В дзене не тикаем каждые 0.7с — перерисовка раз в минуту.
    if (zen && Date.now() - lastZenRender < ZEN_RENDER_EVERY) return;
    renderOverview(ov);
  };
  ws.onclose = () => {
    wsLive = false;
    $("live-dot").classList.remove("on");
    wsRetry = Math.min(wsRetry * 2, 30000);
    setTimeout(connectLive, wsRetry);
  };
  ws.onerror = () => ws.close();
}

// ── График equity ────────────────────────────────────────────────────────────

// Функция расчета контрольных точек для сглаживания линии Безье
function getCurvePaths(vals, x, y, y0Val) {
  const points = vals.map((v, i) => ({ x: x(i), y: y(v) }));
  if (points.length === 0) return { line: "", area: "" };
  if (points.length === 1) {
    return {
      line: `M ${points[0].x.toFixed(1)} ${points[0].y.toFixed(1)}`,
      area: `M ${points[0].x.toFixed(1)} ${y0Val.toFixed(1)} L ${points[0].x.toFixed(1)} ${points[0].y.toFixed(1)} Z`
    };
  }

  const controlPoint = (current, previous, next, reverse) => {
    const p = previous || current;
    const n = next || current;
    
    const lengthX = n.x - p.x;
    const lengthY = n.y - p.y;
    const speed = 0.16; // коэффициент сглаживания
    const angle = Math.atan2(lengthY, lengthX) + (reverse ? Math.PI : 0);
    const length = Math.sqrt(lengthX * lengthX + lengthY * lengthY) * speed;

    const rx = current.x + Math.cos(angle) * length;
    const ry = current.y + Math.sin(angle) * length;
    return [rx, ry];
  };

  let linePath = `M ${points[0].x.toFixed(1)} ${points[0].y.toFixed(1)}`;
  for (let i = 1; i < points.length; i++) {
    const p = points[i];
    const prev = points[i - 1];
    
    const [cp1x, cp1y] = controlPoint(prev, points[i - 2], p, false);
    const [cp2x, cp2y] = controlPoint(p, prev, points[i + 1], true);
    
    linePath += ` C ${cp1x.toFixed(1)},${cp1y.toFixed(1)} ${cp2x.toFixed(1)},${cp2y.toFixed(1)} ${p.x.toFixed(1)},${p.y.toFixed(1)}`;
  }

  const areaPath = `M ${points[0].x.toFixed(1)} ${y0Val.toFixed(1)} L ${points[0].x.toFixed(1)} ${points[0].y.toFixed(1)} ` + 
    linePath.substring(linePath.indexOf("C")) + 
    ` L ${points[points.length - 1].x.toFixed(1)} ${y0Val.toFixed(1)} Z`;

  return { line: linePath, area: areaPath };
}

function renderEquity(pts) {
  lastEquityPts = pts;
  const svg = $("equity-chart");
  if (!svg) return;
  if (pts.length < 2) {
    $("equity-wrap").innerHTML = `<div class="muted">Недостаточно закрытых сделок для графика</div>`;
    return;
  }
  const W = 340, H = 140, padX = 4, padT = 12, padB = 6;
  const vals = pts.map((p) => p.cumPct);
  const last = vals[vals.length - 1];
  const min = Math.min(0, ...vals), max = Math.max(0, ...vals);
  const x = (i) => padX + (i / (pts.length - 1)) * (W - 2 * padX);
  const y = (v) => H - padB - ((v - min) / (max - min || 1)) * (H - padT - padB);

  // В дзене линия нейтрального цвета: тренд виден, окраски «хорошо/плохо» нет.
  const color = zen ? "var(--accent)" : (last >= 0 ? "var(--green)" : "var(--red)");
  
  const { line: linePath, area: areaPath } = getCurvePaths(vals, x, y, y(0));

  svg.innerHTML = `
    <defs>
      <linearGradient id="eq-fill" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="${color}" stop-opacity="0.35"/>
        <stop offset="100%" stop-color="${color}" stop-opacity="0.02"/>
      </linearGradient>
    </defs>
    <line x1="0" y1="${y(0)}" x2="${W}" y2="${y(0)}"
      stroke="var(--hint)" stroke-width="0.6" stroke-dasharray="4 4" opacity="0.6"/>
    <path d="${areaPath}" fill="url(#eq-fill)"/>
    <path class="eq-line" d="${linePath}" fill="none" stroke="${color}" stroke-width="2"
      stroke-linejoin="round" stroke-linecap="round"/>
    <circle class="eq-dot" cx="${x(vals.length - 1)}" cy="${y(last)}" r="3.5" fill="${color}"
      stroke="var(--bg)" stroke-width="1.5"/>`;

  // Прорисовка линии «осциллографом» при каждом рендере графика.
  const path = svg.querySelector(".eq-line");
  if (path && path.getTotalLength) {
    const len = path.getTotalLength();
    path.style.transition = "none";
    path.style.strokeDasharray = len;
    path.style.strokeDashoffset = len;
    void path.getBoundingClientRect(); // форсируем reflow до анимации
    path.style.transition = "stroke-dashoffset 1.1s var(--ease, ease)";
    path.style.strokeDashoffset = "0";
  }

  $("equity-last").innerHTML = pnlSpan(last, "%", 1);
  $("equity-axis").innerHTML = `
    <span>${fmtDate(pts[0].time)}</span>
    <span>макс ${fmt(Math.max(...vals), 1)}% · мин ${fmt(Math.min(...vals), 1)}%</span>
    <span>${fmtDate(pts[pts.length - 1].time)}</span>`;
}

async function loadEquity() {
  try {
    renderEquity(await api("/api/equity"));
  } catch (e) { /* график не критичен */ }
}

// ── Сделки ───────────────────────────────────────────────────────────────────

let allTrades = [];
let tradeFilter = "all";

function renderTrades() {
  const list = allTrades.filter((t) => {
    if (tradeFilter === "win") return t.pnlPct != null && t.pnlPct > 0;
    if (tradeFilter === "loss") return t.pnlPct != null && t.pnlPct <= 0;
    if (tradeFilter === "open") return t.status !== "closed";
    return true;
  });
  $("trades-list").innerHTML = list.length
    ? list.map((t) => {
      const dur = t.closedAt ? fmtDur(t.openedAt, t.closedAt) : "";
      const usd = !zen && t.pnl != null ? " " + pnlSpan(t.pnl) : "";
      return `
      <div class="row">
        <div class="main">
          <div><b>${t.symbol}</b>${dirBadge(t.direction)}
            ${t.status !== "closed" ? `<span class="badge accent">открыта</span>` : ""}
          </div>
          <div class="sub">$${fmtPrice(t.entry)} → ${t.closePrice ? "$" + fmtPrice(t.closePrice) : "…"}</div>
        </div>
        <div class="right">
          <div>${t.pnlPct != null ? pnlSpan(t.pnlPct, "%") : "—"}${usd}</div>
          <div class="sub">${fmtTime(t.closedAt || t.openedAt)}${dur && dur !== "0 мин" ? " · " + dur : ""}</div>
        </div>
      </div>`;
    }).join("")
    : `<div class="muted">Ничего не найдено</div>`;
}

async function loadTrades() {
  try {
    allTrades = await api("/api/trades?limit=100");
    renderTrades();
    if (lastStatsData) renderTradeStats(lastStatsData);
  } catch (e) {
    $("trades-list").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

// лунка выбранного фильтра перетекает к активному чипу (только нео-тема,
// в обсидиане #chip-glider скрыт стилями)
function moveChipGlider() {
  const active = document.querySelector("#trade-filters .chip.active");
  const g = $("chip-glider");
  if (!active || !g) return;
  g.style.width = active.offsetWidth + "px";
  g.style.transform = `translate3d(${active.offsetLeft}px, 0, 0)`;
}
window.addEventListener("resize", moveChipGlider);
window.addEventListener("load", moveChipGlider); // после загрузки шрифтов

document.querySelectorAll("#trade-filters .chip").forEach((c) =>
  c.addEventListener("click", () => {
    haptic();
    tradeFilter = c.dataset.f;
    document.querySelectorAll("#trade-filters .chip").forEach((b) =>
      b.classList.toggle("active", b === c));
    moveChipGlider();
    renderTrades();
  }));

// ── Ожидающие лимитные ордеры ────────────────────────────────────────────────

async function loadOrders() {
  try {
    const { orders, ttlDays } = await api("/api/orders");
    $("orders-count").textContent = orders.length || "";
    if (!orders.length) {
      $("orders-list").innerHTML = `<div class="muted">Нет ожидающих ордеров</div>`;
      return;
    }
    $("orders-list").innerHTML = orders.map((o) => {
      // дистанция от текущей цены до входа: ↓ = цене нужно опуститься
      const dist = o.lastPrice > 0 ? ((o.lastPrice - o.price) / o.price) * 100 : null;
      const distStr = dist == null ? "—"
        : `${dist >= 0 ? "↓" : "↑"} ${fmt(Math.abs(dist), 1)}%`;
      const expires = ttlDays > 0
        ? new Date(new Date(o.createdAt).getTime() + ttlDays * 86400000) : null;
      const leftMs = expires ? expires - Date.now() : 0;
      const leftStr = expires
        ? (leftMs <= 0 ? "истёк, скоро снимется"
          : "снимется через " + (leftMs > 48 * 3600000
            ? Math.round(leftMs / 86400000) + " дн"
            : Math.round(leftMs / 3600000) + " ч"))
        : "";
      const notional = o.price * o.qty;
      return `
      <div class="row">
        <div class="main">
          <div><b>${o.symbol}</b>${dirBadge(o.side)}
            <span class="badge">$${fmt(notional, 0)}</span>
          </div>
          <div class="sub">вход $${fmtPrice(o.price)} · сейчас $${fmtPrice(o.lastPrice)}</div>
          <div class="sub">SL $${fmtPrice(o.stopLoss)} · TP $${fmtPrice(o.takeProfit)}</div>
        </div>
        <div class="right">
          <div><span class="badge accent">до входа ${distStr}</span></div>
          <div class="sub">${fmtTime(o.createdAt)}</div>
          <div class="sub">${leftStr}</div>
        </div>
      </div>`;
    }).join("");
  } catch (e) {
    $("orders-list").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

// ── Вкладки, дзен, обновление ────────────────────────────────────────────────

const TABS = ["overview", "trades", "orders"];
const loaded = {};

// Ползунок вкладок позиционируем целыми пикселями по фактической геометрии
// кнопки: проценты от дробной ширины ячейки заставляют мобильный WebKit
// растрировать лунку с тенями на субпиксельной позиции — появляются
// артефакты-«призраки» (заметно на крайней ячейке).
function moveTabGlider() {
  const btn = document.querySelector(".tab.active");
  const g = $("tab-glider");
  if (!btn || !g) return;
  g.style.width = btn.offsetWidth + "px";
  g.style.transform = `translate3d(${btn.offsetLeft}px, 0, 0)`;
}
window.addEventListener("resize", moveTabGlider);
window.addEventListener("load", moveTabGlider); // после загрузки шрифтов

function show(tab) {
  haptic();
  document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  document.querySelectorAll(".page").forEach((p) => p.classList.toggle("active", p.id === tab));
  moveTabGlider();
  if (tab === "trades" && !loaded.trades) { loaded.trades = true; loadTrades(); }
  if (tab === "orders" && !loaded.orders) { loaded.orders = true; loadOrders(); }
  if (tab === "trades") moveChipGlider();
}

document.querySelectorAll(".tab").forEach((b) =>
  b.addEventListener("click", () => show(b.dataset.tab)));

// Свайп влево/вправо по контенту переключает вкладки. Игнорируем жесты,
// начатые на горизонтально скроллящихся фильтрах, и явно вертикальные.
let swipeX = null, swipeY = null;
document.querySelector("main").addEventListener("touchstart", (e) => {
  if (e.target.closest(".filters")) { swipeX = null; return; }
  swipeX = e.touches[0].clientX;
  swipeY = e.touches[0].clientY;
}, { passive: true });
document.querySelector("main").addEventListener("touchend", (e) => {
  if (swipeX == null) return;
  const dx = e.changedTouches[0].clientX - swipeX;
  const dy = e.changedTouches[0].clientY - swipeY;
  swipeX = swipeY = null;
  if (Math.abs(dx) < 60 || Math.abs(dx) < Math.abs(dy) * 2) return;
  const cur = TABS.indexOf(document.querySelector(".tab.active").dataset.tab);
  const next = cur + (dx < 0 ? 1 : -1);
  if (next >= 0 && next < TABS.length) show(TABS[next]);
}, { passive: true });

// Класс .stuck на обёртке вкладок, пока она прилипла к верху: включает
// сплошную подложку, чтобы контент не просвечивал вокруг при скролле.
try {
  const wrapEl = $("tabs-wrap");
  new IntersectionObserver(([e]) =>
    wrapEl.classList.toggle("stuck", !e.isIntersecting)
  ).observe($("tabs-sentinel"));
} catch (e) { /* старый WebView без IntersectionObserver */ }

function applyZen() {
  document.body.classList.toggle("zen", zen);
  $("zen-btn").classList.toggle("active", zen);
  $("live-dot").classList.toggle("on", wsLive && !zen);
  if (lastOv) renderOverview(lastOv);
  if (lastStatsData) renderStats(lastStatsData);
  if (lastEquityPts) renderEquity(lastEquityPts);
  if (loaded.trades) renderTrades();
}

$("zen-btn").addEventListener("click", () => {
  haptic("medium");
  zen = !zen;
  localStorage.setItem("zen", zen ? "1" : "0");
  applyZen();
});

// ── Авторежим (кнопка в шапке = команда /auto в боте) ────────────────────────

let autoMode = null; // null — ещё не загружен, кнопка скрыта

function renderAutoBtn() {
  if (autoMode == null) return;
  const b = $("auto-btn");
  b.style.display = "";
  b.classList.toggle("active", autoMode);
}

async function loadSettings() {
  try {
    autoMode = !!(await api("/api/settings")).auto;
    renderAutoBtn();
  } catch (e) { /* нет доступа к настройкам — кнопка остаётся скрытой */ }
}

$("auto-btn").addEventListener("click", async () => {
  if (autoMode == null) return;
  haptic("medium");
  const want = !autoMode;
  autoMode = want; // оптимистично, откатим при ошибке
  renderAutoBtn();
  try {
    autoMode = !!(await api("/api/settings", { auto: want })).auto;
  } catch (e) {
    autoMode = !want;
  }
  renderAutoBtn();
});

const themeMenu = $("theme-menu");
$("theme-btn").addEventListener("click", () => {
  haptic();
  themeMenu.hidden = !themeMenu.hidden;
});
document.querySelectorAll(".theme-opt").forEach((b) =>
  b.addEventListener("click", () => {
    haptic("medium");
    theme = b.dataset.theme;
    localStorage.setItem("theme", theme);
    applyTheme();
    themeMenu.hidden = true;
  }));
// тап вне меню закрывает его
document.addEventListener("click", (e) => {
  if (!themeMenu.hidden && !e.target.closest(".theme-wrap")) themeMenu.hidden = true;
});

$("refresh").addEventListener("click", async () => {
  haptic("medium");
  const btn = $("refresh");
  btn.classList.add("spin");
  const jobs = [loadOverview(), loadEquity(), loadStats()];
  if (loaded.trades) jobs.push(loadTrades());
  if (loaded.orders) jobs.push(loadOrders());
  await Promise.allSettled(jobs);
  btn.classList.remove("spin");
});

document.body.classList.toggle("zen", zen);
$("zen-btn").classList.toggle("active", zen);
applyTheme();
moveTabGlider();
loadOverview();
loadEquity();
loadStats();
loadSettings();
connectLive();
// Поллинг — только как фолбэк, пока нет живого WS-соединения.
setInterval(() => { if (!wsLive) loadOverview(); }, 10000);
setInterval(loadStats, 60000);
