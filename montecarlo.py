"""
Monte Carlo simulation + max consecutive loss streak analysis.
"""

import random
import math
import statistics

# ── Actual trade sequence from baseline backtest ───────────────────────────
# (ordered by entry date, True = win, False = loss/zero)
ACTUAL_TRADES = [
    # id,   win/loss  pnl
    (2095, True,  +26.92),
    (2096, False,  -7.17),
    (2097, True,  +27.30),
    (2098, False,  -9.67),
    (2099, False,  -7.22),
    (2100, False, -10.05),
    (2101, False,  -6.22),
    (2102, True,   +1.94),
    # 2103 not filled
    (2104, True,   +4.58),
    (2105, True,  +27.85),
    # 2106 not filled
    (2107, True,   +4.76),
    (2108, True,   +1.80),
    # 2109 not filled
    (2110, True,  +26.15),
    (2111, False,  -7.58),
    (2112, False,  -7.15),
    (2113, False,  -7.52),
    (2114, True,   +1.74),
    (2115, True,   +1.79),
    (2116, True,   +6.38),
    (2117, True,   +3.41),
    (2118, True,  +10.19),
    (2119, True,   +0.62),
    (2120, True,   +9.73),
    (2121, False,  -8.43),
    (2122, True,  +20.96),
    (2123, True,   +3.40),
    (2124, True,   +4.01),
    (2125, True,   +9.72),
    # 2126 not filled
    (2127, True,   +4.17),
    (2128, True,   +6.24),
    # 2129 not filled
    (2130, True,    0.00),  # no close yet → 0
    (2131, True,   +3.48),
    (2132, False,  -9.74),
    (2133, False,  -9.09),
    (2134, True,    0.00),  # no close yet → 0
]

# ── Max consecutive losses in actual sequence ──────────────────────────────
def max_consec_losses(trades):
    max_streak = cur = 0
    streak_end = 0
    cur_start = 0
    best_start = best_end = 0
    for i, (sid, win, pnl) in enumerate(trades):
        if not win:
            if cur == 0:
                cur_start = i
            cur += 1
            if cur > max_streak:
                max_streak = cur
                best_start = cur_start
                best_end   = i
        else:
            cur = 0
    return max_streak, best_start, best_end

streak, s_start, s_end = max_consec_losses(ACTUAL_TRADES)
print("═"*62)
print("  MAX CONSECUTIVE LOSSES — actual trade sequence")
print("═"*62)
print(f"  Max streak : {streak} losses in a row")
print(f"  Trades     : #{ACTUAL_TRADES[s_start][0]} → #{ACTUAL_TRADES[s_end][0]}")
print(f"  PnLs       : ", end="")
for t in ACTUAL_TRADES[s_start:s_end+1]:
    print(f"{t[2]:+.2f}%", end="  ")
print()

# Also show all streaks ≥ 2
print(f"\n  All loss streaks ≥ 2:")
cur = 0
streak_trades = []
for i, (sid, win, pnl) in enumerate(ACTUAL_TRADES):
    if not win:
        streak_trades.append((sid, pnl))
        cur += 1
    else:
        if cur >= 2:
            ids = [f"#{t[0]}" for t in streak_trades]
            pnls = [f"{t[1]:+.2f}%" for t in streak_trades]
            print(f"    {cur}x: {', '.join(ids)}  pnl: {', '.join(pnls)}")
        cur = 0
        streak_trades = []
if cur >= 2:
    ids  = [f"#{t[0]}" for t in streak_trades]
    pnls = [f"{t[1]:+.2f}%" for t in streak_trades]
    print(f"    {cur}x: {', '.join(ids)}  pnl: {', '.join(pnls)}")

# ── Monte Carlo ────────────────────────────────────────────────────────────
print()
print("═"*62)
print("  MONTE CARLO  —  10,000 iterations × 35 trades")
print("  WR=60.7%  avgWin=+15.67%  avgLoss=−8.94%")
print("  Capital=$1,389  risk=10%/trade  leverage=3x")
print("═"*62)

N_ITER   = 10_000
N_TRADES = 35
WIN_PROB = 0.607
AVG_WIN  = 15.67   # % on position
AVG_LOSS = -8.94   # % on position
START    = 1_389.0
FRACTION = 0.10
LEVERAGE = 3.0

# We model each trade outcome as drawn from a distribution.
# To preserve variance (not just mean), we use a two-point distribution
# that matches the empirical win/loss %s from actual trades.
# Win PnLs (actual): compute std to sample from normal
actual_wins  = [t[2] for t in ACTUAL_TRADES if t[1] and t[2] > 0]
actual_losses= [t[2] for t in ACTUAL_TRADES if not t[1] or t[2] < 0]

win_mean  = statistics.mean(actual_wins)   if actual_wins  else AVG_WIN
win_std   = statistics.stdev(actual_wins)  if len(actual_wins)>1  else 8.0
loss_mean = statistics.mean(actual_losses) if actual_losses else AVG_LOSS
loss_std  = statistics.stdev(actual_losses)if len(actual_losses)>1 else 1.5

print(f"\n  Win  distribution : mean={win_mean:+.2f}%  std={win_std:.2f}%  (from {len(actual_wins)} actual wins)")
print(f"  Loss distribution : mean={loss_mean:+.2f}%  std={loss_std:.2f}%  (from {len(actual_losses)} actual losses)")
print(f"\n  Running {N_ITER:,} simulations…")

rng = random.Random(42)

final_balances = []
max_dd_list    = []
max_streak_list= []
ruined         = 0  # balance < 1

for _ in range(N_ITER):
    bal   = START
    peak  = START
    maxdd = 0.0
    consec_loss = 0
    max_consec  = 0

    for _ in range(N_TRADES):
        margin = bal * FRACTION
        if rng.random() < WIN_PROB:
            pnl_pct = rng.gauss(win_mean, win_std)
            pnl_pct = max(pnl_pct, 0.01)   # wins stay positive
            consec_loss = 0
        else:
            pnl_pct = rng.gauss(loss_mean, loss_std)
            pnl_pct = min(pnl_pct, -0.01)  # losses stay negative
            consec_loss += 1
            max_consec = max(max_consec, consec_loss)

        gain = margin * LEVERAGE * pnl_pct / 100
        gain = max(gain, -margin)   # liquidation cap
        bal += gain
        if bal <= 0:
            bal = 0
            ruined += 1
            break

        if bal > peak:
            peak = bal
        dd = (peak - bal) / peak * 100
        if dd > maxdd:
            maxdd = dd

    final_balances.append(bal)
    max_dd_list.append(maxdd)
    max_streak_list.append(max_consec)

final_balances.sort()
max_dd_list.sort()
max_streak_list.sort()

# ── Statistics ─────────────────────────────────────────────────────────────
def percentile(sorted_list, p):
    idx = int(len(sorted_list) * p / 100)
    return sorted_list[min(idx, len(sorted_list)-1)]

p5   = percentile(final_balances,  5)
p25  = percentile(final_balances, 25)
p50  = percentile(final_balances, 50)
p75  = percentile(final_balances, 75)
p95  = percentile(final_balances, 95)

loss30_count = sum(1 for b in final_balances if b <= START * 0.70)
loss50_count = sum(1 for b in final_balances if b <= START * 0.50)
double_count = sum(1 for b in final_balances if b >= START * 2.0)

prob_loss30 = loss30_count / N_ITER * 100
prob_loss50 = loss50_count / N_ITER * 100
prob_double = double_count / N_ITER * 100
prob_ruin   = ruined       / N_ITER * 100

avg_final = statistics.mean(final_balances)
med_final = statistics.median(final_balances)

p95_dd = percentile(max_dd_list, 95)
med_dd = percentile(max_dd_list, 50)

p95_streak = percentile(max_streak_list, 95)
med_streak = percentile(max_streak_list, 50)

print()
print("─"*62)
print("  FINAL BALANCE DISTRIBUTION")
print("─"*62)
print(f"  Mean final balance   : ${avg_final:>10,.2f}  ({(avg_final/START-1)*100:+.1f}%)")
print(f"  Median final balance : ${med_final:>10,.2f}  ({(med_final/START-1)*100:+.1f}%)")
print()
print(f"   5th percentile      : ${p5:>10,.2f}  ({(p5/START-1)*100:+.1f}%)")
print(f"  25th percentile      : ${p25:>10,.2f}  ({(p25/START-1)*100:+.1f}%)")
print(f"  50th percentile      : ${p50:>10,.2f}  ({(p50/START-1)*100:+.1f}%)")
print(f"  75th percentile      : ${p75:>10,.2f}  ({(p75/START-1)*100:+.1f}%)")
print(f"  95th percentile      : ${p95:>10,.2f}  ({(p95/START-1)*100:+.1f}%)")
print()
print("─"*62)
print("  RISK METRICS")
print("─"*62)
print(f"  P(loss ≥ 30%)        : {prob_loss30:>6.2f}%   ({loss30_count:,} / {N_ITER:,} runs)")
print(f"  P(loss ≥ 50%)        : {prob_loss50:>6.2f}%   ({loss50_count:,} / {N_ITER:,} runs)")
print(f"  P(ruin, balance → 0) : {prob_ruin:>6.2f}%   ({ruined:,} / {N_ITER:,} runs)")
print(f"  P(2x capital)        : {prob_double:>6.2f}%   ({double_count:,} / {N_ITER:,} runs)")
print()
print(f"  Median max drawdown  : {med_dd:.1f}%")
print(f"  95th pct max DD      : {p95_dd:.1f}%")
print()
print("─"*62)
print("  MAX CONSECUTIVE LOSSES (in simulations)")
print("─"*62)
streak_dist = {}
for s in max_streak_list:
    streak_dist[s] = streak_dist.get(s, 0) + 1
for k in sorted(streak_dist):
    pct = streak_dist[k] / N_ITER * 100
    bar = "█" * int(pct / 1)
    print(f"  {k} in a row : {pct:5.1f}%  {bar}")
print(f"\n  Median max streak    : {med_streak}")
print(f"  95th pct max streak  : {p95_streak}")
print("─"*62)

# ── Equity curve histogram ─────────────────────────────────────────────────
print()
print("  FINAL BALANCE HISTOGRAM  (each █ ≈ 100 runs)")
buckets = {}
step = 200
for b in final_balances:
    bucket = int(b // step) * step
    buckets[bucket] = buckets.get(bucket, 0) + 1
for lo in sorted(buckets):
    hi  = lo + step
    cnt = buckets[lo]
    bar = "█" * (cnt // 100)
    marker = " ← start" if lo <= START < hi else ""
    print(f"  ${lo:>6}–{hi:<6} : {cnt:>5}  {bar}{marker}")
print("═"*62)
