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
const en = () => theme === "garden"; // тема «Сад» — англ. интерфейс (пиксельный шрифт ровнее)

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
  // в теме «Сад» подписи вкладок на английском (пиксельный шрифт ровнее на латинице)
  const TAB_LABELS = { overview: ["Обзор", "Garden"], trades: ["Сделки", "Trades"], orders: ["Ордеры", "Orders"] };
  document.querySelectorAll("#tabs .tab").forEach((b) => {
    const l = TAB_LABELS[b.dataset.tab];
    if (l) b.textContent = theme === "garden" ? l[1] : l[0];
  });
  const CHIP_LABELS = { all: ["Все", "All"], win: ["Прибыльные", "Profit"], loss: ["Убыточные", "Loss"], open: ["Открытые", "Open"] };
  document.querySelectorAll("#trade-filters .chip").forEach((b) => {
    const l = CHIP_LABELS[b.dataset.f];
    if (l) b.textContent = theme === "garden" ? l[1] : l[0];
  });
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
  const u = en() ? ["min", "h", "d"] : ["мин", "ч", "дн"];
  if (mins < 60) return mins + " " + u[0];
  if (mins < 48 * 60) return (mins / 60).toFixed(1).replace(".0", "") + " " + u[1];
  return Math.round(mins / 1440) + " " + u[2];
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
  noteGardenEvents(ov); // события сбора/гибели при закрытии позиций
  if (theme === "garden") {
    renderGarden(ov);
    $("updated-text").textContent = "Updated " + new Date(ov.updatedAt).toLocaleTimeString("ru-RU");
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
const NS = "http://www.w3.org/2000/svg";

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

// ── Сад: рисованный пиксель-арт — деревянные грядки-ящики с культурами ───────
const CPAL = { g: "#43702f", G: "#5f9a3f", l: "#8fca5a", C: "#c6e896", r: "#d0433a", R: "#e07a30", O: "#d98a34", o: "#a9631c", P: "#5f8a3a", t: "#5a3f25",
  b: "#9a7a3a", B: "#6f5327", y: "#b9a45a" }; // бурые — для увядшего
// увядшая культура (near-SL): поникшие бурые листья, любой тип
const CROP_WILT = ["y     y", " b   b ", "  BbB  ", "  bBb  ", "   b   "];
// культуры по стадиям: 0 пусто · 1 росток · 2 рост · 3 спелое
// 5 стадий: 0 пусто · 1 росток · 2 рост · 3 завязь · 4 спелое
const CROPS = {
  tomato: [[],
    ["  l  ", " gGg ", "  t  "],
    ["  ggg  ", " gGGGg ", " gGGg  "],
    ["  ggGgg  ", " gGGGGGg ", "gGGGlGGGGg", " gGGGGGGg ", "  ggGGgg  ", "   ttt   "],
    ["  g g g g g  ", " gGGGGGGGGGg ", "gGrGGGlGGGrGg", "gGGGrGGGrGGGg", "gGrGGGGGGGrGg", " gGGGrGrGGGg ", "  gGGGGGGGg  ", "    ttttt    "]],
  cabbage: [[],
    ["  l  ", " gGg "],
    ["  ggg  ", " gGGGg "],
    ["   gGGGg   ", "  gGGGGGg  ", " gGGGGGGGg ", " ggGGGGGgg "],
    ["   gGGGg   ", "  gGCCCGg  ", " gGCCCCCGg ", "gGCCGGCCCGg", "gGGCCCCCGGg", " gGGGGGGGg ", "  gg   gg  "]],
  pumpkin: [[],
    ["  l  ", " gGg "],
    ["  ggg  ", " gGPGg "],
    ["  ggggg  ", " gPPPPPg ", " gPPPPPg ", "  ggggg  "],
    ["   OOOOO   ", "  OOoOOoO  ", " OOOoOOoOO ", "OOOoOOOoOOO", "OOOoOOOoOOO", " OOOoOOoOO ", "  OOOOOOO  ", "  g     g  "]],
  carrot: [[],
    [" l l ", " gGg "],
    ["l l l", " gGGg "],
    ["l l l l", " gGGGg ", " gGg   "],
    [" l l l l l ", "  lGlGlG   ", "  gGGGGg   ", "   RRRR    ", "   RRR     ", "    R      "]],
  beet: [[],
    [" l l ", " gGg "],
    ["l l l", " gGGg "],
    ["l l l l", " gGGGg "],
    [" l l l l ", "  lGGl   ", "  gGg    ", "  rrr    ", "  rr     "]],
};
const CROP_KEYS = ["tomato", "cabbage", "pumpkin", "carrot", "beet"];

// пиксельная культура из строк, якорь низ-центр в (cx,cy)
function cropSvg(cx, cy, rows) {
  const H = rows.length, W = Math.max(...rows.map((r) => r.length));
  let s = "";
  for (let y = 0; y < H; y++) for (let x = 0; x < rows[y].length; x++) {
    const ch = rows[y][x]; if (ch === " ") continue; const c = CPAL[ch]; if (!c) continue;
    s += `<rect x="${cx - (W >> 1) + x}" y="${cy - H + y}" width="1.02" height="1.02" fill="${c}"/>`;
  }
  return s;
}
// деревянная приподнятая грядка-ящик, центр земли в (X,Y)
function bedSvg(X, Y) {
  const woodT = "#7a5636", woodL = "#6a4a2c", woodR = "#5a3f25", woodD = "#4a3320", plank = "#3a2718", soil = "#3b2b1f", soilL = "#4a3526", speck = "#2b1f15";
  const HW = 17, HH = 9, SH = 8, iHW = 13, iHH = 7;
  let s = "";
  for (let dy = -HH; dy <= HH; dy++) { const hw = Math.round(HW * (1 - Math.abs(dy) / HH)); s += `<rect x="${X - hw}" y="${Y + dy}" width="${2 * hw + 1.02}" height="1.02" fill="${dy < -2 ? woodT : woodL}"/>`; }
  for (let dy = -iHH; dy <= iHH; dy++) { const hw = Math.round(iHW * (1 - Math.abs(dy) / iHH)); s += `<rect x="${X - hw}" y="${Y + dy}" width="${2 * hw + 1.02}" height="1.02" fill="${(dy & 1) ? soil : soilL}"/>`; }
  for (let i = 0; i < 6; i++) s += `<rect x="${X - 8 + ((i * 97) % 16)}" y="${Y - 4 + ((i * 53) % 8)}" width="1.02" height="1.02" fill="${speck}"/>`;
  for (let x = -HW; x <= HW; x++) { const by = Math.round(Y + HH * (1 - Math.abs(x) / HW)); const left = x < 0; s += `<rect x="${X + x}" y="${by}" width="1.02" height="${SH}" fill="${left ? woodD : woodR}"/>`; if ((x + 60) % 6 === 0) s += `<rect x="${X + x}" y="${by}" width="1.02" height="${SH}" fill="${plank}"/>`; s += `<rect x="${X + x}" y="${by + SH - 1}" width="1.02" height="1.02" fill="${plank}"/>`; }
  return s;
}
function gardenReadoutHTML() {
  const pos = lastOv ? lastOv.positions || [] : [];
  const p = pos.find((x) => x.symbol === gardenSelSym);
  if (!p) return `<span class="gr-hint">🌿 Tap — plot · pinch — zoom · double-tap — reset</span>`;
  const m = posMetrics(p);
  const stage = m.tp > 0.72 ? "almost ripe" : m.tp > 0.5 ? "ripening" : m.tp > 0.22 ? "growing" : "sprout";
  return `<div class="gr-row"><b>${p.symbol}</b>${dirBadge(p.side)}<span class="badge">${fmt(p.leverage, 0)}x</span>` +
    `<span class="gr-pnl">${pnlSpan(p.unrealisedPnl)} · ROE ${pnlSpan(m.roe, "%", 1)}</span></div>` +
    `<div class="gr-sub">entry $${fmtPrice(p.entryPrice)} · mark $${fmtPrice(p.markPrice)} · to TP ${(m.tp * 100).toFixed(0)}% — ${stage}</div>`;
}

// событие закрытия позиции: сравниваем прошлый набор символов с текущим.
// исчез символ → сделка закрыта: в плюсе — «урожай», в минусе — «гибель».
let gardenPrev = null;
function noteGardenEvents(ov) {
  const cur = {};
  (ov.positions || []).forEach((p) => { cur[p.symbol] = posMetrics(p).roe; });
  if (gardenPrev && theme === "garden") {
    for (const sym in cur) if (!(sym in gardenPrev)) gardenToast("plant", sym);       // новая позиция — «посадка»
    for (const sym in gardenPrev) if (!(sym in cur)) {                                 // позиция закрыта
      const ok = gardenPrev[sym] >= 0;
      gardenToast(ok ? "harvest" : "death", sym);
      if (ok) flyCoins();
    }
  }
  gardenPrev = cur;
}
const TOAST_TXT = { plant: "🌱 Planted — ", harvest: "🌾 Harvested — ", death: "🥀 Withered — " };
function gardenToast(kind, text) {
  const el = document.createElement("div");
  el.className = "garden-toast " + kind;
  el.textContent = (TOAST_TXT[kind] || "") + text;
  document.body.appendChild(el);
  haptic(kind === "death" ? "medium" : "light");
  setTimeout(() => el.remove(), 3600);
}
// монетки летят в счётчик «Урожай»
function flyCoins() {
  const target = document.querySelector(".garden-hud .gh:last-child .gh-v");
  const plot = document.querySelector(".garden-plot");
  if (!target || !plot) return;
  const tr = target.getBoundingClientRect(), pr = plot.getBoundingClientRect();
  const ox = pr.left + pr.width / 2, oy = pr.top + pr.height / 2;
  for (let i = 0; i < 6; i++) {
    const c = document.createElement("div");
    c.className = "garden-coin"; c.textContent = "🪙";
    c.style.left = ox + "px"; c.style.top = oy + "px";
    document.body.appendChild(c);
    requestAnimationFrame(() => {
      c.style.transition = `transform .9s cubic-bezier(.4,0,.2,1) ${(i * 0.05).toFixed(2)}s, opacity .9s ${(i * 0.05).toFixed(2)}s`;
      c.style.transform = `translate(${(tr.left + tr.width / 2 - ox + (Math.random() * 22 - 11)).toFixed(0)}px, ${(tr.top - oy).toFixed(0)}px) scale(.55)`;
      c.style.opacity = "0";
    });
    setTimeout(() => c.remove(), 1300 + i * 60);
  }
  target.classList.add("gh-pulse");
  setTimeout(() => target.classList.remove("gh-pulse"), 700);
}
// «пока вас не было»: сводка с прошлого визита (localStorage), показываем один раз
let gardenSummaryDone = false;
function gardenPersistAndSummary() {
  if (theme !== "garden" || !lastStatsData || !lastOv) return;
  const cur = { rp: lastStatsData.realizedPnl || 0, syms: (lastOv.positions || []).map((p) => p.symbol), t: Date.now() };
  if (!gardenSummaryDone) {
    gardenSummaryDone = true;
    let prev = null; try { prev = JSON.parse(localStorage.getItem("gardenSeen")); } catch (e) { /* нет истории */ }
    if (prev && cur.t - prev.t > 10 * 60 * 1000) {
      const dRp = cur.rp - prev.rp;
      const opened = cur.syms.filter((s) => !prev.syms.includes(s)).length;
      const closed = prev.syms.filter((s) => !cur.syms.includes(s)).length;
      const parts = [];
      if (Math.abs(dRp) >= 0.01) parts.push(`harvest ${dRp >= 0 ? "+" : "−"}${fmt(Math.abs(dRp))}$`);
      if (opened) parts.push(`planted ${opened}`);
      if (closed) parts.push(`cleared ${closed}`);
      if (parts.length) {
        const el = document.createElement("div");
        el.className = "garden-toast away";
        el.textContent = "🌙 While away: " + parts.join(" · ");
        document.body.appendChild(el);
        setTimeout(() => el.remove(), 5200);
      }
    }
  }
  localStorage.setItem("gardenSeen", JSON.stringify(cur));
}

// сигнатура значимого состояния сада — пересобираем сцену только при её смене
// (иначе каждый тик/тап рвёт CSS-анимации светлячков, звёзд, парения)
let lastGardenSig = "";
function gardenSig(ov) {
  const parts = (ov.positions || []).map((p) => {
    const m = posMetrics(p);
    const st = (m.sl > 0.6 || m.roe < -18) ? "w" : (m.tp > 0.72 ? 4 : m.tp > 0.5 ? 3 : m.tp > 0.22 ? 2 : 1);
    return p.symbol + st;
  }).sort().join(",");
  const up = (ov.balance || {}).unrealisedPnl || 0;
  const w = up > 0.5 ? "s" : up < -20 ? "t" : "c";
  const rp = lastStatsData ? Math.round(lastStatsData.realizedPnl || 0) : 0;
  return parts + "|" + new Date().getHours() + "|" + w + "|" + rp;
}

// пиксельный залитый диск (для луны)
function pixelDisc(cx, cy, r, color) {
  let s = "";
  for (let dy = -r; dy <= r; dy++) {
    const w = Math.round(Math.sqrt(r * r - dy * dy));
    if (w <= 0) continue;
    s += `<rect x="${cx - w}" y="${cy + dy}" width="${2 * w + 1}" height="1" fill="${color}"/>`;
  }
  return s;
}

// мягкое свечение выбранной грядки — пиксельные ромбы в разметке (SVG-нативно,
// работает и на мобилке в отличие от CSS drop-shadow). Статичное, низкая непрозрачность.
const SEL_GLOW = [[26, 14, 0.08, "#f0c25a"], [21, 11, 0.12, "#ffd77a"]];
// силуэт всей приподнятой грядки (верхний ромб + деревянные боковины вниз на SH),
// а не только верхняя грань — обводка «обнимает» весь короб
const selOutline = (cx, cy) =>
  `${cx},${cy - 9} ${cx + 17},${cy} ${cx + 17},${cy + 8} ${cx},${cy + 17} ${cx - 17},${cy + 8} ${cx - 17},${cy}`;
const GLOW_DY = 4; // центр свечения ниже верхней грани — чтобы ореол охватывал весь короб
function selGlowStr(cx, cy) {
  const gy = cy + GLOW_DY;
  return `<g class="garden-selglow">` + SEL_GLOW.map(([hw, hh, op, col]) =>
    `<polygon points="${cx},${(gy - hh)} ${(cx + hw)},${gy} ${cx},${(gy + hh)} ${(cx - hw)},${gy}" fill="${col}" opacity="${op}"/>`).join("") + `</g>`;
}
function selGlowDom(cx, cy) {
  const gy = cy + GLOW_DY;
  const g = document.createElementNS(NS, "g");
  g.setAttribute("class", "garden-selglow");
  SEL_GLOW.forEach(([hw, hh, op, col]) => {
    const poly = document.createElementNS(NS, "polygon");
    poly.setAttribute("points", `${cx},${gy - hh} ${cx + hw},${gy} ${cx},${gy + hh} ${cx - hw},${gy}`);
    poly.setAttribute("fill", col);
    poly.setAttribute("opacity", op);
    g.appendChild(poly);
  });
  return g;
}

function renderGarden(ov) {
  const el = $("garden");
  if (!el) return;
  if (gardenInteracting) return; // не пересобирать SVG посреди жеста
  lastGardenRender = Date.now();
  // ничего значимого не изменилось — сцену не трогаем (анимации живут), только подпись
  const sig = gardenSig(ov);
  if (sig === lastGardenSig && el.querySelector(".garden-svg")) {
    const ro = $("garden-readout");
    if (ro && gardenSelSym) ro.innerHTML = gardenReadoutHTML();
    return;
  }
  lastGardenSig = sig;
  let bloom = ""; // мягкое свечение «спелого» урожая
  const pos = (ov.positions || []).slice(0, 16);
  const bal = ov.balance || {};
  const TW = 39, TH = 21;
  const n = pos.length;
  const G = Math.max(4, Math.min(6, Math.ceil(Math.sqrt(Math.max(1, n))) + 2));
  const iso = (c, r) => ({ x: (c - r) * TW / 2, y: (c + r) * TH / 2 });

  const cells = [];
  for (let r = 0; r < G; r++) for (let c = 0; c < G; c++) cells.push({ c, r });
  cells.sort((a, b) => (a.c + a.r) - (b.c + b.r) || a.c - b.c);

  // позиции раскидываем по грядкам псевдослучайно
  const shuffled = cells.map((c) => ({ c, k: hashStr(c.c + "_" + c.r + "s") })).sort((a, b) => a.k - b.k).map((o) => o.c);
  const posMap = new Map();
  shuffled.slice(0, n).forEach((cell, i) => posMap.set(cell.c * 100 + cell.r, i));

  let minX = 1e9, maxX = -1e9, minY = 1e9, maxY = -1e9;
  cells.forEach((cell) => {
    const { x, y } = iso(cell.c, cell.r);
    minX = Math.min(minX, x); maxX = Math.max(maxX, x);
    minY = Math.min(minY, y); maxY = Math.max(maxY, y);
  });

  // каждая клетка — грядка; передние ряды рисуются поверх задних
  const items = [];
  cells.forEach((cell) => {
    const { x, y } = iso(cell.c, cell.r);
    const depth = cell.c + cell.r, key = cell.c * 100 + cell.r;
    const selHere = posMap.has(key) && pos[posMap.get(key)].symbol === gardenSelSym;
    let s = (selHere ? selGlowStr(x, y) : "") + bedSvg(x, y);
    if (posMap.has(key)) {
      const i = posMap.get(key);
      const p = pos[i], m = posMetrics(p);
      const ck = CROP_KEYS[hashStr(p.symbol) % CROP_KEYS.length];
      const st = m.tp > 0.72 ? 4 : m.tp > 0.5 ? 3 : m.tp > 0.22 ? 2 : 1; // стадия роста ← прогресс к TP
      const wilt = m.sl > 0.6 || m.roe < -18;           // near-SL / крупный минус → увядшее
      const rows = wilt ? CROP_WILT : CROPS[ck][st];
      const sat = m.roe >= 0 ? 1 : clamp(1 + m.roe / 26, 0.45, 1);
      const filt = (!wilt && m.roe < 0) ? ` style="filter:saturate(${sat.toFixed(2)})"` : ""; // лёгкий минус — выцветает
      if (rows && rows.length) s += `<g${filt}>${cropSvg(x, y + 3, rows)}</g>`;
      if (!wilt && st === 4) bloom += `<ellipse cx="${x}" cy="${(y - 3).toFixed(1)}" rx="9" ry="6" fill="${m.long ? "#ffcf9a" : "#ffe6a0"}"/>`;
      if (wilt) { // падающие листья над чахнущей грядкой
        const ld = ((hashStr(p.symbol) % 10) * 0.2).toFixed(2);
        s += `<rect class="garden-leaf" x="${(x - 5).toFixed(1)}" y="${(y - 8).toFixed(1)}" width="2" height="2" fill="#a9863c" style="animation-delay:${ld}s"/>`;
        s += `<rect class="garden-leaf" x="${(x + 4).toFixed(1)}" y="${(y - 6).toFixed(1)}" width="2" height="2" fill="#8a672c" style="animation-delay:${(+ld + 1.1).toFixed(2)}s"/>`;
      }
      const sel = p.symbol === gardenSelSym;
      // подсветка выбранной — мягкая пульсирующая обводка ромба грядки
      if (sel) s += `<polygon class="garden-sel" points="${selOutline(x, y)}" fill="none" stroke="#dca94e" stroke-width="1"/>`;
      const bob = ((cell.c + cell.r) * 0.22 + (hashStr(p.symbol) % 10) * 0.13).toFixed(2);
      let g = `<g class="garden-plant garden-bob${sel ? " sel" : ""}" data-i="${i}" data-cx="${x}" data-cy="${y}" style="animation-delay:${bob}s">`;
      // хит-зона = ромб изо-плитки (TW/2×TH/2): плитки стыкуются без нахлёста,
      // тап всегда попадает ровно в одну грядку (прямоугольник свисал на ряды сзади)
      const hw = TW / 2, hh = TH / 2;
      g += `<polygon points="${x},${(y - hh).toFixed(1)} ${(x + hw).toFixed(1)},${y} ${x},${(y + hh).toFixed(1)} ${(x - hw).toFixed(1)},${y}" fill="transparent"/>`;
      g += s + `</g>`;
      items.push({ depth, y, svg: g });
    } else {
      // не-позиционная грядка — пустая (только земля)
      const bob = ((cell.c + cell.r) * 0.22 + (hashStr("a" + cell.c + "_" + cell.r) % 10) * 0.13).toFixed(2);
      items.push({ depth, y, svg: `<g class="garden-bob" style="animation-delay:${bob}s">${s}</g>` });
    }
  });
  items.sort((a, b) => a.depth - b.depth || a.y - b.y);
  const body = items.map((it) => it.svg).join("");

  // день/ночь по локальному часу (день 6–20, ночь 20–6)
  const hh = new Date().getHours();
  const night = hh < 6 || hh >= 20;
  const bgCol = hh < 5 ? "#201b28" : hh < 8 ? "#463943" : hh < 18 ? "#4a3d42" : hh < 20 ? "#4a3540" : hh < 22 ? "#332a35" : "#201b28";

  const pad = TW * 0.55;
  const vbMinX = minX - pad, vbW = (maxX - minX) + 2 * pad;
  const vbMinY = minY - 40, vbMaxY = maxY + 20; // место под небо (солнце/луна дугой)
  const vbH = vbMaxY - vbMinY;

  const up = bal.unrealisedPnl || 0;
  const weatherLabel = up > 0.5 ? "☀ Sunny" : up < -20 ? "🌧 Storm" : "☁ Cloudy";
  const harvest = lastStatsData ? lastStatsData.realizedPnl : null;
  const hud =
    `<div class="garden-hud">` +
    `<div class="gh"><span class="gh-l">Beds</span><span class="gh-v">${n}</span></div>` +
    `<div class="gh"><span class="gh-l">Weather</span><span class="gh-v">${weatherLabel}</span></div>` +
    `<div class="gh"><span class="gh-l">Harvest</span><span class="gh-v ${harvest != null && harvest < 0 ? "pnl-neg" : "pnl-pos"}">${harvest != null ? (harvest >= 0 ? "+" : "") + fmt(harvest) + "$" : "—"}</span></div>` +
    `</div>`;

  // небесное тело: солнце днём / луна ночью — простой круг с мягким пиксельным
  // свечением, позиция по времени суток (дуга по небу слева направо)
  const tod = hh + new Date().getMinutes() / 60;
  const cf = night ? clamp((hh >= 20 ? tod - 20 : tod + 4) / 10, 0, 1) : clamp((tod - 6) / 14, 0, 1);
  const bx = Math.round(vbMinX + vbW * (0.12 + 0.76 * cf));
  const by = Math.round(vbMinY + 5 + (1 - Math.sin(cf * Math.PI)) * 22);
  const bodyCol = night ? "#e9e3cf" : "#ffd24a", glowCol = night ? "#cdd6ec" : "#ffe08a";
  let critters =
    `<g opacity="0.05">${pixelDisc(bx, by, 6, glowCol)}</g>` + // еле заметный ореол
    pixelDisc(bx, by, 5, bodyCol) +
    (night // кратеры на луне
      ? `<rect x="${bx - 2}" y="${by - 2}" width="2" height="2" fill="#cec8b3"/>` +
        `<rect x="${bx + 1}" y="${by}" width="2" height="1" fill="#cec8b3"/>` +
        `<rect x="${bx - 1}" y="${by + 2}" width="1" height="1" fill="#cec8b3"/>`
      : "");
  if (night) {
    const stars = [[0.1, 0.05], [0.24, 0.12], [0.38, 0.04], [0.52, 0.1], [0.66, 0.03], [0.16, 0.16], [0.46, 0.15], [0.9, 0.12]];
    stars.forEach((s, i) => {
      const sz = i % 3 ? 1 : 2;
      critters += `<rect class="garden-star" x="${Math.round(vbMinX + vbW * s[0])}" y="${Math.round(vbMinY + vbH * s[1] * 0.5)}" width="${sz}" height="${sz}" fill="#e8e6d8" style="animation-delay:${(i * 0.4).toFixed(1)}s"/>`;
    });
    const fpos = [[0.3, 0.6], [0.56, 0.5], [0.72, 0.64], [0.44, 0.7]];
    fpos.forEach((f, i) => {
      const fx = Math.round(vbMinX + vbW * f[0]), fy = Math.round(vbMinY + vbH * f[1]);
      critters += `<g class="garden-firefly" style="animation-delay:${(i * 0.7).toFixed(1)}s, ${(i * 1.6).toFixed(1)}s"><rect x="${fx - 1}" y="${fy - 1}" width="4" height="4" fill="#ffe89a" opacity="0.28"/><rect x="${fx}" y="${fy}" width="2" height="2" fill="#fff4c0"/></g>`;
    });
  } else {
    for (let i = 0; i < 2; i++) critters += `<g transform="translate(${(vbMinX + vbW * (0.34 + 0.3 * i)).toFixed(0)},${(vbMinY + 24 + i * 14).toFixed(0)})"><g class="garden-bfly" style="animation-delay:${(i * 2.3).toFixed(1)}s"><rect x="-2.4" y="0" width="2" height="2" fill="#f2d24a"/><rect x="1" y="0" width="2" height="2" fill="#f2d24a"/><rect x="-0.2" y="0.4" width="1" height="1.4" fill="#b58a24"/></g></g>`;
  }

  const scene =
    `<div class="garden-plot"><svg viewBox="${vbMinX.toFixed(0)} ${vbMinY.toFixed(0)} ${vbW.toFixed(0)} ${vbH.toFixed(0)}" class="garden-svg" shape-rendering="crispEdges" preserveAspectRatio="xMidYMid meet">` +
    `<defs><filter id="gbloom" x="-40%" y="-40%" width="180%" height="180%"><feGaussianBlur stdDeviation="2.4"/></filter></defs>` +
    `<rect x="${vbMinX}" y="${vbMinY}" width="${vbW}" height="${vbH}" fill="${bgCol}"/>${body}${critters}` +
    `<g class="garden-bloom" filter="url(#gbloom)">${bloom}</g></svg></div>`;

  // амбар / альманах — накопительная витрина реальной статистики
  const alm = lastStatsData
    ? `<div class="garden-almanac">` +
      `<div class="alm"><span>🧺 Harvested</span><b class="${lastStatsData.realizedPnl < 0 ? "pnl-neg" : "pnl-pos"}">${(lastStatsData.realizedPnl >= 0 ? "+" : "") + fmt(lastStatsData.realizedPnl)}$</b></div>` +
      `<div class="alm"><span>🌾 Crops</span><b>${lastStatsData.closed}</b></div>` +
      `<div class="alm"><span>⭐ Record</span><b>${pnlSpan(lastStatsData.bestPct, "%", 1)}</b></div>` +
      `<div class="alm"><span>🎯 Winrate</span><b>${lastStatsData.winRate != null ? fmt(lastStatsData.winRate, 0) + "%" : "—"}</b></div>` +
      `</div>`
    : "";

  el.innerHTML = hud + scene + `<div id="garden-readout" class="garden-readout">${gardenReadoutHTML()}</div>` + alm;
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
    statCell(st.winRate != null ? fmt(st.winRate, 0) + "%" : "—", en() ? "Winrate" : "Винрейт") +
    statCell(String(st.closed), en() ? "Closed" : "Закрыто") +
    statCell(pnlSpan(st.bestPct, "%", 1), en() ? "Best" : "Лучшая") +
    statCell(pnlSpan(st.worstPct, "%", 1), en() ? "Worst" : "Худшая");
}

async function loadOverview() {
  try {
    renderOverview(await api("/api/overview"));
  } catch (e) {
    $("balance-card").innerHTML = `<div class="error">${en() ? "Error" : "Ошибка"}: ${e.message}</div>`;
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
    gardenPersistAndSummary(); // «пока вас не было» + сохранение визита
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
            ${t.status !== "closed" ? `<span class="badge accent">${en() ? "open" : "открыта"}</span>` : ""}
          </div>
          <div class="sub">$${fmtPrice(t.entry)} → ${t.closePrice ? "$" + fmtPrice(t.closePrice) : "…"}</div>
        </div>
        <div class="right">
          <div>${t.pnlPct != null ? pnlSpan(t.pnlPct, "%") : "—"}${usd}</div>
          <div class="sub">${fmtTime(t.closedAt || t.openedAt)}${dur && !/^0 /.test(dur) ? " · " + dur : ""}</div>
        </div>
      </div>`;
    }).join("")
    : `<div class="muted">${en() ? "Nothing found" : "Ничего не найдено"}</div>`;
}

async function loadTrades() {
  try {
    allTrades = await api("/api/trades?limit=100");
    renderTrades();
    if (lastStatsData) renderTradeStats(lastStatsData);
  } catch (e) {
    $("trades-list").innerHTML = `<div class="error">${en() ? "Error" : "Ошибка"}: ${e.message}</div>`;
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
      $("orders-list").innerHTML = `<div class="muted">${en() ? "No pending orders" : "Нет ожидающих ордеров"}</div>`;
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
        ? (leftMs <= 0 ? (en() ? "expired, removing soon" : "истёк, скоро снимется")
          : (en() ? "removes in " : "снимется через ") + (leftMs > 48 * 3600000
            ? Math.round(leftMs / 86400000) + (en() ? "d" : " дн")
            : Math.round(leftMs / 3600000) + (en() ? "h" : " ч")))
        : "";
      const notional = o.price * o.qty;
      return `
      <div class="row">
        <div class="main">
          <div><b>${o.symbol}</b>${dirBadge(o.side)}
            <span class="badge">$${fmt(notional, 0)}</span>
          </div>
          <div class="sub">${en() ? "entry" : "вход"} $${fmtPrice(o.price)} · ${en() ? "now" : "сейчас"} $${fmtPrice(o.lastPrice)}</div>
          <div class="sub">SL $${fmtPrice(o.stopLoss)} · TP $${fmtPrice(o.takeProfit)}</div>
        </div>
        <div class="right">
          <div><span class="badge accent">${en() ? "to entry" : "до входа"} ${distStr}</span></div>
          <div class="sub">${fmtTime(o.createdAt)}</div>
          <div class="sub">${leftStr}</div>
        </div>
      </div>`;
    }).join("");
  } catch (e) {
    $("orders-list").innerHTML = `<div class="error">${en() ? "Error" : "Ошибка"}: ${e.message}</div>`;
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
  if (theme === "garden") return; // в саду свайп — это панорама, а не смена вкладки
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
    // выделение обновляем точечно (без пересборки сцены — иначе рвутся анимации)
    gardenEl.querySelectorAll(".garden-sel, .garden-selglow").forEach((x) => x.remove());
    gardenEl.querySelectorAll(".garden-plant.sel").forEach((x) => x.classList.remove("sel"));
    gardenSelSym = gardenSelSym === p.symbol ? null : p.symbol;
    if (gardenSelSym) {
      const cx = +g.dataset.cx, cy = +g.dataset.cy;
      g.insertBefore(selGlowDom(cx, cy), g.firstChild); // свечение под грядкой
      const poly = document.createElementNS(NS, "polygon");
      poly.setAttribute("class", "garden-sel");
      poly.setAttribute("points", selOutline(cx, cy));
      poly.setAttribute("fill", "none");
      poly.setAttribute("stroke", "#dca94e");
      poly.setAttribute("stroke-width", "1");
      g.appendChild(poly);
      g.classList.add("sel");
    }
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
