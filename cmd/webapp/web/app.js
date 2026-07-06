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
  // в саду перетаскивание пальцем не должно закрывать мини-апп
  try { theme === "garden" ? tg.disableVerticalSwipes() : tg.enableVerticalSwipes(); } catch (e) { /* старый клиент */ }
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

// ── Пиксель-арт: спрайты рисуются сеткой «пикселей»-прямоугольников ──────────
// px 1 = один пиксель; вся сцена в пиксель-единицах, SVG масштабируется
// на всю ширину, shape-rendering=crispEdges → чёткие блоки без сглаживания.

const PX = 1.05; // лёгкий нахлёст против швов между «пикселями»
function sprite(rows, pal, ox, oy) {
  let s = "";
  for (let y = 0; y < rows.length; y++) {
    const row = rows[y];
    for (let x = 0; x < row.length; x++) {
      const ch = row[x];
      if (ch === " ") continue;
      const col = pal[ch];
      if (!col) continue;
      s += `<rect x="${ox + x}" y="${oy + y}" width="${PX}" height="${PX}" fill="${col}"/>`;
    }
  }
  return s;
}

// силуэты растений (символы → цвета через палитру плода/листвы)
const SPRITES = {
  0: [ // дуб
    "   lgggl   ", "  glGGGlg  ", " glGGlGGlg ", "glGGlllGGlg",
    "glGGGGGGGlg", " glGGGGGlg ", "  glGGGlg  ", "   gGGGg   ",
    "    TtT    ", "    TtT    ", "    TtT    "],
  1: [ // тюльпан
    " P P P ", " PpPpP ", " PPPPP ", "  PPP  ",
    "   g   ", "  lgl  ", "   g   ", "   g   "],
  2: [ // подсолнух
    "   YYY   ", "  YyByY  ", " YyBBByY ", "  YyByY  ",
    "   YYY   ", "    g    ", "  l g l  ", "    g    ", "    g    "],
  3: [ // ягодный куст
    "  ggggg  ", " glGGGlg ", "glGGlGGlg", "glGGGGGlg", " glGGGlg ", "  gGGGg  "],
  4: [ // ёлочка
    "    l    ", "   gGg   ", "   gGg   ", "  gGGGg  ", "  gGGGg  ",
    " gGGGGGg ", "gGGGGGGGg", "   TtT   ", "   TtT   "],
  5: [ // гриб
    " MMMMM ", "MmwmwmM", "MMmmmMM", "  sss  ", "  sss  ", "  sss  "],
};

// SVG-тело растения (пиксель-спрайт, якорь 0,0 — точка почвы, вверх = −y)
function plantBody(p, m) {
  const base = foliage(m.roe);
  const petal = m.long ? "#ff8fb0" : "#ffcf6a";
  const berry = m.long ? "#ff5d6c" : "#ffbf3c";
  const pal = {
    G: lerpColor(base, "#0a2a12", 0.42), g: base, l: lerpColor(base, "#eafff0", 0.35),
    T: "#7a5535", t: "#573b22",
    P: petal, p: lerpColor(petal, "#ffffff", 0.4),
    Y: "#f4c542", y: "#d79a25", B: "#6e4522",
    r: berry, w: "#f4efe0",
    m: m.long ? "#d8473f" : "#d98a3a", M: m.long ? "#a82f2a" : "#a8641f", s: "#efe6cf",
  };
  const sp = hashStr(p.symbol) % 6;
  const rows = SPRITES[sp];
  const Ws = rows[0].length, Hs = rows.length;
  let s = sprite(rows, pal, -Math.floor(Ws / 2), -Hs);
  // спелость: наливающиеся плоды в кроне
  const fruitN = sp === 1 || sp === 5 ? 0 : Math.round(m.tp * 3);
  for (let k = 0; k < fruitN; k++) {
    const fx = -3 + k * 3, fy = -Hs + 2 + (k % 2);
    s += `<rect x="${fx}" y="${fy}" width="${1.6}" height="${1.6}" fill="${berry}"/>`;
  }
  return s;
}

function gardenReadoutHTML() {
  const pos = lastOv ? lastOv.positions || [] : [];
  const p = pos.find((x) => x.symbol === gardenSelSym);
  if (!p) return `<span class="gr-hint">🌿 Тап — позиция · щипок — зум · двойной тап — сброс</span>`;
  const m = posMetrics(p);
  const stage = m.tp > 0.66 ? "почти спелое" : m.tp > 0.33 ? "наливается" : "растёт";
  return `<div class="gr-row"><b>${p.symbol}</b>${dirBadge(p.side)}<span class="badge">${fmt(p.leverage, 0)}x</span>` +
    `<span class="gr-pnl">${pnlSpan(p.unrealisedPnl)} · ROE ${pnlSpan(m.roe, "%", 1)}</span></div>` +
    `<div class="gr-sub">вход $${fmtPrice(p.entryPrice)} · марк $${fmtPrice(p.markPrice)} · до TP ${(m.tp * 100).toFixed(0)}% — ${stage}</div>`;
}

// одна пиксельная изо-плитка травы с тонким краем (земля)
function drawTile(X, Y, TW, TH, EH, alt) {
  const top = alt ? "#6bae4c" : "#7cc05a";
  const rim = lerpColor(top, "#2f4f22", 0.38);
  const rimL = lerpColor(rim, "#000000", 0.18);
  const dirt = "#caa25a", dirtD = "#a17a3c";
  const hh = TH / 2;
  let s = "";
  for (let dy = -hh; dy <= hh; dy++) {
    const hx = Math.round((TW / 2) * (1 - Math.abs(dy) / hh));
    if (hx <= 0) continue;
    s += `<rect x="${X - hx}" y="${Math.round(Y + dy)}" width="${2 * hx + PX}" height="${PX}" fill="${top}"/>`;
  }
  for (let x = -TW / 2; x <= TW / 2; x++) {
    const by = Math.round(Y + hh * (1 - Math.abs(x) / (TW / 2)));
    const col = X + x, left = x < 0;
    s += `<rect x="${col}" y="${by}" width="${PX}" height="${EH}" fill="${left ? rimL : rim}"/>`;
    s += `<rect x="${col}" y="${by + EH - 1}" width="${PX}" height="${PX + 0.5}" fill="${left ? dirtD : dirt}"/>`;
  }
  return s;
}

// пиксельная плитка воды (пруд) с бликами
function drawWaterTile(X, Y, TW, TH, EH) {
  const top = "#57b4d6", edge = "#3f7f9e", dirt = "#caa25a", dirtD = "#a17a3c";
  const hh = TH / 2;
  let s = "";
  for (let dy = -hh; dy <= hh; dy++) {
    const hx = Math.round((TW / 2) * (1 - Math.abs(dy) / hh));
    if (hx <= 0) continue;
    s += `<rect x="${X - hx}" y="${Math.round(Y + dy)}" width="${2 * hx + PX}" height="${PX}" fill="${top}"/>`;
  }
  s += `<rect class="garden-water" x="${X - 6}" y="${Y - 2}" width="5" height="${PX}" fill="#c3ecf6"/>`;
  s += `<rect class="garden-water" x="${X + 2}" y="${Y + 1}" width="4" height="${PX}" fill="#c3ecf6" style="animation-delay:.9s"/>`;
  for (let x = -TW / 2; x <= TW / 2; x++) {
    const by = Math.round(Y + hh * (1 - Math.abs(x) / (TW / 2)));
    const col = X + x, left = x < 0;
    s += `<rect x="${col}" y="${by}" width="${PX}" height="${EH}" fill="${left ? lerpColor(edge, "#000000", 0.15) : edge}"/>`;
    s += `<rect x="${col}" y="${by + EH - 1}" width="${PX}" height="${PX + 0.5}" fill="${left ? dirtD : dirt}"/>`;
  }
  return s;
}

// декоративная зелень на «пустых» плитках — фон для растений-позиций
const AMB_PAL = {
  G: "#3d8b41", g: "#57a552", l: "#6cb857", T: "#7a5535", t: "#573b22",
  P: "#ff9ab8", p: "#ffd3e0", Y: "#f4c542", y: "#d79a25", B: "#6e4522",
  r: "#e0564f", w: "#ffffff", m: "#d8473f", M: "#a82f2a", s: "#efe6cf",
  c: "#e05a4f", C: "#b5372f",
};
const AMB = {
  tuft: ["l l", "gGg"],
  flower: [" P ", "PpP", " g ", "gGg"],
  coral: [" c c c ", " cCcCc ", "cCcccCc", " CgGgC ", "  gGg  "],
};

function renderGarden(ov) {
  const el = $("garden");
  if (!el) return;
  if (gardenInteracting) return; // не пересобирать SVG посреди жеста
  lastGardenRender = Date.now();
  const shadow = (rx) => `<ellipse cx="0" cy="1.5" rx="${rx}" ry="${(rx * 0.32).toFixed(1)}" fill="#243218" opacity="0.16"/>`;
  let bloom = ""; // слой «свечения» (bloom) — размывается фильтром, blend: screen
  const pos = (ov.positions || []).slice(0, 16);
  const bal = ov.balance || {};
  const TW = 24, TH = 12, EH = 4;
  const n = pos.length;
  const G = Math.max(5, Math.min(6, Math.ceil(Math.sqrt(Math.max(1, n))) + 3));
  const iso = (c, r) => ({ x: (c - r) * TW / 2, y: (c + r) * TH / 2 });

  const cells = [];
  for (let r = 0; r < G; r++) for (let c = 0; c < G; c++) cells.push({ c, r });
  cells.sort((a, b) => (a.c + a.r) - (b.c + b.r) || a.c - b.c);

  // пруд — блок 2×2 плиток
  const water = new Set();
  [[1, 1], [2, 1], [1, 2], [2, 2]].forEach(([c, r]) => { if (c < G && r < G) water.add(c * 100 + r); });
  if (water.size) { const pcw = iso(1.5, 1.5); bloom += `<ellipse cx="${pcw.x.toFixed(1)}" cy="${pcw.y.toFixed(1)}" rx="16" ry="8" fill="#bfe8fb"/>`; }

  // растения-позиции раскидываем по плиткам псевдослучайно (мимо пруда)
  const shuffled = cells.filter((c) => !water.has(c.c * 100 + c.r))
    .map((c) => ({ c, k: hashStr(c.c + "_" + c.r + "s") })).sort((a, b) => a.k - b.k).map((o) => o.c);
  const posMap = new Map();
  shuffled.slice(0, n).forEach((cell, i) => posMap.set(cell.c * 100 + cell.r, i));

  let minY = 1e9, maxX = -1e9, minX = 1e9, maxY = -1e9;
  cells.forEach((cell) => {
    const { x, y } = iso(cell.c, cell.r);
    minX = Math.min(minX, x); maxX = Math.max(maxX, x);
    minY = Math.min(minY, y); maxY = Math.max(maxY, y);
  });

  // плитки — задними рядами вперёд (шахматная раскраска)
  let tiles = "";
  cells.forEach((cell) => {
    const { x, y } = iso(cell.c, cell.r);
    tiles += water.has(cell.c * 100 + cell.r)
      ? drawWaterTile(x, y, TW, TH, EH)
      : drawTile(x, y, TW, TH, EH, (cell.c + cell.r) % 2);
  });

  // пропсы: растения-позиции (кликабельны) + фоновая зелень, сорт по глубине
  const props = [];
  cells.forEach((cell) => {
    const { x, y } = iso(cell.c, cell.r);
    const depth = cell.c + cell.r;
    const key = cell.c * 100 + cell.r;
    if (water.has(key)) return;
    if (posMap.has(key)) {
      const i = posMap.get(key);
      const p = pos[i], m = posMetrics(p);
      if (m.tp > 0.4) bloom += `<circle cx="${x.toFixed(1)}" cy="${(y - 12).toFixed(1)}" r="3" fill="${m.long ? "#ff8a97" : "#ffd77a"}"/>`;
      const droop = m.sl > 0.45 ? (m.sl - 0.45) * 14 * (m.long ? 1 : -1) : 0;
      const wilt = m.sl > 0.6 || m.roe < -18;
      const sway = (3.4 + (hashStr(p.symbol) % 10) * 0.12).toFixed(2);
      const delay = ((hashStr(p.symbol) % 20) * 0.15).toFixed(2);
      const sel = p.symbol === gardenSelSym;
      let g = `<g class="garden-plant${sel ? " sel" : ""}" data-i="${i}" transform="translate(${x.toFixed(1)},${y.toFixed(1)})">`;
      g += shadow(6);
      g += `<rect x="-5" y="-2" width="10" height="3" fill="#6b4f34"/><rect x="-6" y="-1" width="12" height="2" fill="#6b4f34"/>`; // лунка
      if (sel) g += `<rect x="-9" y="-24" width="18" height="26" fill="none" stroke="#ffe08a" stroke-width="1"/>`;
      g += `<rect x="-9" y="-24" width="18" height="26" fill="transparent"/>`;
      g += `<g class="${wilt ? "" : "garden-sway"}" style="animation-duration:${sway}s;animation-delay:${delay}s" transform="rotate(${droop.toFixed(1)})" opacity="${wilt ? 0.7 : 1}">${plantBody(p, m)}</g>`;
      g += `<rect x="8" y="-13" width="${PX}" height="13" fill="#5b4a34"/><rect x="9" y="-13" width="4" height="3" fill="${m.long ? "#37b26a" : "#e0566a"}"/></g>`;
      props.push({ depth, y, svg: g });
    } else {
      const h = hashStr("a" + cell.c + "_" + cell.r) % 10;
      let rows = null;
      if (h < 3) rows = null;              // пустая трава
      else if (h < 5) rows = SPRITES[4];   // ёлочка
      else if (h < 6) rows = SPRITES[0];   // дуб
      else if (h < 8) rows = AMB.flower;
      else if (h < 9) rows = AMB.coral;
      else rows = AMB.tuft;
      if (rows) {
        if (rows === AMB.flower) bloom += `<circle cx="${x.toFixed(1)}" cy="${(y - 4).toFixed(1)}" r="3" fill="#ffb9d0"/>`;
        else if (rows === AMB.coral) bloom += `<circle cx="${x.toFixed(1)}" cy="${(y - 5).toFixed(1)}" r="3.5" fill="#ff7a6a"/>`;
        const svg = `<g transform="translate(${x.toFixed(1)},${y.toFixed(1)})">${shadow(rows[0].length * 0.42)}${sprite(rows, AMB_PAL, -Math.floor(rows[0].length / 2), -rows.length)}</g>`;
        props.push({ depth, y, svg });
      }
    }
  });
  props.sort((a, b) => a.depth - b.depth || a.y - b.y);
  const propSvg = props.map((p) => p.svg).join("");

  // границы вьюпорта
  const pad = TW / 2 + 2;
  const vbMinX = minX - pad, vbW = (maxX - minX) + 2 * pad;
  const vbMinY = minY - 26, vbMaxY = maxY + TH / 2 + EH + 3;
  const vbH = vbMaxY - vbMinY;

  const up = bal.unrealisedPnl || 0;
  const sunny = up > 0.5, storm = up < -20;
  const sunX = maxX - 6, sunY = vbMinY + 6;
  let weather = "";
  if (sunny) {
    weather = sprite([" YY ", "YYYY", "YYYY", " YY "], { Y: "#ffd24a" }, sunX, sunY);
    bloom += `<circle cx="${sunX + 2}" cy="${sunY + 2}" r="10" fill="#fff2b0"/><circle cx="${sunX + 2}" cy="${sunY + 2}" r="26" fill="#ffe79a" opacity="0.5"/>`;
  } else {
    weather = sprite(["  wwww  ", " wwwwww ", "wwwwwwww"], { w: "#dfe6ec" }, sunX - 4, sunY);
    if (storm) for (let d = 0; d < 4; d++) weather += `<rect class="garden-rain" x="${sunX + d * 2}" y="${sunY + 4}" width="${PX}" height="3" fill="#8fb8d8" style="animation-delay:${(d * 0.2).toFixed(1)}s"/>`;
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
    `<div class="garden-plot"><svg viewBox="${vbMinX.toFixed(0)} ${vbMinY.toFixed(0)} ${vbW.toFixed(0)} ${vbH.toFixed(0)}" class="garden-svg" shape-rendering="crispEdges" preserveAspectRatio="xMidYMid meet">` +
    `<defs><linearGradient id="gsky" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${skyTop}"/><stop offset="1" stop-color="${skyBot}"/></linearGradient>` +
    `<filter id="gbloom" x="-40%" y="-40%" width="180%" height="180%"><feGaussianBlur stdDeviation="2.6"/></filter></defs>` +
    `<rect x="${vbMinX}" y="${vbMinY}" width="${vbW}" height="${vbH}" fill="url(#gsky)"/>${weather}${tiles}${propSvg}` +
    `<g class="garden-bloom" filter="url(#gbloom)">${bloom}</g></svg></div>`;

  el.innerHTML = hud + scene + `<div id="garden-readout" class="garden-readout">${gardenReadoutHTML()}</div>`;
  applyGardenTransform();
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

// ── Сад: зум (щипок/колесо) и панорама (перетаскивание пальцем) ───────────────
let gz = { s: 1, tx: 0, ty: 0 };
let gardenInteracting = false;
function gardenSvgEl() { return document.querySelector("#garden .garden-svg"); }
function applyGardenTransform() {
  const svg = gardenSvgEl();
  if (!svg) return;
  gz.s = clamp(gz.s, 1, 5);
  const plot = svg.parentElement.getBoundingClientRect();
  const maxX = (gz.s - 1) * plot.width / 2, maxY = (gz.s - 1) * plot.height / 2;
  gz.tx = clamp(gz.tx, -maxX, maxX);
  gz.ty = clamp(gz.ty, -maxY, maxY);
  svg.style.transform = `translate(${gz.tx.toFixed(1)}px,${gz.ty.toFixed(1)}px) scale(${gz.s.toFixed(3)})`;
}
function gardenZoomAt(f, cx, cy) {
  const svg = gardenSvgEl();
  if (!svg) return;
  const r = svg.parentElement.getBoundingClientRect();
  const fx = cx - r.left - r.width / 2, fy = cy - r.top - r.height / 2;
  const ns = clamp(gz.s * f, 1, 5), ff = ns / gz.s;
  gz.tx = ff * gz.tx + (1 - ff) * fx;
  gz.ty = ff * gz.ty + (1 - ff) * fy;
  gz.s = ns;
  applyGardenTransform();
}
(function gardenGestures() {
  const host = $("garden");
  if (!host) return;
  let drag = false, lastX = 0, lastY = 0, lastDist = 0, lastTap = 0;
  const inPlot = (e) => e.target.closest(".garden-plot");
  const dist = (t) => Math.hypot(t[0].clientX - t[1].clientX, t[0].clientY - t[1].clientY);
  host.addEventListener("touchstart", (e) => {
    if (!inPlot(e)) return;
    gardenInteracting = true;
    if (e.touches.length === 1) { drag = true; lastX = e.touches[0].clientX; lastY = e.touches[0].clientY; }
    else if (e.touches.length === 2) { drag = false; lastDist = dist(e.touches); }
  }, { passive: true });
  host.addEventListener("touchmove", (e) => {
    if (!inPlot(e)) return;
    if (e.touches.length === 2) {
      const d = dist(e.touches);
      if (lastDist > 0) gardenZoomAt(d / lastDist, (e.touches[0].clientX + e.touches[1].clientX) / 2, (e.touches[0].clientY + e.touches[1].clientY) / 2);
      lastDist = d; e.preventDefault();
    } else if (drag && e.touches.length === 1) {
      const dx = e.touches[0].clientX - lastX, dy = e.touches[0].clientY - lastY;
      lastX = e.touches[0].clientX; lastY = e.touches[0].clientY;
      if (gz.s > 1.02 || Math.abs(dx) + Math.abs(dy) > 4) { gz.tx += dx; gz.ty += dy; applyGardenTransform(); e.preventDefault(); }
    }
  }, { passive: false });
  host.addEventListener("touchend", (e) => {
    if (e.touches.length > 0) return;
    drag = false; lastDist = 0; gardenInteracting = false;
    const t = Date.now();
    if (t - lastTap < 300) { gz = { s: 1, tx: 0, ty: 0 }; applyGardenTransform(); } // двойной тап — сброс
    lastTap = t;
  });
  host.addEventListener("wheel", (e) => {
    if (!inPlot(e)) return;
    e.preventDefault();
    gardenZoomAt(e.deltaY < 0 ? 1.12 : 1 / 1.12, e.clientX, e.clientY);
  }, { passive: false });
  // мышью (десктоп) — перетаскивание панорамы
  let mDrag = false, mX = 0, mY = 0;
  host.addEventListener("pointerdown", (e) => {
    if (e.pointerType === "touch" || !inPlot(e)) return;
    mDrag = true; mX = e.clientX; mY = e.clientY;
  });
  window.addEventListener("pointermove", (e) => {
    if (!mDrag) return;
    gz.tx += e.clientX - mX; gz.ty += e.clientY - mY;
    mX = e.clientX; mY = e.clientY; applyGardenTransform();
  });
  window.addEventListener("pointerup", () => { mDrag = false; });
})();

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
