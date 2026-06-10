const tg = window.Telegram.WebApp;
tg.ready();
tg.expand();

const $ = (id) => document.getElementById(id);

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

const fmt = (n, d = 2) =>
  n == null ? "—" : n.toLocaleString("ru-RU", { minimumFractionDigits: d, maximumFractionDigits: d });

const fmtPrice = (n) => {
  if (n == null) return "—";
  const d = n >= 100 ? 2 : n >= 1 ? 4 : 6;
  return n.toLocaleString("ru-RU", { maximumFractionDigits: d });
};

const pnlSpan = (v, suffix = "$") => {
  const cls = v >= 0 ? "pnl-pos" : "pnl-neg";
  const sign = v >= 0 ? "+" : "";
  return `<span class="${cls}">${sign}${fmt(v)}${suffix}</span>`;
};

const fmtTime = (iso) => {
  const d = new Date(iso);
  return d.toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit" }) +
    " " + d.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" });
};

// ── Обзор ────────────────────────────────────────────────────────────────────

async function loadOverview() {
  try {
    const ov = await api("/api/overview");
    const b = ov.balance;
    $("balance-card").innerHTML = `
      <div class="balance-equity">$${fmt(b.equity)}</div>
      <div class="balance-row"><span>Баланс</span><b>$${fmt(b.walletBalance)}</b></div>
      <div class="balance-row"><span>Нереализ. PnL</span><b>${pnlSpan(b.unrealisedPnl)}</b></div>
      <div class="balance-row"><span>Доступно</span><b>$${fmt(b.available)}</b></div>`;

    $("positions").innerHTML = ov.positions.length
      ? ov.positions.map((p) => `
        <div class="pos">
          <div class="pos-head">
            <span class="pos-symbol">${p.symbol}
              <span class="side-${p.side.toLowerCase()}">${p.side === "Buy" ? "LONG" : "SHORT"} ${p.leverage}x</span>
            </span>
            ${pnlSpan(p.unrealisedPnl)}
          </div>
          <div class="pos-detail">
            <span>${p.size} @ $${fmtPrice(p.entryPrice)}</span>
            <span>Mark $${fmtPrice(p.markPrice)}</span>
          </div>
          <div class="pos-detail">
            <span>SL ${p.stopLoss ? "$" + fmtPrice(p.stopLoss) : "—"}</span>
            <span>TP ${p.takeProfit ? "$" + fmtPrice(p.takeProfit) : "—"}</span>
          </div>
        </div>`).join("")
      : `<div class="muted">Нет открытых позиций</div>`;

    $("updated").textContent = "Обновлено " + new Date(ov.updatedAt).toLocaleTimeString("ru-RU");
  } catch (e) {
    $("balance-card").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

async function loadEquity() {
  try {
    const pts = await api("/api/equity");
    const svg = $("equity-chart");
    if (pts.length < 2) {
      svg.outerHTML = `<div class="muted">Недостаточно закрытых сделок для графика</div>`;
      return;
    }
    const W = 320, H = 120, pad = 6;
    const vals = pts.map((p) => p.cumPct);
    const min = Math.min(0, ...vals), max = Math.max(0, ...vals);
    const x = (i) => pad + (i / (pts.length - 1)) * (W - 2 * pad);
    const y = (v) => H - pad - ((v - min) / (max - min || 1)) * (H - 2 * pad);
    const line = vals.map((v, i) => `${x(i)},${y(v)}`).join(" ");
    svg.innerHTML = `
      <line class="zero" x1="0" y1="${y(0)}" x2="${W}" y2="${y(0)}"></line>
      <polyline points="${line}"></polyline>`;
  } catch (e) { /* график не критичен */ }
}

// ── Сделки ───────────────────────────────────────────────────────────────────

async function loadTrades() {
  try {
    const trades = await api("/api/trades?limit=100");
    $("trades-list").innerHTML = trades.length
      ? trades.map((t) => `
        <div class="row">
          <div>
            <div><b>${t.symbol}</b>
              <span class="side-${t.direction.toLowerCase() === "long" || t.direction === "Buy" ? "buy" : "sell"}">${t.direction}</span>
              ${t.status !== "closed" ? `<span class="badge">${t.status}</span>` : ""}
            </div>
            <div class="sub">${fmtTime(t.openedAt)} → ${t.closedAt ? fmtTime(t.closedAt) : "открыта"}</div>
          </div>
          <div>${t.pnlPct != null ? pnlSpan(t.pnlPct, "%") : "—"}</div>
        </div>`).join("")
      : `<div class="muted">Сделок пока нет</div>`;
  } catch (e) {
    $("trades-list").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

// ── Сигналы ──────────────────────────────────────────────────────────────────

async function loadSignals() {
  try {
    const signals = await api("/api/signals?limit=100");
    $("signals-list").innerHTML = signals.length
      ? signals.map((s) => `
        <div class="row">
          <div>
            <div><b>${s.symbol}</b>
              <span class="side-${s.direction.toLowerCase() === "long" || s.direction === "Buy" ? "buy" : "sell"}">${s.direction}</span>
              <span class="badge">${s.strategy} ${s.timeframe}</span>
            </div>
            <div class="sub">${fmtTime(s.time)} · вход $${fmtPrice(s.entry)} · SL $${fmtPrice(s.stopLoss)} · RR ${fmt(s.rrRatio, 1)}</div>
          </div>
          <div>${s.sent ? "✅" : "⏳"}</div>
        </div>`).join("")
      : `<div class="muted">Сигналов пока нет</div>`;
  } catch (e) {
    $("signals-list").innerHTML = `<div class="error">Ошибка: ${e.message}</div>`;
  }
}

// ── Вкладки и автообновление ─────────────────────────────────────────────────

const loaded = {};
function show(tab) {
  document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  document.querySelectorAll(".page").forEach((p) => p.classList.toggle("active", p.id === tab));
  if (tab === "trades" && !loaded.trades) { loaded.trades = true; loadTrades(); }
  if (tab === "signals" && !loaded.signals) { loaded.signals = true; loadSignals(); }
}

document.querySelectorAll(".tab").forEach((b) =>
  b.addEventListener("click", () => show(b.dataset.tab)));

loadOverview();
loadEquity();
setInterval(loadOverview, 10000);
