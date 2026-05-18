"""
Detailed analysis of losing signals:
- BTC 3-day change before signal
- Long/Short ratio at signal time
- Funding rate at signal time
- Distance to MA200 daily (alt coin)
"""

import csv
import requests
import time as time_mod
from datetime import datetime, timezone, timedelta

# SL signals from backtest output
SL_SIGNAL_IDS = {2096, 2098, 2099, 2100, 2101, 2111, 2112, 2113, 2121, 2132, 2133}

BASE = "https://api.bybit.com/v5"

def get(path, params):
    r = requests.get(BASE + path, params=params, timeout=10)
    d = r.json()
    if d["retCode"] != 0:
        raise RuntimeError(f"{path}: {d['retMsg']}")
    return d["result"]

def kline(symbol, interval, start_ms, end_ms, limit=200):
    res = get("/market/kline", {
        "category": "linear", "symbol": symbol,
        "interval": interval, "start": start_ms, "end": end_ms, "limit": limit
    })
    rows = res["list"]  # [[startMs, o, h, l, c, vol, turnover], ...]
    # sorted descending from API, reverse to ascending
    rows = sorted(rows, key=lambda x: int(x[0]))
    return [(int(r[0]), float(r[4])) for r in rows]  # (ms, close)

def btc_3d_change(sig_date: datetime) -> float:
    """BTC % change in 3 days before signal."""
    end_ms   = int(sig_date.timestamp() * 1000)
    start_ms = int((sig_date - timedelta(days=4)).timestamp() * 1000)
    candles = kline("BTCUSDT", "D", start_ms, end_ms)
    if len(candles) < 2:
        return float("nan")
    # last candle before signal, and 3 days prior
    closes = [c for ms, c in candles if ms < end_ms]
    if len(closes) < 2:
        return float("nan")
    return (closes[-1] / closes[max(0, len(closes)-4)] - 1) * 100

def ma200_distance(symbol: str, sig_date: datetime) -> float:
    """Alt price vs MA200 daily (%), negative = below MA200."""
    end_ms   = int(sig_date.timestamp() * 1000)
    start_ms = int((sig_date - timedelta(days=210)).timestamp() * 1000)
    candles = kline(symbol, "D", start_ms, end_ms, limit=200)
    closes = [c for ms, c in candles if ms < end_ms]
    if len(closes) < 200:
        return float("nan")
    ma200 = sum(closes[-200:]) / 200
    price = closes[-1]
    return (price / ma200 - 1) * 100

def funding_rate(symbol: str, sig_date: datetime) -> float:
    """Most recent funding rate at or before signal time."""
    end_ms   = int(sig_date.timestamp() * 1000)
    start_ms = int((sig_date - timedelta(days=2)).timestamp() * 1000)
    res = get("/market/funding/history", {
        "category": "linear", "symbol": symbol,
        "startTime": start_ms, "endTime": end_ms, "limit": 20
    })
    rows = res.get("list", [])
    if not rows:
        return float("nan")
    # rows sorted descending by fundingRateTimestamp
    rows = sorted(rows, key=lambda x: int(x["fundingRateTimestamp"]))
    return float(rows[-1]["fundingRate"]) * 100  # to %

def ls_ratio(symbol: str, sig_date: datetime) -> float:
    """Long/Short ratio (longs/shorts) closest to signal time (1h period)."""
    end_ms   = int(sig_date.timestamp() * 1000)
    start_ms = int((sig_date - timedelta(hours=24)).timestamp() * 1000)
    try:
        res = get("/market/account-ratio", {
            "category": "linear", "symbol": symbol,
            "period": "1h", "startTime": start_ms, "endTime": end_ms, "limit": 24
        })
        rows = res.get("list", [])
        if not rows:
            return float("nan")
        rows = sorted(rows, key=lambda x: int(x["timestamp"]))
        r = rows[-1]
        buy  = float(r["buyRatio"])
        sell = float(r["sellRatio"])
        if sell == 0:
            return float("nan")
        return buy / sell
    except Exception:
        return float("nan")

def load_signals(csv_path):
    signals = []
    with open(csv_path) as f:
        for row in csv.DictReader(f):
            sid = int(row["signal_id"])
            if sid not in SL_SIGNAL_IDS:
                continue
            dt = datetime.strptime(
                row["date"] + " " + row["time"], "%Y-%m-%d %H:%M"
            ).replace(tzinfo=timezone.utc)
            signals.append({
                "id":     sid,
                "symbol": row["symbol"],
                "date":   dt,
                "entry_low":  float(row["entry_low"]),
                "entry_high": float(row["entry_high"]),
                "sl":         float(row["sl"]),
            })
    signals.sort(key=lambda x: x["date"])
    return signals

def main():
    signals = load_signals("signals_trader3.csv")
    print(f"Analyzing {len(signals)} losing signals...\n")

    rows = []
    for s in signals:
        print(f"  #{s['id']} {s['symbol']} {s['date'].date()} ...", end=" ", flush=True)
        try:
            btc_chg = btc_3d_change(s["date"])
            time_mod.sleep(0.15)
            ma200   = ma200_distance(s["symbol"], s["date"])
            time_mod.sleep(0.15)
            fr      = funding_rate(s["symbol"], s["date"])
            time_mod.sleep(0.15)
            lsr     = ls_ratio(s["symbol"], s["date"])
            time_mod.sleep(0.15)
            sl_dist = (s["entry_low"] - s["sl"]) / s["entry_low"] * 100
            rows.append({**s, "btc_3d": btc_chg, "ma200_dist": ma200,
                         "funding": fr, "ls_ratio": lsr, "sl_dist": sl_dist})
            print("ok")
        except Exception as e:
            print(f"ERROR: {e}")
            rows.append({**s, "btc_3d": float("nan"), "ma200_dist": float("nan"),
                         "funding": float("nan"), "ls_ratio": float("nan"), "sl_dist": 0})

    # ── Print table ──────────────────────────────────────────────────────────
    W = 108
    print("\n" + "═"*W)
    print(f"  {'#ID':<6} {'Symbol':<14} {'Date':<11} "
          f"{'BTC 3d%':>8} {'MA200d%':>8} {'Funding%':>9} {'L/S ratio':>10} {'SL dist%':>9}")
    print("─"*W)
    for r in rows:
        def f(v): return f"{v:+.2f}" if v == v else "  n/a "
        def fp(v): return f"{v:.3f}" if v == v else "  n/a "
        print(f"  #{r['id']:<5} {r['symbol']:<14} {r['date'].date()} "
              f"  {f(r['btc_3d']):>8} {f(r['ma200_dist']):>8} "
              f"  {f(r['funding']):>9} {fp(r['ls_ratio']):>10}  {f(r['sl_dist']):>8}")
    print("═"*W)

    # ── Pattern analysis ─────────────────────────────────────────────────────
    valid = [r for r in rows if r["btc_3d"] == r["btc_3d"]]  # not nan
    def avg(key):
        vals = [r[key] for r in valid if r[key] == r[key]]
        return sum(vals)/len(vals) if vals else float("nan")

    print(f"\n  PATTERN SUMMARY (avg across {len(valid)} signals)")
    print("─"*50)
    print(f"  BTC 3d change before signal  : {avg('btc_3d'):+.2f}%")
    print(f"  Alt distance from MA200 daily: {avg('ma200_dist'):+.2f}%")
    print(f"  Funding rate at signal       : {avg('funding'):+.4f}%")
    print(f"  Long/Short ratio             : {avg('ls_ratio'):.3f}")
    print(f"  SL distance from entry       : {avg('sl_dist'):+.2f}%")
    print()

    # Per-metric thresholds
    btc_neg = sum(1 for r in valid if r["btc_3d"] < 0)
    btc_pos = sum(1 for r in valid if r["btc_3d"] > 0)
    above_ma = sum(1 for r in valid if r["ma200_dist"] > 20)
    high_fund = sum(1 for r in valid if r["funding"] > 0.01)
    ls_below1 = sum(1 for r in valid if 0 < r["ls_ratio"] < 1)

    print(f"  BTC was falling 3d before    : {btc_neg}/{len(valid)} signals")
    print(f"  BTC was rising  3d before    : {btc_pos}/{len(valid)} signals")
    print(f"  Alt >20% above MA200         : {above_ma}/{len(valid)} signals  (extended/overheated)")
    print(f"  Funding rate elevated >0.01% : {high_fund}/{len(valid)} signals")
    print(f"  L/S ratio <1.0 (more shorts) : {ls_below1}/{len(valid)} signals")
    print("─"*50)

if __name__ == "__main__":
    main()
