"""
Filter backtest: test MA200 and L/S ratio filters against baseline.
Fetches metrics for ALL signals, caches to metrics_cache.json.
Uses hardcoded backtest results (PnL, entry/close dates) from Go backtester.
"""

import csv, json, math, os, time as time_mod, requests
from datetime import datetime, timezone, timedelta
from collections import defaultdict

BASE = "https://api.bybit.com/v5"
CACHE_FILE = "metrics_cache.json"

# ── Hardcoded backtest results (from Go backtester output) ─────────────────
# Fields: id, symbol, filled, hit_sl, pnl_pct, entry_dt, close_dt
# Dates in UTC. close_dt = None if not filled.
RESULTS = [
    (2095,"ALGOUSDT",  True, False, +26.92, "2026-03-22","2026-05-07"),
    (2096,"AVAXUSDT",  True, True,  -7.17,  "2026-03-24","2026-03-29"),
    (2097,"ONDOUSDT",  True, False, +27.30, "2026-04-12","2026-05-08"),
    (2098,"DOTUSDT",   True, True,  -9.67,  "2026-03-25","2026-04-02"),
    (2099,"ENAUSDT",   True, True,  -7.22,  "2026-03-26","2026-03-27"),
    (2100,"APTUSDT",   True, True,  -10.05, "2026-03-28","2026-04-02"),
    (2101,"XLMUSDT",   True, True,  -6.22,  "2026-03-29","2026-04-12"),
    (2102,"XRPUSDT",   True, False, +1.94,  "2026-03-29","2026-04-16"),
    (2103,"TONUSDT",   False,False, 0.0,    None,         None),
    (2104,"SOLUSDT",   True, False, +4.58,  "2026-04-04","2026-05-11"),
    (2105,"ARBUSDT",   True, False, +27.85, "2026-04-05","2026-05-08"),
    (2106,"APTUSDT",   False,False, 0.0,    None,         None),
    (2107,"XTZUSDT",   True, False, +4.76,  "2026-04-05","2026-05-10"),
    (2108,"ADAUSDT",   True, False, +1.80,  "2026-04-07","2026-05-08"),
    (2109,"DASHUSDT",  False,False, 0.0,    None,         None),
    (2110,"ICPUSDT",   True, False, +26.15, "2026-04-08","2026-05-09"),
    (2111,"WLDUSDT",   True, True,  -7.58,  "2026-04-19","2026-05-01"),
    (2112,"WLFIUSDT",  True, True,  -7.15,  "2026-04-11","2026-04-27"),
    (2113,"RENDERUSDT",True, True,  -7.52,  "2026-04-14","2026-04-29"),
    (2114,"ASTERUSDT", True, False, +1.74,  "2026-04-25","2026-05-09"),
    (2115,"AVAXUSDT",  True, False, +1.79,  "2026-04-19","2026-05-08"),
    (2116,"CHZUSDT",   True, False, +6.38,  "2026-04-16","2026-04-24"),
    (2117,"DOTUSDT",   True, False, +3.41,  "2026-04-19","2026-05-13"),
    (2118,"UNIUSDT",   True, False, +10.19, "2026-04-21","2026-05-10"),
    (2119,"BCHUSDT",   True, False, +0.62,  "2026-04-23","2026-05-06"),
    (2120,"TAOUSDT",   True, False, +9.73,  "2026-04-25","2026-05-06"),
    (2121,"XLMUSDT",   True, True,  -8.43,  "2026-04-25","2026-05-04"),
    (2122,"JUPUSDT",   True, False, +20.96, "2026-04-29","2026-05-10"),
    (2123,"LINKUSDT",  True, False, +3.40,  "2026-04-29","2026-05-09"),
    (2124,"HYPEUSDT",  True, False, +4.01,  "2026-05-12","2026-05-15"),
    (2125,"NEARUSDT",  True, False, +9.72,  "2026-05-02","2026-05-10"),
    (2126,"FETUSDT",   False,False, 0.0,    None,         None),
    (2127,"ATOMUSDT",  True, False, +4.17,  "2026-05-05","2026-05-12"),
    (2128,"AEROUSDT",  True, False, +6.24,  "2026-05-08","2026-05-10"),
    (2129,"SUIUSDT",   False,False, 0.0,    None,         None),
    (2130,"AAVEUSDT",  True, False, 0.0,    "2026-05-15", None),   # no close yet
    (2131,"IMXUSDT",   True, False, +3.48,  "2026-05-12","2026-05-14"),
    (2132,"NIGHTUSDT", True, True,  -9.74,  "2026-05-12","2026-05-15"),
    (2133,"FILUSDT",   True, True,  -9.09,  "2026-05-13","2026-05-16"),
    (2134,"SKYUSDT",   True, False, 0.0,    "2026-05-14", None),   # no close yet
    (2135,"ENAUSDT",   False,False, 0.0,    None,         None),
    (2136,"ICPUSDT",   False,False, 0.0,    None,         None),
]

# ── API helpers ────────────────────────────────────────────────────────────

def get(path, params):
    r = requests.get(BASE + path, params=params, timeout=10)
    d = r.json()
    if d["retCode"] != 0:
        raise RuntimeError(f"{path}: {d['retMsg']}")
    return d["result"]

def kline_closes(symbol, interval, start_ms, end_ms, limit=200):
    res = get("/market/kline", {
        "category":"linear","symbol":symbol,
        "interval":interval,"start":start_ms,"end":end_ms,"limit":limit
    })
    rows = sorted(res["list"], key=lambda x: int(x[0]))
    return [(int(r[0]), float(r[4])) for r in rows]

def fetch_ma200_dist(symbol, sig_date):
    end_ms   = int(sig_date.timestamp()*1000)
    start_ms = int((sig_date - timedelta(days=215)).timestamp()*1000)
    candles  = kline_closes(symbol, "D", start_ms, end_ms, limit=200)
    closes   = [c for ms,c in candles if ms < end_ms]
    if len(closes) < 200:
        return None
    ma200 = sum(closes[-200:]) / 200
    return (closes[-1]/ma200 - 1) * 100

def fetch_ls_ratio(symbol, sig_date):
    end_ms   = int(sig_date.timestamp()*1000)
    start_ms = int((sig_date - timedelta(hours=24)).timestamp()*1000)
    try:
        res  = get("/market/account-ratio", {
            "category":"linear","symbol":symbol,
            "period":"1h","startTime":start_ms,"endTime":end_ms,"limit":24
        })
        rows = sorted(res.get("list",[]), key=lambda x: int(x["timestamp"]))
        if not rows:
            return None
        r = rows[-1]
        sell = float(r["sellRatio"])
        return float(r["buyRatio"])/sell if sell else None
    except Exception:
        return None

# ── Cache management ───────────────────────────────────────────────────────

def load_cache():
    if os.path.exists(CACHE_FILE):
        with open(CACHE_FILE) as f:
            return json.load(f)
    return {}

def save_cache(c):
    with open(CACHE_FILE,"w") as f:
        json.dump(c, f, indent=2)

def get_metrics(cache):
    """Fetch (or return cached) MA200 dist and L/S ratio for all signals."""
    # Signal dates: read from csv
    sig_dates = {}
    with open("signals_trader3.csv") as f:
        for row in csv.DictReader(f):
            sid = int(row["signal_id"])
            dt = datetime.strptime(row["date"]+" "+row["time"],"%Y-%m-%d %H:%M").replace(tzinfo=timezone.utc)
            sig_dates[sid] = (row["symbol"], dt)

    changed = False
    for sid, (sym, dt) in sig_dates.items():
        key = str(sid)
        if key not in cache:
            cache[key] = {}
        entry = cache[key]

        if "ma200" not in entry:
            try:
                v = fetch_ma200_dist(sym, dt)
                entry["ma200"] = v
                print(f"    #{sid} {sym} MA200={'n/a' if v is None else f'{v:+.1f}%'}", flush=True)
                time_mod.sleep(0.2)
                changed = True
            except Exception as e:
                print(f"    #{sid} {sym} MA200 ERROR: {e}")
                entry["ma200"] = None

        if "ls" not in entry:
            try:
                v = fetch_ls_ratio(sym, dt)
                entry["ls"] = v
                print(f"    #{sid} {sym} L/S={'n/a' if v is None else f'{v:.3f}'}", flush=True)
                time_mod.sleep(0.2)
                changed = True
            except Exception as e:
                print(f"    #{sid} {sym} L/S ERROR: {e}")
                entry["ls"] = None

    if changed:
        save_cache(cache)
    return cache

# ── Portfolio simulation (event-driven, 3x fixed, 10% margin) ─────────────

def portfolio_sim(active_results, leverage=3.0, fraction=0.10, start=1000.0):
    """
    active_results: list of (entry_dt, close_dt, pnl_pct) for non-skipped filled trades with close_dt.
    Returns (final_balance, max_drawdown_pct).
    """
    events = []
    for i,(ed,cd,pnl) in enumerate(active_results):
        if ed: events.append((ed, "open", i))
        if cd: events.append((cd, "close", i))
    events.sort(key=lambda x: (x[0], 0 if x[1]=="close" else 1))

    bal   = start
    peak  = start
    maxdd = 0.0
    alloc = [0.0]*len(active_results)

    for dt, kind, idx in events:
        if kind == "open":
            alloc[idx] = bal * fraction
        else:
            raw  = alloc[idx] * leverage * active_results[idx][2] / 100
            gain = max(raw, -alloc[idx])
            bal += gain
            bal  = max(bal, 0)
            if bal > peak: peak = bal
            dd = (peak-bal)/peak*100 if peak else 0
            if dd > maxdd: maxdd = dd

    return bal, maxdd

# ── Analysis ───────────────────────────────────────────────────────────────

def parse_dt(s):
    if not s: return None
    return datetime.strptime(s, "%Y-%m-%d").replace(tzinfo=timezone.utc)

def run_filter(name, results_meta, cache, filter_fn):
    """
    results_meta: list of dicts with all signal data + metrics.
    filter_fn(m) -> True means SKIP.
    """
    filled   = [m for m in results_meta if m["filled"]]
    active   = [m for m in filled if not filter_fn(m)]
    skipped  = [m for m in filled if filter_fn(m)]

    # Stats on active trades
    wins   = [m for m in active if m["pnl"] > 0]
    losses = [m for m in active if m["pnl"] < 0]
    decided = len(wins)+len(losses)
    wr      = len(wins)/decided*100 if decided else 0
    avg_pnl = sum(m["pnl"] for m in active)/len(active) if active else 0

    # Portfolio sim (only trades with both entry+close date)
    sim_input = [(parse_dt(m["entry_dt"]), parse_dt(m["close_dt"]), m["pnl"])
                 for m in active if m["entry_dt"] and m["close_dt"]]
    final, maxdd = portfolio_sim(sim_input)

    # Skipped breakdown
    skipped_wins   = sum(1 for m in skipped if m["pnl"] > 0)
    skipped_losses = sum(1 for m in skipped if m["pnl"] < 0)
    skipped_zero   = sum(1 for m in skipped if m["pnl"] == 0)

    return {
        "name":           name,
        "total_filled":   len(filled),
        "active":         len(active),
        "skipped":        len(skipped),
        "wins":           len(wins),
        "losses":         len(losses),
        "wr":             wr,
        "avg_pnl":        avg_pnl,
        "final":          final,
        "maxdd":          maxdd,
        "skip_wins":      skipped_wins,
        "skip_losses":    skipped_losses,
        "skip_zero":      skipped_zero,
        "skipped_ids":    [m["id"] for m in skipped],
    }

def main():
    cache = load_cache()

    # Check if any metrics missing
    missing = False
    for row in RESULTS:
        if str(row[0]) not in cache or "ma200" not in cache.get(str(row[0]),{}) or "ls" not in cache.get(str(row[0]),{}):
            missing = True
            break
    if missing:
        print("[*] Fetching missing metrics (cached in metrics_cache.json)…")
        cache = get_metrics(cache)
    else:
        print(f"[*] All metrics loaded from cache ({CACHE_FILE})")

    # Build rich result list
    results_meta = []
    for row in RESULTS:
        sid, sym, filled, hit_sl, pnl, entry_dt, close_dt = row
        m = cache.get(str(sid), {})
        results_meta.append({
            "id":       sid,
            "symbol":   sym,
            "filled":   filled,
            "hit_sl":   hit_sl,
            "pnl":      pnl,
            "entry_dt": entry_dt,
            "close_dt": close_dt,
            "ma200":    m.get("ma200"),   # None = not enough history
            "ls":       m.get("ls"),
        })

    # ── Filters ──────────────────────────────────────────────────────────
    def fA(m):  # skip if alt > 25% below MA200
        v = m["ma200"]
        return v is not None and v < -25.0

    def fB(m):  # skip if L/S > 2.0
        v = m["ls"]
        return v is not None and v > 2.0

    def fC(m): return fA(m) or fB(m)
    def baseline(m): return False

    variants = [
        ("Baseline",    baseline),
        ("Filter A (MA200<-25%)", fA),
        ("Filter B (L/S>2.0)",   fB),
        ("Filter A+B",           fC),
    ]

    results_table = [run_filter(name, results_meta, cache, fn) for name,fn in variants]

    # ── Print comparison table ────────────────────────────────────────────
    W = 100
    print("\n" + "═"*W)
    print("  FILTER COMPARISON  (fixed 3x leverage, 10% margin, $1 000 start)")
    print("═"*W)
    print(f"  {'Variant':<26} {'Filled':>6} {'Skip':>5} {'Trad':>5} {'Wins':>5} {'Loss':>5} "
          f"{'WR%':>6} {'AvgPnL':>8} {'MaxDD':>7} {'Final $':>9}")
    print("─"*W)
    for r in results_table:
        print(f"  {r['name']:<26} {r['total_filled']:>6} {r['skipped']:>5} {r['active']:>5} "
              f"{r['wins']:>5} {r['losses']:>5} "
              f"{r['wr']:>5.1f}% {r['avg_pnl']:>+7.2f}% {r['maxdd']:>6.1f}%  ${r['final']:>8.2f}")
    print("═"*W)

    # ── Skipped signals breakdown ─────────────────────────────────────────
    print()
    for r in results_table[1:]:  # skip baseline
        if r["skipped"] == 0:
            continue
        total_skip = r["skipped"]
        print(f"  {r['name']}  —  {total_skip} skipped signals:")
        print(f"    Would have been profitable : {r['skip_wins']}  (missed upside)")
        print(f"    Would have been losses     : {r['skip_losses']}  (saved from loss)")
        print(f"    Break-even/not closed      : {r['skip_zero']}")
        # Which signals?
        skipped_detail = [m for m in results_meta if m["filled"] and m["id"] in r["skipped_ids"]]
        for m in sorted(skipped_detail, key=lambda x: x["id"]):
            ma_s = f"{m['ma200']:+.1f}%" if m["ma200"] is not None else "  n/a "
            ls_s = f"{m['ls']:.2f}"      if m["ls"]   is not None else " n/a"
            outcome = "PROFIT" if m["pnl"]>0 else ("LOSS" if m["pnl"]<0 else "zero")
            print(f"    #{m['id']:>4} {m['symbol']:<14} MA200={ma_s}  L/S={ls_s}  pnl={m['pnl']:+.2f}%  → {outcome}")
        print()

    # ── Per-signal metrics table ──────────────────────────────────────────
    print("─"*W)
    print(f"  {'#ID':<6} {'Symbol':<14} {'Filled':>6} {'PnL%':>8} "
          f"{'MA200d%':>9} {'L/S':>7}  {'fA':>4} {'fB':>4} {'fC':>4}")
    print("─"*W)
    for m in results_meta:
        if not m["filled"]: continue
        ma_s = f"{m['ma200']:+.1f}%" if m["ma200"] is not None else "   n/a"
        ls_s = f"{m['ls']:.2f}"      if m["ls"]   is not None else "  n/a"
        skip_a = "SKIP" if fA(m) else "  ok"
        skip_b = "SKIP" if fB(m) else "  ok"
        skip_c = "SKIP" if fC(m) else "  ok"
        print(f"  #{m['id']:<5} {m['symbol']:<14} {'YES':>6} {m['pnl']:>+7.2f}% "
              f"{ma_s:>9} {ls_s:>7}  {skip_a} {skip_b} {skip_c}")
    print("─"*W)

if __name__ == "__main__":
    main()
