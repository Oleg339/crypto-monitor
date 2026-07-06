const tg = window.Telegram.WebApp;
tg.ready();
tg.expand();
// ── Темы оформления ──────────────────────────────────────────────────────────
// Тема — класс на <body> (стили в style.css) + подстройка фона/шапки Telegram.

const THEMES = {
  obsidian: { bg: "#07080b" },
  neo:      { bg: "#1a1c22" },
  garden:   { bg: "#0b0f0c" }, // тёмный хром, сама сцена сада — светлая внутри
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
  document.body.classList.toggle("garden", theme === "garden");
  document.querySelectorAll(".theme-opt").forEach((b) =>
    b.classList.toggle("active", b.dataset.theme === theme));
  applyTgColors();
  // ширины элементов зависят от шрифта темы
  moveChipGlider();
  moveTabGlider();
  // сад заменяет обзор — перерисуем текущие данные под новую тему
  if (lastOv) renderOverview(lastOv);
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
  if (theme === "garden") {
    renderGarden(ov);
    $("updated-text").textContent = "Обновлено " + new Date(ov.updatedAt).toLocaleTimeString("ru-RU");
    return;
  }
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

// ══ Сад ═══════════════════════════════════════════════════════════════════════
// Тема-игра: изометрическая грядка, каждая открытая позиция — растение.
//   вид культуры  — детерминированно по тикеру
//   спелость      — прогресс цены к TP → наливающийся плод
//   здоровье      — ROE: в плюсе пышное/цветёт, в минусе вянет/буреет
//   стресс        — прогресс к SL → наклон/увядание
//   флажок        — направление: зелёный ▲ long / красный ▼ short
//   погода сцены  — общий нереализ. PnL: солнце / облака / дождь
//   урожай (HUD)  — реализованный PnL

let lastGardenRender = 0;
const GARDEN_RENDER_EVERY = 4000;
let gardenSelSym = null;

const clamp = (v, a, b) => Math.max(a, Math.min(b, v));
function hashStr(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h;
}
function lerpColor(a, b, t) {
  t = clamp(t, 0, 1);
  const pa = [1, 3, 5].map((i) => parseInt(a.slice(i, i + 2), 16));
  const pb = [1, 3, 5].map((i) => parseInt(b.slice(i, i + 2), 16));
  return "#" + pa.map((v, i) => Math.round(v + (pb[i] - v) * t).toString(16).padStart(2, "0")).join("");
}

// метрики позиции для «садовой» логики
function posMetrics(p) {
  const long = isLong(p.side);
  const margin = p.leverage > 0 ? (p.size * p.entryPrice) / p.leverage : 0;
  const roe = margin > 0 ? (p.unrealisedPnl / margin) * 100 : 0;
  let tp = 0, sl = 0;
  if (p.takeProfit > 0 && p.entryPrice > 0) {
    const d = long ? p.takeProfit - p.entryPrice : p.entryPrice - p.takeProfit;
    if (d > 0) tp = clamp((long ? p.markPrice - p.entryPrice : p.entryPrice - p.markPrice) / d, 0, 1);
  }
  if (p.stopLoss > 0 && p.entryPrice > 0) {
    const d = long ? p.entryPrice - p.stopLoss : p.stopLoss - p.entryPrice;
    if (d > 0) sl = clamp((long ? p.entryPrice - p.markPrice : p.markPrice - p.entryPrice) / d, 0, 1);
  }
  return { long, margin, roe, tp, sl, value: p.size * p.markPrice };
}

function foliage(roe) {
  return roe >= 0
    ? lerpColor("#6cbf5f", "#2f8f43", clamp(roe / 25, 0, 1))   // мягкая → сочная зелень
    : lerpColor("#9fae52", "#9c6b39", clamp(-roe / 25, 0, 1)); // желтеет → буреет
}

// SVG-тело растения (якорь 0,0 — точка почвы, вверх = минус y)
function plantBody(p, m) {
  const fol = foliage(m.roe);
  const folD = lerpColor(fol, "#0b2a12", 0.3);
  const stem = "#7a5c38", stemD = "#5f4227";
  const sp = hashStr(p.symbol) % 6;
  const bloom = m.roe > 2;
  const fruitN = Math.round(m.tp * 3);
  const petal = m.long ? "#ff8fa0" : "#ffd27a";
  const berry = m.long ? "#ff5d6c" : "#ffc24a";
  const leaf = (x, y, r, rot) =>
    `<ellipse cx="${x}" cy="${y}" rx="${r}" ry="${r * 0.5}" fill="${fol}" transform="rotate(${rot} ${x} ${y})"/>`;
  let s = "";
  switch (sp) {
    case 0: // росток
      s += `<rect x="-1.2" y="-22" width="2.4" height="22" rx="1.2" fill="${stem}"/>`;
      s += leaf(-6, -18, 8, 25) + leaf(6, -20, 9, -25);
      if (bloom) s += `<circle cx="0" cy="-24" r="3.4" fill="${petal}"/>`;
      break;
    case 1: // тюльпан
      s += `<rect x="-1.3" y="-30" width="2.6" height="30" rx="1.3" fill="${stem}"/>`;
      s += leaf(-8, -13, 10, 18);
      s += `<path d="M-7 -30 Q-8 -41 0 -43 Q8 -41 7 -30 Q0 -34 -7 -30 Z" fill="${bloom ? petal : fol}"/>`;
      break;
    case 2: // подсолнух
      s += `<rect x="-1.6" y="-40" width="3.2" height="40" rx="1.6" fill="${stem}"/>`;
      s += leaf(-9, -20, 12, 20) + leaf(9, -28, 12, -20);
      s += `<g transform="translate(0,-44)">`;
      for (let a = 0; a < 12; a++) s += `<ellipse cx="0" cy="-9" rx="3" ry="6" fill="#ffcf5d" transform="rotate(${a * 30})"/>`;
      s += `<circle r="7" fill="#7a4a22"/></g>`;
      break;
    case 3: // ягодный куст
      s += `<ellipse cx="0" cy="-14" rx="16" ry="14" fill="${fol}"/>`;
      s += `<ellipse cx="-7" cy="-20" rx="9" ry="8" fill="${lerpColor(fol, "#ffffff", 0.1)}"/>`;
      s += `<ellipse cx="8" cy="-18" rx="9" ry="8" fill="${folD}"/>`;
      for (let k = 0; k < fruitN; k++) s += `<circle cx="${-8 + k * 8}" cy="${-10 - (k % 2) * 6}" r="2.7" fill="${berry}"/>`;
      break;
    case 4: // ёлочка
      s += `<rect x="-2" y="-9" width="4" height="9" fill="${stemD}"/>`;
      s += `<path d="M0 -46 L-13 -24 L13 -24 Z" fill="${fol}"/>`;
      s += `<path d="M0 -36 L-16 -12 L16 -12 Z" fill="${folD}"/>`;
      s += `<path d="M0 -26 L-19 -2 L19 -2 Z" fill="${fol}"/>`;
      for (let k = 0; k < fruitN; k++) s += `<circle cx="${-8 + k * 8}" cy="-8" r="2.5" fill="${berry}"/>`;
      break;
    default: // гриб
      s += `<path d="M-6 0 Q-7 -14 0 -15 Q7 -14 6 0 Z" fill="#efe6d0"/>`;
      s += `<path d="M-16 -13 Q0 -30 16 -13 Q0 -20 -16 -13 Z" fill="${m.long ? "#e0564f" : "#d98a3a"}"/>`;
      for (let k = 0; k < 3; k++) s += `<circle cx="${-8 + k * 8}" cy="-16" r="2" fill="#fff"/>`;
  }
  return s;
}

// изометрическая плитка грядки
function tileSVG(X, Y, occ, TW, TH, D) {
  const top = occ ? "#6fae54" : "#7ea862", topD = "#5d9247";
  const sideL = "#4d7d3a", sideR = "#3f6a30";
  let s = "";
  s += `<polygon points="${X - TW / 2},${Y} ${X},${Y + TH / 2} ${X},${Y + TH / 2 + D} ${X - TW / 2},${Y + D}" fill="${sideL}"/>`;
  s += `<polygon points="${X + TW / 2},${Y} ${X},${Y + TH / 2} ${X},${Y + TH / 2 + D} ${X + TW / 2},${Y + D}" fill="${sideR}"/>`;
  s += `<polygon points="${X},${Y - TH / 2} ${X + TW / 2},${Y} ${X},${Y + TH / 2} ${X - TW / 2},${Y}" fill="${top}" stroke="${topD}" stroke-width="1"/>`;
  if (occ) s += `<ellipse cx="${X}" cy="${Y}" rx="15" ry="6" fill="#6b4f34"/><ellipse cx="${X}" cy="${Y - 1}" rx="11" ry="4" fill="#7c5c3c"/>`;
  return s;
}

function gardenReadoutHTML() {
  const pos = lastOv ? lastOv.positions || [] : [];
  const p = pos.find((x) => x.symbol === gardenSelSym);
  if (!p) return `<span class="gr-hint">🌿 Нажми на растение, чтобы увидеть позицию</span>`;
  const m = posMetrics(p);
  const stage = m.tp > 0.66 ? "почти спелое" : m.tp > 0.33 ? "наливается" : "растёт";
  return `<div class="gr-row"><b>${p.symbol}</b>${dirBadge(p.side)}<span class="badge">${fmt(p.leverage, 0)}x</span>` +
    `<span class="gr-pnl">${pnlSpan(p.unrealisedPnl)} · ROE ${pnlSpan(m.roe, "%", 1)}</span></div>` +
    `<div class="gr-sub">вход $${fmtPrice(p.entryPrice)} · марк $${fmtPrice(p.markPrice)} · до TP ${(m.tp * 100).toFixed(0)}% — ${stage}</div>`;
}

function renderGarden(ov) {
  lastGardenRender = Date.now();
  const el = $("garden");
  if (!el) return;
  const pos = (ov.positions || []).slice(0, 12);
  const bal = ov.balance || {};
  const TW = 86, TH = 44, D = 9;
  const n = pos.length;
  const cols = n <= 1 ? 2 : n <= 6 ? 3 : 4;
  const rows = Math.max(2, Math.ceil(Math.max(n, cols) / cols));
  const marginX = 24, topSpace = 96, bottomSpace = D + 22;
  const originX = marginX + (rows - 1) * TW / 2;
  const originY = topSpace;
  const W = originX + (cols - 1) * TW / 2 + TW / 2 + marginX;
  const H = originY + (cols - 1 + rows - 1) * TH / 2 + TH / 2 + bottomSpace;
  const iso = (c, r) => ({ x: originX + (c - r) * TW / 2, y: originY + (c + r) * TH / 2 });

  const cells = [];
  for (let r = 0; r < rows; r++) for (let c = 0; c < cols; c++) cells.push({ c, r, idx: r * cols + c });
  cells.sort((a, b) => (a.c + a.r) - (b.c + b.r) || a.c - b.c);

  let tiles = "";
  const plants = [];
  cells.forEach((cell) => {
    const { x, y } = iso(cell.c, cell.r);
    const occ = cell.idx < n;
    tiles += tileSVG(x, y, occ, TW, TH, D);
    if (occ) plants.push({ cell, x, y, p: pos[cell.idx], i: cell.idx });
  });
  plants.sort((a, b) => (a.cell.c + a.cell.r) - (b.cell.c + b.cell.r));

  let plantSvg = "";
  plants.forEach((pl) => {
    const m = posMetrics(pl.p);
    const droop = m.sl > 0.45 ? (m.sl - 0.45) * 16 * (m.long ? 1 : -1) : 0;
    const wilt = m.sl > 0.6 || m.roe < -18;
    const sway = (3.4 + (hashStr(pl.p.symbol) % 10) * 0.12).toFixed(2);
    const delay = ((hashStr(pl.p.symbol) % 20) * 0.15).toFixed(2);
    const selRing = pl.p.symbol === gardenSelSym
      ? `<ellipse cx="0" cy="2" rx="20" ry="8" fill="none" stroke="#ffe08a" stroke-width="2"/>` : "";
    plantSvg += `<g class="garden-plant${pl.p.symbol === gardenSelSym ? " sel" : ""}" data-i="${pl.i}" transform="translate(${pl.x.toFixed(1)},${pl.y.toFixed(1)})">`;
    plantSvg += selRing;
    plantSvg += `<ellipse cx="0" cy="-14" rx="22" ry="30" fill="transparent"/>`; // клик-зона
    plantSvg += `<g class="${wilt ? "" : "garden-sway"}" style="animation-duration:${sway}s;animation-delay:${delay}s" transform="rotate(${droop.toFixed(1)})" opacity="${wilt ? 0.72 : 1}">${plantBody(pl.p, m)}</g>`;
    plantSvg += `<g transform="translate(12,-1)"><line x1="0" y1="0" x2="0" y2="-24" stroke="#5b4a34" stroke-width="1.4"/><path d="M0 -24 L9 -21.5 L0 -19 Z" fill="${m.long ? "#37b26a" : "#e0566a"}"/></g>`;
    plantSvg += `</g>`;
  });

  const up = bal.unrealisedPnl || 0;
  const sunny = up > 0.5, storm = up < -20;
  let weather = "";
  if (sunny) {
    weather = `<circle cx="${W - 42}" cy="42" r="22" fill="#ffd873" opacity="0.25"/><circle cx="${W - 42}" cy="42" r="15" fill="#ffd873"/>`;
  } else {
    weather = `<g fill="#cfd6dd"><ellipse cx="${W - 54}" cy="40" rx="18" ry="11"/><ellipse cx="${W - 38}" cy="44" rx="14" ry="9"/></g>`;
    if (storm) for (let d = 0; d < 5; d++) weather += `<line class="garden-rain" x1="${W - 58 + d * 8}" y1="52" x2="${W - 60 + d * 8}" y2="61" stroke="#8fb8d8" stroke-width="1.6" style="animation-delay:${(d * 0.2).toFixed(1)}s"/>`;
  }

  const harvest = lastStatsData ? lastStatsData.realizedPnl : null;
  const weatherLabel = sunny ? "☀ ясно" : storm ? "🌧 буря" : "☁ облачно";
  const hud =
    `<div class="garden-hud">` +
    `<div class="gh"><span class="gh-l">Растений</span><span class="gh-v">${n}</span></div>` +
    `<div class="gh"><span class="gh-l">Погода</span><span class="gh-v">${weatherLabel}</span></div>` +
    `<div class="gh"><span class="gh-l">Урожай</span><span class="gh-v ${harvest != null && harvest < 0 ? "pnl-neg" : "pnl-pos"}">${harvest != null ? (harvest >= 0 ? "+" : "") + fmt(harvest) + "$" : "—"}</span></div>` +
    `</div>`;

  const skyTop = sunny ? "#a6dcf0" : "#9fb4c4", skyBot = sunny ? "#dff0e6" : "#cdd8dc";
  const scene =
    `<div class="garden-plot"><svg viewBox="0 0 ${W.toFixed(0)} ${H.toFixed(0)}" class="garden-svg" preserveAspectRatio="xMidYMax meet">` +
    `<defs><linearGradient id="gsky" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${skyTop}"/><stop offset="1" stop-color="${skyBot}"/></linearGradient></defs>` +
    `<rect x="0" y="0" width="${W}" height="${H}" fill="url(#gsky)"/>${weather}${tiles}${plantSvg}</svg></div>`;

  el.innerHTML = hud + scene + `<div id="garden-readout" class="garden-readout">${gardenReadoutHTML()}</div>`;
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
    // Сад перестраивает SVG — не чаще, чем раз в несколько секунд.
    if (theme === "garden" && Date.now() - lastGardenRender < GARDEN_RENDER_EVERY) return;
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
  // viewBox = фактические пиксели контейнера: масштаб 1:1 по обеим осям,
  // иначе preserveAspectRatio=none плющит круги и тени в эллипсы
  const W = Math.max(200, svg.clientWidth || 340);
  const H = svg.clientHeight || 116;
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  const padX = 4, padT = 12, padB = 6;
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
      <filter id="eq-glow" x="-20%" y="-60%" width="140%" height="220%">
        <feGaussianBlur stdDeviation="3"/>
      </filter>
    </defs>
    <line x1="0" y1="${y(0)}" x2="${W}" y2="${y(0)}"
      stroke="var(--hint)" stroke-width="0.6" stroke-dasharray="4 4" opacity="0.6"/>
    <path d="${areaPath}" fill="url(#eq-fill)"/>
    <path d="${linePath}" fill="none" stroke="${color}" stroke-width="2.5" opacity="0.4"
      filter="url(#eq-glow)" stroke-linejoin="round" stroke-linecap="round"/>
    <path class="eq-line" d="${linePath}" fill="none" stroke="${color}" stroke-width="2"
      stroke-linejoin="round" stroke-linecap="round"/>
    <circle class="eq-dot" cx="${x(vals.length - 1)}" cy="${y(last)}" r="3.5" fill="${color}"
      stroke="var(--bg)" stroke-width="1.5"/>
    <g class="eq-cross" style="display:none">
      <line class="eq-cross-line" x1="0" x2="0" y1="${padT - 8}" y2="${H - padB}"
        stroke="var(--ink-3)" stroke-width="0.8" stroke-dasharray="3 3"/>
      <circle class="eq-cross-dot" r="4" fill="${color}" stroke="var(--bg)" stroke-width="1.5"/>
    </g>`;

  eqGeom = { pts, vals, x, y, W, padX };

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

// ── Скраббинг графика ────────────────────────────────────────────────────────
// Палец/курсор над графиком: перекрестие прыгает по точкам-сделкам (с
// хаптиком), подпись показывает дату, сделку и накопленный итог.

let eqGeom = null;   // геометрия последнего рендера
let eqLastIdx = -1;  // индекс подсвеченной точки — для хаптика по смене

function eqScrub(clientX) {
  const svg = $("equity-chart");
  if (!eqGeom || !svg) return;
  const g = svg.querySelector(".eq-cross");
  const tip = $("eq-tip");
  if (!g || !tip) return;

  const { pts, vals, x, y, W, padX } = eqGeom;
  const r = svg.getBoundingClientRect();
  const sx = (clientX - r.left) * W / r.width;
  const step = (W - 2 * padX) / (pts.length - 1);
  const i = Math.max(0, Math.min(pts.length - 1, Math.round((sx - padX) / step)));

  const px = x(i), py = y(vals[i]);
  g.style.display = "";
  const line = g.querySelector(".eq-cross-line");
  line.setAttribute("x1", px);
  line.setAttribute("x2", px);
  const dot = g.querySelector(".eq-cross-dot");
  dot.setAttribute("cx", px);
  dot.setAttribute("cy", py);

  const p = pts[i];
  const trade = p.symbol
    ? `<b>${p.symbol.replace("USDT", "")}</b> ${pnlSpan(p.pnlPct, "%", 1)} · ` : "";
  tip.innerHTML = `${fmtDate(p.time)} · ${trade}Σ ${pnlSpan(p.cumPct, "%", 1)}`;
  tip.style.display = "";
  const cssX = px / W * r.width;
  tip.style.left = Math.max(0, Math.min(r.width - tip.offsetWidth, cssX - tip.offsetWidth / 2)) + "px";

  if (i !== eqLastIdx) {
    haptic();
    eqLastIdx = i;
  }
}

function eqScrubEnd() {
  eqLastIdx = -1;
  const svg = $("equity-chart");
  const g = svg && svg.querySelector(".eq-cross");
  if (g) g.style.display = "none";
  const tip = $("eq-tip");
  if (tip) tip.style.display = "none";
}

{
  const wrap = $("equity-wrap");
  wrap.addEventListener("pointerdown", (e) => eqScrub(e.clientX));
  wrap.addEventListener("pointermove", (e) => eqScrub(e.clientX));
  wrap.addEventListener("pointerup", eqScrubEnd);
  wrap.addEventListener("pointercancel", eqScrubEnd);
  wrap.addEventListener("pointerleave", eqScrubEnd);
}

// перерисовка под новую ширину (поворот экрана, ресайз окна)
let eqResizeT = 0;
window.addEventListener("resize", () => {
  clearTimeout(eqResizeT);
  eqResizeT = setTimeout(() => { if (lastEquityPts) renderEquity(lastEquityPts); }, 150);
});

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
  // фильтры скроллятся горизонтально, по графику скраббинг — не свайпаем
  if (e.target.closest(".filters") || e.target.closest("#equity-wrap")) { swipeX = null; return; }
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

// Сад: тап по растению — показать позицию в подписи под сценой.
const gardenEl = $("garden");
if (gardenEl) {
  gardenEl.addEventListener("click", (e) => {
    const g = e.target.closest("[data-i]");
    if (!g) return;
    const pos = lastOv ? lastOv.positions || [] : [];
    const p = pos[+g.dataset.i];
    if (!p) return;
    haptic();
    gardenSelSym = gardenSelSym === p.symbol ? null : p.symbol;
    gardenEl.querySelectorAll(".garden-plant.sel").forEach((x) => x.classList.remove("sel"));
    if (gardenSelSym) g.classList.add("sel");
    const ro = $("garden-readout");
    if (ro) ro.innerHTML = gardenReadoutHTML();
  });
}

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
