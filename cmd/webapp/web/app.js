const tg = window.Telegram.WebApp;
tg.ready();
tg.expand();
try { tg.setHeaderColor("secondary_bg_color"); } catch (e) { /* старые клиенты */ }

const $ = (id) => document.getElementById(id);
const haptic = (style = "light") => {
  try { tg.HapticFeedback.impactOccurred(style); } catch (e) { /* не критично */ }
};

async function api(path) {
  const resp = await fetch(path, {
    headers: { Authorization: "tma " + tg.initData },
  });
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({}));
    throw new Error(body.error || resp.status);
  }
  return resp.json();
}

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

function renderBalance(b) {
  const upnlPct = b.walletBalance ? (b.unrealisedPnl / b.walletBalance) * 100 : 0;
  const up = b.unrealisedPnl >= 0;
  $("balance-card").innerHTML = `
    <div class="hero-label">Equity</div>
    <div class="hero-equity">$${fmt(b.equity)}</div>
    <span class="hero-pnl ${up ? "up" : "down"}">
      ${up ? "▲" : "▼"} ${fmt(Math.abs(b.unrealisedPnl))}$ · ${fmt(Math.abs(upnlPct))}%
    </span>
    <div class="hero-grid">
      <div><div class="lbl">Баланс кошелька</div><div class="val">$${fmt(b.walletBalance)}</div></div>
      <div><div class="lbl">Доступно</div><div class="val">$${fmt(b.available)}</div></div>
      <div><div class="lbl">Нереализ. PnL</div><div class="val">${pnlSpan(b.unrealisedPnl)}</div></div>
      <div><div class="lbl">В позициях</div><div class="val">$${fmt(b.walletBalance - b.available)}</div></div>
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

    return `
      <div class="pos ${long ? "long" : "short"}">
        <div class="pos-head">
          <span class="pos-symbol">${p.symbol}${dirBadge(p.side)}
            <span class="badge">${fmt(p.leverage, 0)}x</span>
          </span>
          <span class="pos-pnl">${pnlSpan(p.unrealisedPnl)}
            <span class="roe">ROE ${pnlSpan(roe, "%", 1)}</span>
          </span>
        </div>
        <div class="pos-detail">
          <span>Вход <b>$${fmtPrice(p.entryPrice)}</b></span>
          <span>Марк <b>$${fmtPrice(p.markPrice)}</b></span>
          <span>Объём <b>$${fmt(value, 0)}</b></span>
        </div>
        ${range}
      </div>`;
  }).join("");
}

const statCell = (v, l) => `<div class="stat"><div class="v">${v}</div><div class="l">${l}</div></div>`;

function renderStats(st, el) {
  el.innerHTML =
    statCell(st.winRate != null ? fmt(st.winRate, 0) + "%" : "—", "Винрейт") +
    statCell(`${st.closed}<small style="color:var(--hint)">+${st.open}</small>`, "Сделок") +
    statCell(pnlSpan(st.totalPct, "%", 1), "Σ PnL") +
    statCell(pnlSpan(st.avgPct, "%", 1), "Сред.");
}

async function loadOverview() {
  try {
    const ov = await api("/api/overview");
    renderBalance(ov.balance);
    renderPositions(ov.positions);
    $("updated").textContent = "Обновлено " + new Date(ov.updatedAt).toLocaleTimeString("ru-RU");
  } catch (e) {
    $("balance-card").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

async function loadStats() {
  try {
    const st = await api("/api/stats");
    renderStats(st, $("stat-grid"));
    if (loaded.trades) renderTradeStats(st);
    lastStats = st;
  } catch (e) { /* статистика не критична */ }
}

let lastStats = null;
function renderTradeStats(st) {
  $("trade-stats").innerHTML =
    statCell(st.winRate != null ? fmt(st.winRate, 0) + "%" : "—", "Винрейт") +
    statCell(String(st.closed), "Закрыто") +
    statCell(pnlSpan(st.bestPct, "%", 1), "Лучшая") +
    statCell(pnlSpan(st.worstPct, "%", 1), "Худшая");
}

// ── График equity ────────────────────────────────────────────────────────────

async function loadEquity() {
  try {
    const pts = await api("/api/equity");
    const svg = $("equity-chart");
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

    const color = last >= 0 ? "var(--green)" : "var(--red)";
    const line = vals.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
    const area = `${padX},${y(0).toFixed(1)} ${line} ${x(vals.length - 1).toFixed(1)},${y(0).toFixed(1)}`;

    svg.innerHTML = `
      <defs>
        <linearGradient id="eq-fill" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${color}" stop-opacity="0.35"/>
          <stop offset="100%" stop-color="${color}" stop-opacity="0.02"/>
        </linearGradient>
      </defs>
      <line x1="0" y1="${y(0)}" x2="${W}" y2="${y(0)}"
        stroke="var(--hint)" stroke-width="0.6" stroke-dasharray="4 4" opacity="0.6"/>
      <polygon points="${area}" fill="url(#eq-fill)"/>
      <polyline points="${line}" fill="none" stroke="${color}" stroke-width="2"
        stroke-linejoin="round" stroke-linecap="round"/>
      <circle cx="${x(vals.length - 1)}" cy="${y(last)}" r="3.5" fill="${color}"
        stroke="var(--bg)" stroke-width="1.5"/>`;

    $("equity-last").innerHTML = pnlSpan(last, "%", 1);
    $("equity-axis").innerHTML = `
      <span>${fmtDate(pts[0].time)}</span>
      <span>макс ${fmt(Math.max(...vals), 1)}% · мин ${fmt(Math.min(...vals), 1)}%</span>
      <span>${fmtDate(pts[pts.length - 1].time)}</span>`;
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
    ? list.map((t) => `
      <div class="row">
        <div class="main">
          <div><b>${t.symbol}</b>${dirBadge(t.direction)}
            ${t.status !== "closed" ? `<span class="badge accent">открыта</span>` : ""}
          </div>
          <div class="sub">$${fmtPrice(t.entry)} → ${t.closePrice ? "$" + fmtPrice(t.closePrice) : "…"}</div>
        </div>
        <div class="right">
          <div>${t.pnlPct != null ? pnlSpan(t.pnlPct, "%") : "—"}</div>
          <div class="sub">${fmtTime(t.openedAt)}${t.closedAt ? " · " + fmtDur(t.openedAt, t.closedAt) : ""}</div>
        </div>
      </div>`).join("")
    : `<div class="muted">Ничего не найдено</div>`;
}

async function loadTrades() {
  try {
    allTrades = await api("/api/trades?limit=100");
    renderTrades();
    if (lastStats) renderTradeStats(lastStats);
  } catch (e) {
    $("trades-list").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

document.querySelectorAll("#trade-filters .chip").forEach((c) =>
  c.addEventListener("click", () => {
    haptic();
    tradeFilter = c.dataset.f;
    document.querySelectorAll("#trade-filters .chip").forEach((b) =>
      b.classList.toggle("active", b === c));
    renderTrades();
  }));

// ── Сигналы ──────────────────────────────────────────────────────────────────

async function loadSignals() {
  try {
    const signals = await api("/api/signals?limit=100");
    $("signals-list").innerHTML = signals.length
      ? signals.map((s) => `
        <div class="row">
          <div class="main">
            <div><b>${s.symbol}</b>${dirBadge(s.direction)}
              <span class="badge">${s.strategy}</span><span class="badge">${s.timeframe}</span>
            </div>
            <div class="sub">вход $${fmtPrice(s.entry)} · SL $${fmtPrice(s.stopLoss)}
              ${s.takeProfits && s.takeProfits.length ? "· TP " + s.takeProfits.map(fmtPrice).join(" / ") : ""}</div>
          </div>
          <div class="right">
            <div><span class="badge accent">RR ${fmt(s.rrRatio, 1)}</span> ${s.sent ? "✅" : "⏳"}</div>
            <div class="sub">${fmtTime(s.time)}</div>
          </div>
        </div>`).join("")
      : `<div class="muted">Сигналов пока нет</div>`;
  } catch (e) {
    $("signals-list").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

// ── Вкладки, обновление ──────────────────────────────────────────────────────

const TABS = ["overview", "trades", "signals"];
const loaded = {};

function show(tab) {
  haptic();
  document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  document.querySelectorAll(".page").forEach((p) => p.classList.toggle("active", p.id === tab));
  $("tab-glider").style.transform = `translateX(${TABS.indexOf(tab) * 100}%)`;
  if (tab === "trades" && !loaded.trades) { loaded.trades = true; loadTrades(); }
  if (tab === "signals" && !loaded.signals) { loaded.signals = true; loadSignals(); }
}

document.querySelectorAll(".tab").forEach((b) =>
  b.addEventListener("click", () => show(b.dataset.tab)));

$("refresh").addEventListener("click", async () => {
  haptic("medium");
  const btn = $("refresh");
  btn.classList.add("spin");
  const jobs = [loadOverview(), loadEquity(), loadStats()];
  if (loaded.trades) jobs.push(loadTrades());
  if (loaded.signals) jobs.push(loadSignals());
  await Promise.allSettled(jobs);
  btn.classList.remove("spin");
});

loadOverview();
loadEquity();
loadStats();
setInterval(loadOverview, 10000);
setInterval(loadStats, 60000);
