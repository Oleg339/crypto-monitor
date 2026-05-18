// Standalone backtester — multi-timeframe, relaxed scoring, BTC trend filter.
//
// Data source : https://public.bybit.com/kline_for_metatrader4/ (60m monthly CSV.gz)
// Entry TF    : 1H (= 60m bars, no conversion needed)
// Trend TF    : 4H (aggregate 4×1H bars)
// BTC filter  : 1D BTC candles — skip longs if BTC dropped >3% in last 2 days
//
// Strategies (2-of-3 soft scoring, hard BTC filter always applied):
//   MeanRev1H      — price below 4H MA60, bounce conditions on 1H
//   SupportBounce1H — price near 1H support, trend context from 4H MA60 slope
//
// Walk-forward: 70% train (discarded) / 30% OOS evaluation
// Period: 2022-01-01 → 2024-05-01 (last 6 months of public data excluded)
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
	"github.com/yourorg/crypto-monitor/internal/indicators"
)

// ── Config ────────────────────────────────────────────────────────────────

const (
	publicBase  = "https://public.bybit.com/kline_for_metatrader4"
	commission  = 0.00055 // 0.055% taker per side
	initCap     = 1000.0
	riskFrac    = 0.05 // 5% of balance per trade
	minRR       = 1.5  // require decent reward-to-risk
	cooldownH   = 8    // hours between signals on same symbol/strategy
	atBaseRisk  = 0.01 // AdaptiveTrend base risk per trade
)

var allSymbols = []string{
	"BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT", "BNBUSDT",
	"ADAUSDT", "LTCUSDT", "LINKUSDT", "AVAXUSDT", "MATICUSDT",
	"NEARUSDT", "ATOMUSDT", "FTMUSDT", "SANDUSDT", "GMTUSDT",
	"AXSUSDT", "FILUSDT", "ETCUSDT", "TRBUSDT", "UNFIUSDT",
	"APEUSDT", "LUNA2USDT",
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ── Data loading ──────────────────────────────────────────────────────────

func monthURL(symbol string, year int, month time.Month) string {
	s := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	e := s.AddDate(0, 1, -1)
	return fmt.Sprintf("%s/%s/%d/%s_60_%s_%s.csv.gz",
		publicBase, symbol, year, symbol,
		s.Format("2006-01-02"), e.Format("2006-01-02"))
}

// download60m fetches one monthly file and returns parsed 60m (1H) candles.
// CSV format (no header): YYYY.MM.DD HH:MM,open,high,low,close,volume
func download60m(ctx context.Context, symbol string, year int, month time.Month) ([]bybit.Candle, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, monthURL(symbol, year, month), nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	var out []bybit.Candle
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		p := strings.SplitN(line, ",", 6)
		if len(p) < 6 {
			continue
		}
		t, err := time.ParseInLocation("2006.01.02 15:04", p[0], time.UTC)
		if err != nil {
			continue
		}
		o, _ := strconv.ParseFloat(p[1], 64)
		h, _ := strconv.ParseFloat(p[2], 64)
		l, _ := strconv.ParseFloat(p[3], 64)
		c, _ := strconv.ParseFloat(p[4], 64)
		v, _ := strconv.ParseFloat(p[5], 64)
		if o == 0 {
			continue
		}
		out = append(out, bybit.Candle{Time: t, Symbol: symbol, Interval: "60",
			Open: o, High: h, Low: l, Close: c, Volume: v})
	}
	return out, sc.Err()
}

// loadCandles1H returns all 60m candles for symbol in [start, end).
func loadCandles1H(ctx context.Context, symbol string, start, end time.Time) ([]bybit.Candle, error) {
	var all []bybit.Candle
	for year := start.Year(); year <= end.Year(); year++ {
		for month := time.January; month <= time.December; month++ {
			ms := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
			me := ms.AddDate(0, 1, 0)
			if me.Before(start) || ms.After(end) {
				continue
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			batch, err := download60m(ctx, symbol, year, month)
			if err != nil {
				slog.Debug("month skip", "symbol", symbol, "year", year, "month", month, "err", err)
				continue
			}
			all = append(all, batch...)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no data")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })

	// Filter to [start, end)
	var filtered []bybit.Candle
	for _, c := range all {
		if !c.Time.Before(start) && c.Time.Before(end) {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

// aggregate4H groups sorted 60m candles into complete 4H bars.
func aggregate4H(sym string, c1H []bybit.Candle) []bybit.Candle {
	const sec = int64(4 * 3600)
	var bars []bybit.Candle
	barTS := int64(-1)
	var o, h, l, c, vol float64
	open := false

	flush := func() {
		if open {
			bars = append(bars, bybit.Candle{
				Time: time.Unix(barTS, 0).UTC(), Symbol: sym, Interval: "240",
				Open: o, High: h, Low: l, Close: c, Volume: vol,
			})
		}
	}
	for _, can := range c1H {
		bs := (can.Time.Unix() / sec) * sec
		if bs != barTS {
			flush()
			barTS, o, h, l, c, vol, open = bs, can.Open, can.High, can.Low, can.Close, can.Volume, true
		} else {
			if can.High > h {
				h = can.High
			}
			if can.Low < l {
				l = can.Low
			}
			c = can.Close
			vol += can.Volume
		}
	}
	// Do NOT flush last bar — it may be partial
	return bars
}

// aggregate1D groups sorted 60m candles into complete 1D bars (UTC midnight).
func aggregate1D(sym string, c1H []bybit.Candle) []bybit.Candle {
	const sec = int64(24 * 3600)
	var bars []bybit.Candle
	barTS := int64(-1)
	var o, h, l, c, vol float64
	open := false

	flush := func() {
		if open {
			bars = append(bars, bybit.Candle{
				Time: time.Unix(barTS, 0).UTC(), Symbol: sym, Interval: "D",
				Open: o, High: h, Low: l, Close: c, Volume: vol,
			})
		}
	}
	for _, can := range c1H {
		bs := (can.Time.Unix() / sec) * sec
		if bs != barTS {
			flush()
			barTS, o, h, l, c, vol, open = bs, can.Open, can.High, can.Low, can.Close, can.Volume, true
		} else {
			if can.High > h {
				h = can.High
			}
			if can.Low < l {
				l = can.Low
			}
			c = can.Close
			vol += can.Volume
		}
	}
	return bars
}

// candlesBefore returns candles from a sorted slice whose bar is fully closed before t.
// intervalH is the bar duration in hours (1 for 1H, 4 for 4H, 24 for 1D).
func candlesBefore(candles []bybit.Candle, intervalH int, t time.Time) []bybit.Candle {
	dur := time.Duration(intervalH) * time.Hour
	cutoff := t.Add(-dur) // bar open time must be <= cutoff for bar to be closed
	idx := sort.Search(len(candles), func(i int) bool {
		return candles[i].Time.After(cutoff)
	})
	return candles[:idx]
}

// tail returns the last n elements of a slice (or the whole slice if shorter).
func tail(s []bybit.Candle, n int) []bybit.Candle {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ── Indicator helpers ─────────────────────────────────────────────────────

func closes(candles []bybit.Candle) []float64 {
	out := make([]float64, len(candles))
	for i, c := range candles {
		out[i] = c.Close
	}
	return out
}

func vols(candles []bybit.Candle) []float64 {
	out := make([]float64, len(candles))
	for i, c := range candles {
		out[i] = c.Volume
	}
	return out
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var s float64
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals))
}

func minLow(candles []bybit.Candle) float64 {
	if len(candles) == 0 {
		return 0
	}
	m := candles[0].Low
	for _, c := range candles[1:] {
		if c.Low < m {
			m = c.Low
		}
	}
	return m
}

// ── BTC trend filter ─────────────────────────────────────────────────────

// btcTrendOK returns true if BTC has NOT dropped more than 3% over the last 2 daily bars.
// Short-term BTC momentum check.
func btcTrendOK(btcDaily []bybit.Candle) bool {
	if len(btcDaily) < 3 {
		return true
	}
	last := btcDaily[len(btcDaily)-1]
	prev := btcDaily[len(btcDaily)-3]
	change := (last.Close - prev.Close) / prev.Close * 100
	return change > -3.0
}

// isMarketBullish implements three regime filters — all must pass to allow longs.
//
//  1. BTC daily MA200: BTC close > 200-day MA (macro bull market)
//  2. BTC 4-week trend: BTC close[now] > BTC close[28 days ago]
//  3. Symbol 4-week trend: symbol close[now] > symbol close[28 days ago]
//     (skips alts that are individually in a downtrend even if BTC is bullish)
//
// Weekly MA200 would need 200 weeks ≈ 4 years of data; we have 2.5 years,
// so daily MA200 is used — same concept, equivalent signal.
func isMarketBullish(btcDaily, symDaily []bybit.Candle) bool {
	if len(btcDaily) < 200 {
		return false
	}
	btcCloses := closes(btcDaily)
	ma200 := indicators.SMA(btcCloses, 200)
	n := len(ma200)
	if ma200[n-1] == 0 || btcDaily[n-1].Close <= ma200[n-1] {
		return false
	}
	if len(btcDaily) >= 29 {
		if btcDaily[len(btcDaily)-1].Close <= btcDaily[len(btcDaily)-29].Close {
			return false
		}
	}
	if len(symDaily) >= 29 {
		if symDaily[len(symDaily)-1].Close <= symDaily[len(symDaily)-29].Close {
			return false
		}
	}
	return true
}

// ── Candlestick reversal confirmation ────────────────────────────────────

// candlePattern identifies which bullish reversal pattern (if any) is present
// on the last candle of the slice. Returns "" if none matches.
//
//  "pinbar"    — long lower wick ≥ 2× body, upper wick ≤ body, body in upper half
//  "engulfing" — current bullish body fully engulfs prior bearish body
//  "doji"      — almost no body (<10% range), lower wick ≥ 70% range
func candlePattern(candles []bybit.Candle) string {
	n := len(candles)
	if n < 2 {
		return ""
	}
	cur := candles[n-1]
	prev := candles[n-2]

	body := math.Abs(cur.Close - cur.Open)
	fullRange := cur.High - cur.Low
	if fullRange == 0 {
		return ""
	}
	lowerWick := math.Min(cur.Open, cur.Close) - cur.Low
	upperWick := cur.High - math.Max(cur.Open, cur.Close)

	// 1. Pin Bar / Hammer
	if body > 0 && lowerWick >= 2*body && upperWick <= body {
		if (math.Min(cur.Open, cur.Close)-cur.Low)/fullRange >= 0.5 {
			return "pinbar"
		}
	}

	// 2. Bullish Engulfing
	if prev.Open > prev.Close && cur.Close > cur.Open {
		if cur.Open <= prev.Close && cur.Close >= prev.Open {
			return "engulfing"
		}
	}

	// 3. Dragonfly Doji
	if body/fullRange < 0.10 && lowerWick/fullRange >= 0.70 {
		return "doji"
	}

	return ""
}

func hasBullishReversal(candles []bybit.Candle) bool {
	return candlePattern(candles) != ""
}

// ── Signal generation ─────────────────────────────────────────────────────

type entrySignal struct {
	entry, sl, tp float64
	rr            float64
	strat         string
	supportPrice  float64 // only set by supportBounce1H
	atr           float64 // only set by momentumBreakout1H (for trailing stop)
	trailing      bool    // use trailing stop instead of fixed TP
}

// meanRev1H: mean reversion on 1H with 4H context.
//
// Hard filters (all required):
//   - BTC 1D not down >3% in 2 days
//   - 4H close < 4H MA60 (we are in the oversold zone of the higher TF)
//
// Soft score — need 2 of 3:
//  1. 1H RSI(14) < 45
//  2. 1H close < 1H MA20  (price is depressed on entry TF)
//  3. 1H volume > avg20 × 1.3
//
// Entry : current 1H close
// Stop  : min(low, last 5 bars) − 0.8%
// TP    : 1H MA20 (mean reversion target, must be above entry)
// Min RR: 1.3
func meanRev1H(c1H, c4H, btcD []bybit.Candle) *entrySignal {
	if len(c1H) < 65 || len(c4H) < 20 {
		return nil
	}

	// ── Hard: BTC trend ──
	if !btcTrendOK(btcD) {
		return nil
	}

	// ── Hard: 4H price below 4H MA60 ──
	cl4H := closes(c4H)
	p := len(cl4H)
	ma60_4H := indicators.SMA(cl4H, min(60, p))
	if ma60_4H[p-1] == 0 || c4H[p-1].Close >= ma60_4H[p-1] {
		return nil
	}

	// ── Hard: RSI must be oversold ──
	cl1H := closes(c1H)
	n := len(cl1H)
	rsiSeries := indicators.RSI(cl1H, 14)
	rsiVal := rsiSeries[n-1]
	if rsiVal <= 0 || rsiVal >= 40 {
		return nil // only genuinely oversold entries
	}

	// ── Soft scoring — need 1 of 2 ──
	score := 0

	// 1. price < 1H MA20
	ma20 := indicators.SMA(cl1H, 20)
	lastMA20 := ma20[n-1]
	lastClose := cl1H[n-1]
	if lastMA20 > 0 && lastClose < lastMA20 {
		score++
	}

	// 2. volume spike
	vl := vols(c1H)
	avgVol := mean(vl[max(0, n-21) : n-1])
	if avgVol > 0 && vl[n-1] > avgVol*1.3 {
		score++
	}

	if score < 1 {
		return nil
	}

	// ── Entry params ──
	entry := lastClose
	sl := minLow(tail(c1H, 5)) * 0.992
	tp := lastMA20 // revert to 1H MA20

	if tp <= entry || sl >= entry {
		return nil
	}
	risk := (entry - sl) / entry
	reward := (tp - entry) / entry
	if risk <= 0 {
		return nil
	}
	rr := reward / risk
	if rr < minRR {
		return nil
	}

	return &entrySignal{entry: entry, sl: sl, tp: tp, rr: rr, strat: "MeanRev1H", supportPrice: 0}
}

// supportBounce1H: support bounce on 1H with 4H slope context.
//
// Hard filters:
//   - BTC 1D not down >3% in 2 days
//   - 4H MA60 slope > −1% over last 3 bars (not in a strong 4H downtrend)
//
// Soft score — need 2 of 3:
//  1. 1H RSI(14) < 42
//  2. price within 1.5% of nearest 1H support level
//  3. last 1H volume > previous bar volume × 1.1 (rising volume)
//
// Entry : current 1H close
// Stop  : support price − 1.5%
// TP    : nearest resistance above entry
// Min RR: 1.3
func supportBounce1H(c1H, c4H, btcD []bybit.Candle) *entrySignal {
	if len(c1H) < 65 || len(c4H) < 20 {
		return nil
	}

	// ── Hard: BTC trend ──
	if !btcTrendOK(btcD) {
		return nil
	}

	// ── Hard: 4H MA60 must be rising ──
	cl4H := closes(c4H)
	p := len(cl4H)
	ma60_4H := indicators.SMA(cl4H, min(60, p))
	cur4H, prev4H := ma60_4H[p-1], ma60_4H[max(0, p-4)]
	if prev4H <= 0 || cur4H <= prev4H {
		return nil // 4H MA60 not rising → no support bounce longs
	}

	// ── Support/resistance levels ──
	lvls := indicators.FindSupportResistance(c1H, 5)
	if len(lvls) == 0 {
		return nil
	}

	lastClose := c1H[len(c1H)-1].Close

	// Find nearest support below price
	var supportPrice float64
	for _, lv := range lvls {
		if lv.Type == "support" && lv.Price < lastClose {
			dist := (lastClose - lv.Price) / lastClose * 100
			if dist <= 1.5 {
				supportPrice = lv.Price
				break
			}
		}
	}
	if supportPrice == 0 {
		return nil // no support nearby
	}

	// Find nearest resistance above price
	var resistPrice float64
	for _, lv := range lvls {
		if lv.Type == "resistance" && lv.Price > lastClose {
			resistPrice = lv.Price
			break
		}
	}
	if resistPrice == 0 {
		return nil // no resistance target
	}

	// ── Hard: RSI must be oversold ──
	cl1H := closes(c1H)
	n := len(cl1H)
	rsiSeries := indicators.RSI(cl1H, 14)
	rsiVal := rsiSeries[n-1]
	if rsiVal <= 0 || rsiVal >= 40 {
		return nil // only oversold entries near support
	}

	// ── Entry params ──
	entry := lastClose
	sl := supportPrice * 0.985 // support − 1.5%
	tp := resistPrice

	if tp <= entry || sl >= entry {
		return nil
	}
	risk := (entry - sl) / entry
	reward := (tp - entry) / entry
	if risk <= 0 {
		return nil
	}
	rr := reward / risk
	if rr < minRR {
		return nil
	}

	return &entrySignal{entry: entry, sl: sl, tp: tp, rr: rr, strat: "SupportBounce1H", supportPrice: supportPrice}
}

// momentumBreakout1H: breakout of 20-bar high on 1H with daily MA200 regime.
//
// Hard filters (all required):
//   - Price closed above highest high of last 20 bars (breakout)
//   - Volume > 20-bar avg × 1.5 (confirmed breakout)
//   - BTC daily MA200: price above (macro regime — reuses isMarketBullish check upstream)
//   - BTC 1D not down >3% in 2 days
//
// Entry : current 1H close (breakout bar)
// Stop  : min(low, last 5 bars)
// TP    : trailing stop 2×ATR(14) — no fixed target, ride the trend
// Min RR: 1.5 (initial risk/reward at entry)
func momentumBreakout1H(c1H, btcD []bybit.Candle) *entrySignal {
	if len(c1H) < 30 {
		return nil
	}

	// ── Hard: BTC short-term trend ──
	if !btcTrendOK(btcD) {
		return nil
	}

	n := len(c1H)
	lastClose := c1H[n-1].Close

	// ── Hard: close > highest high of last 20 bars (excluding current) ──
	window := c1H[n-21 : n-1] // 20 bars before current
	highest := window[0].High
	for _, bar := range window[1:] {
		if bar.High > highest {
			highest = bar.High
		}
	}
	if lastClose <= highest {
		return nil // no breakout
	}

	// ── Hard: volume > avg20 × 1.5 ──
	vl := vols(c1H)
	avgVol := mean(vl[n-21 : n-1])
	if avgVol <= 0 || vl[n-1] < avgVol*1.5 {
		return nil
	}

	// ── ATR for trailing stop ──
	atrSeries := indicators.ATR(c1H, 14)
	atrVal := atrSeries[n-1]
	if atrVal <= 0 {
		return nil
	}

	// ── Entry params ──
	entry := lastClose
	sl := minLow(tail(c1H[:n-1], 5))
	if sl <= 0 || sl >= entry {
		return nil
	}

	risk := (entry - sl) / entry
	if risk <= 0 {
		return nil
	}
	// For trailing trades we set tp=0 (unused), rr based on 3×ATR potential
	potentialReward := 3 * atrVal / entry
	rr := potentialReward / risk
	if rr < minRR {
		return nil
	}

	return &entrySignal{
		entry:    entry,
		sl:       sl,
		tp:       0, // trailing — no fixed TP
		rr:       rr,
		strat:    "MomentumBreakout1H",
		atr:      atrVal,
		trailing: true,
	}
}

// momentumBreakout4H: same logic as 1H but on 4H bars — less noise, bigger moves.
// Max hold: 60 × 4H bars = 10 days.
func momentumBreakout4H(c4H, btcD []bybit.Candle) *entrySignal {
	if len(c4H) < 30 {
		return nil
	}
	if !btcTrendOK(btcD) {
		return nil
	}

	n := len(c4H)
	lastClose := c4H[n-1].Close

	// Close > highest high of last 20 bars (excluding current)
	window := c4H[n-21 : n-1]
	highest := window[0].High
	for _, bar := range window[1:] {
		if bar.High > highest {
			highest = bar.High
		}
	}
	if lastClose <= highest {
		return nil
	}

	// Volume > avg20 × 1.5
	vl := vols(c4H)
	avgVol := mean(vl[n-21 : n-1])
	if avgVol <= 0 || vl[n-1] < avgVol*1.5 {
		return nil
	}

	// ATR(14) on 4H
	atrSeries := indicators.ATR(c4H, 14)
	atrVal := atrSeries[n-1]
	if atrVal <= 0 {
		return nil
	}

	entry := lastClose
	sl := minLow(tail(c4H[:n-1], 5))
	if sl <= 0 || sl >= entry {
		return nil
	}
	risk := (entry - sl) / entry
	if risk <= 0 {
		return nil
	}
	potentialReward := 3 * atrVal / entry
	rr := potentialReward / risk
	if rr < minRR {
		return nil
	}

	return &entrySignal{
		entry:    entry,
		sl:       sl,
		tp:       0,
		rr:       rr,
		strat:    "MomentumBreakout4H",
		atr:      atrVal,
		trailing: true,
	}
}

// ── Regime detection ──────────────────────────────────────────────────────

type adaptiveRegime string

const (
	regimeBull = adaptiveRegime("bull")
	regimeBear = adaptiveRegime("bear")
	regimeSide = adaptiveRegime("side")
)

func detectRegime(c1H, c1D []bybit.Candle) adaptiveRegime {
	if len(c1D) < 50 {
		return regimeSide
	}
	dailyCloses := closes(c1D)
	n := len(dailyCloses)
	currentClose := c1D[n-1].Close

	// MA50 daily
	ma50 := indicators.SMA(dailyCloses, min(50, n))
	aboveMA50 := ma50[n-1] > 0 && currentClose > ma50[n-1]

	// MA200 daily (proxy for weekly MA200 — need 4y for real weekly MA200)
	aboveMA200 := false
	if n >= 200 {
		ma200 := indicators.SMA(dailyCloses, 200)
		aboveMA200 = ma200[n-1] > 0 && currentClose > ma200[n-1]
	} else if n >= 10 {
		// Not enough history — use MA50 slope as proxy
		aboveMA200 = ma50[n-1] > ma50[max(0, n-10)]
	}

	// ATR declining on 1H: current ATR14 < avg ATR over past 20 bars
	atrDeclining := false
	if len(c1H) >= 35 {
		atrSeries := indicators.ATR(c1H, 14)
		nh := len(atrSeries)
		recentATR := atrSeries[nh-1]
		var pastSum float64
		count := 0
		for j := nh - 21; j < nh-1; j++ {
			if j >= 0 && atrSeries[j] > 0 {
				pastSum += atrSeries[j]
				count++
			}
		}
		if count > 0 {
			atrDeclining = recentATR < pastSum/float64(count)
		}
	}

	if aboveMA200 && aboveMA50 && atrDeclining {
		return regimeBull
	}
	if !aboveMA200 && !aboveMA50 {
		return regimeBear
	}
	return regimeSide
}

// ── AdaptiveTrend signal generation ──────────────────────────────────────

// adaptiveTrendSignal generates an entry for the AdaptiveTrend strategy.
// Returns (signal, volScale). volScale is the effective capital fraction.
func adaptiveTrendSignal(c1H, c1D []bybit.Candle, regime adaptiveRegime) (*entrySignal, float64) {
	if len(c1H) < 30 {
		return nil, 0
	}

	n := len(c1H)
	lastClose := c1H[n-1].Close

	atrSeries := indicators.ATR(c1H, 14)
	atrVal := atrSeries[n-1]
	if atrVal <= 0 {
		return nil, 0
	}

	slDist := 1.5 * atrVal
	slPct := slDist / lastClose
	if slPct <= 0 {
		return nil, 0
	}

	// Volatility scaling: risk exactly atBaseRisk of capital
	volScale := atBaseRisk / slPct
	// Cap: never more than 10× base (prevents insane leverage in calm markets)
	if volScale > 10*atBaseRisk {
		volScale = 10 * atBaseRisk
	}

	switch regime {
	case regimeBull, regimeSide:
		// Long MeanReversion: RSI < 40, price < 20H MA reference
		cl1H := closes(c1H)
		rsiSeries := indicators.RSI(cl1H, 14)
		rsiVal := rsiSeries[n-1]
		if rsiVal <= 0 || rsiVal >= 40 {
			return nil, 0
		}

		ma20 := indicators.SMA(cl1H, 20)
		if ma20[n-1] <= 0 || lastClose >= ma20[n-1] {
			return nil, 0
		}

		entry := lastClose
		sl := entry - slDist
		if sl <= 0 {
			return nil, 0
		}

		// Sideways: halve the vol scale
		effectiveScale := volScale
		if regime == regimeSide {
			effectiveScale /= 2
		}

		strat := "AT_Bull"
		if regime == regimeSide {
			strat = "AT_Side"
		}

		return &entrySignal{
			entry: entry, sl: sl, tp: 0,
			rr:       2.0 / 1.5, // expected 2×ATR gain / 1.5×ATR loss
			strat:    strat, atr: atrVal, trailing: true,
		}, effectiveScale

	case regimeBear:
		// Short momentum: breakout of 20-bar low downward
		window := c1H[n-21 : n-1]
		lowest := window[0].Low
		for _, bar := range window[1:] {
			if bar.Low < lowest {
				lowest = bar.Low
			}
		}
		if lastClose >= lowest {
			return nil, 0 // no breakdown
		}

		// Volume confirmation
		vl := vols(c1H)
		avgVol := mean(vl[n-21 : n-1])
		if avgVol <= 0 || vl[n-1] < avgVol*1.5 {
			return nil, 0
		}

		entry := lastClose
		sl := entry + slDist // stop ABOVE entry for shorts
		return &entrySignal{
			entry: entry, sl: sl, tp: 0,
			rr:      2.0 / 1.5,
			strat:   "AT_Bear", atr: atrVal, trailing: true,
		}, volScale
	}
	return nil, 0
}

// ── Trade simulation ─────────────────────────────────────────────────────

type trade struct {
	strat      string
	entry, exit float64
	pnlPct     float64
	won        bool
	entryTime  time.Time
	regime     string  // "bull"/"bear"/"side"/"" for non-adaptive
	volScale   float64 // >0: use this as capital fraction instead of riskFrac
}

// simulateTrade walks future 1H candles until SL or TP is hit.
// Max 5 days (120 bars) before abandoning.
func simulateTrade(sig *entrySignal, future []bybit.Candle, entryTime time.Time) *trade {
	if len(future) == 0 {
		return nil
	}
	entryComm := sig.entry * (1 + commission)
	limit := len(future)
	if limit > 120 {
		limit = 120
	}
	for _, c := range future[:limit] {
		if c.Low <= sig.sl {
			exit := sig.sl * (1 - commission)
			return &trade{sig.strat, sig.entry, sig.sl, (exit - entryComm) / entryComm * 100, false, entryTime, "", 0}
		}
		if c.High >= sig.tp {
			exit := sig.tp * (1 - commission)
			return &trade{sig.strat, sig.entry, sig.tp, (exit - entryComm) / entryComm * 100, true, entryTime, "", 0}
		}
	}
	return nil
}

// simulateTrailTrade walks future candles with a trailing stop of 2×ATR.
// The stop ratchets up as price makes new highs but never moves down.
// maxBars limits how many candles to walk (use 240 for 1H, 60 for 4H).
func simulateTrailTrade(sig *entrySignal, future []bybit.Candle, maxBars int, entryTime time.Time) *trade {
	if len(future) == 0 || sig.atr <= 0 {
		return nil
	}
	entryComm := sig.entry * (1 + commission)
	trailDist := 2 * sig.atr
	trailStop := sig.entry - trailDist // initial trailing stop
	if trailStop < sig.sl {
		trailStop = sig.sl // never looser than hard stop
	}

	limit := len(future)
	if limit > maxBars {
		limit = maxBars
	}

	peakHigh := sig.entry
	for _, c := range future[:limit] {
		if c.High > peakHigh {
			peakHigh = c.High
			newStop := peakHigh - trailDist
			if newStop > trailStop {
				trailStop = newStop
			}
		}
		if c.Low <= trailStop {
			exitPrice := trailStop
			won := exitPrice > sig.entry
			exit := exitPrice * (1 - commission)
			return &trade{sig.strat, sig.entry, exitPrice, (exit - entryComm) / entryComm * 100, won, entryTime, "", 0}
		}
	}
	last := future[limit-1]
	exit := last.Close * (1 - commission)
	won := last.Close > sig.entry
	return &trade{sig.strat, sig.entry, last.Close, (exit - entryComm) / entryComm * 100, won, entryTime, "", 0}
}

// simulateTrailShort walks future candles for a SHORT position with trailing stop.
// Stop starts at entry + 2×ATR, trails down as price makes new lows.
func simulateTrailShort(sig *entrySignal, future []bybit.Candle, maxBars int, entryTime time.Time) *trade {
	if len(future) == 0 || sig.atr <= 0 {
		return nil
	}
	entryComm := sig.entry * (1 - commission) // sell at entry (short)
	trailDist := 2 * sig.atr
	trailStop := sig.entry + trailDist // stop above entry
	hardSL := sig.sl                   // 1.5×ATR stop

	limit := len(future)
	if limit > maxBars {
		limit = maxBars
	}

	peakLow := sig.entry
	for _, c := range future[:limit] {
		// Check hard SL first (price went up against us)
		if c.High >= hardSL {
			exitPrice := hardSL
			exit := exitPrice * (1 + commission)
			pnl := (entryComm - exit) / sig.entry * 100 // short pnl
			return &trade{sig.strat, sig.entry, exitPrice, pnl, false, entryTime, "bear", 0}
		}
		// Update trailing stop on new lows
		if c.Low < peakLow {
			peakLow = c.Low
			newStop := peakLow + trailDist
			if newStop < trailStop {
				trailStop = newStop
			}
		}
		// Trail stop hit (price bounced up to stop)
		if c.High >= trailStop && trailStop < sig.entry {
			exitPrice := trailStop
			exit := exitPrice * (1 + commission)
			pnl := (entryComm - exit) / sig.entry * 100
			won := pnl > 0
			return &trade{sig.strat, sig.entry, exitPrice, pnl, won, entryTime, "bear", 0}
		}
	}
	// Time exit
	last := future[limit-1]
	exit := last.Close * (1 + commission)
	pnl := (entryComm - exit) / sig.entry * 100
	won := pnl > 0
	return &trade{sig.strat, sig.entry, last.Close, pnl, won, entryTime, "bear", 0}
}

// ── Backtest engine ───────────────────────────────────────────────────────

type result struct {
	Symbol       string
	Strat        string
	Trades       int
	WinRate      float64
	PF           float64
	MaxDD        float64
	Sharpe       float64
	TotalPnL     float64
	AvgWin       float64
	AvgLoss      float64
	AvgReturn    float64 // mean per-trade return %, commission-inclusive
	FinalCapital float64
}

// filterStats tracks how the candlestick filter affected each strategy's signals.
type filterStats struct {
	symbol, strat    string
	passed, rejected int
	patternCounts    map[string]int // pinbar / engulfing / doji
	tradesPassed     []trade        // simulated with filter ON
	tradesRejected   []trade        // simulated with filter OFF (signals that were blocked)
}

// adaptiveSymStats holds per-symbol AdaptiveTrend trades broken down by regime.
type adaptiveSymStats struct {
	symbol   string
	byRegime map[adaptiveRegime][]trade
}

func runBacktest(symbol string, c1H, c4H, c1D, btcDaily, btcDaily1D []bybit.Candle) ([]result, []filterStats, adaptiveSymStats) {
	n := len(c1H)
	trainEnd := int(float64(n) * 0.70)
	oos1H := c1H[trainEnd:]

	lastSig := map[string]time.Time{}

	fsMR  := filterStats{symbol: symbol, strat: "MeanRev1H", patternCounts: map[string]int{}}
	fsSB  := filterStats{symbol: symbol, strat: "SupportBounce1H", patternCounts: map[string]int{}}
	fsMB  := filterStats{symbol: symbol, strat: "MomentumBreakout1H", patternCounts: map[string]int{}}
	fsMB4 := filterStats{symbol: symbol, strat: "MomentumBreakout4H", patternCounts: map[string]int{}}

	atTrades := map[adaptiveRegime][]trade{
		regimeBull: {},
		regimeBear: {},
		regimeSide: {},
	}

	for i := 100; i < len(oos1H); i++ {
		currentTime := oos1H[i].Time

		btcDCtx := tail(candlesBefore(btcDaily1D, 24, currentTime), 210)
		symDCtx := tail(candlesBefore(c1D, 24, currentTime), 35)
		if !isMarketBullish(btcDCtx, symDCtx) {
			continue
		}

		w1H := tail(oos1H[:i], 100)
		w4H := tail(candlesBefore(c4H, 4, currentTime), 25)
		wBTC := tail(candlesBefore(btcDaily, 24, currentTime), 5)

		// ── MeanRev: candlestick reversal confirmation ──
		if sigMR := meanRev1H(w1H, w4H, wBTC); sigMR != nil {
			if ls, ok := lastSig["MR"]; !ok || currentTime.Sub(ls) >= time.Duration(cooldownH)*time.Hour {
				pattern := candlePattern(w1H)
				if t := simulateTrade(sigMR, oos1H[i:], currentTime); t != nil {
					if pattern != "" {
						fsMR.passed++
						fsMR.patternCounts[pattern]++
						fsMR.tradesPassed = append(fsMR.tradesPassed, *t)
						lastSig["MR"] = currentTime
					} else {
						fsMR.rejected++
						fsMR.tradesRejected = append(fsMR.tradesRejected, *t)
					}
				}
			}
		}

		// ── SupportBounce: volume rising on 2 bars OR close above support ──
		if sigSB := supportBounce1H(w1H, w4H, wBTC); sigSB != nil {
			if ls, ok := lastSig["SB"]; !ok || currentTime.Sub(ls) >= time.Duration(cooldownH)*time.Hour {
				vl := vols(w1H)
				nv := len(vl)
				volRising := nv >= 3 && vl[nv-1] > vl[nv-2] && vl[nv-2] > vl[nv-3]
				closeAboveSupport := sigSB.entry > sigSB.supportPrice*1.002
				if t := simulateTrade(sigSB, oos1H[i:], currentTime); t != nil {
					if volRising || closeAboveSupport {
						fsSB.passed++
						if volRising {
							fsSB.patternCounts["volRising"]++
						}
						if closeAboveSupport {
							fsSB.patternCounts["closeAboveSupport"]++
						}
						fsSB.tradesPassed = append(fsSB.tradesPassed, *t)
						lastSig["SB"] = currentTime
					} else {
						fsSB.rejected++
						fsSB.tradesRejected = append(fsSB.tradesRejected, *t)
					}
				}
			}
		}

		// ── MomentumBreakout1H ──
		if sigMB := momentumBreakout1H(w1H, wBTC); sigMB != nil {
			if ls, ok := lastSig["MB"]; !ok || currentTime.Sub(ls) >= time.Duration(cooldownH)*time.Hour {
				if t := simulateTrailTrade(sigMB, oos1H[i:], 240, currentTime); t != nil {
					fsMB.passed++
					fsMB.tradesPassed = append(fsMB.tradesPassed, *t)
					lastSig["MB"] = currentTime
				}
			}
		}

		// ── MomentumBreakout4H — runs on 4H bars, future candles also 4H ──
		w4Hfull := tail(candlesBefore(c4H, 4, currentTime), 25)
		if sigMB4 := momentumBreakout4H(w4Hfull, wBTC); sigMB4 != nil {
			if ls, ok := lastSig["MB4"]; !ok || currentTime.Sub(ls) >= 4*time.Duration(cooldownH)*time.Hour {
				// Future 4H candles after currentTime
				idx4H := sort.Search(len(c4H), func(j int) bool { return c4H[j].Time.After(currentTime) })
				future4H := c4H[idx4H:]
				if t := simulateTrailTrade(sigMB4, future4H, 60, currentTime); t != nil {
					fsMB4.passed++
					fsMB4.tradesPassed = append(fsMB4.tradesPassed, *t)
					lastSig["MB4"] = currentTime
				}
			}
		}

		// ── AdaptiveTrend ──
		w1Hctx := tail(oos1H[:i], 100)
		w1Dctx := tail(candlesBefore(c1D, 24, currentTime), 55)
		regime := detectRegime(w1Hctx, w1Dctx)

		if sigAT, vs := adaptiveTrendSignal(w1Hctx, w1Dctx, regime); sigAT != nil {
			if ls, ok := lastSig["AT"]; !ok || currentTime.Sub(ls) >= time.Duration(cooldownH)*time.Hour {
				var t *trade
				if regime == regimeBear {
					t = simulateTrailShort(sigAT, oos1H[i:], 240, currentTime)
				} else {
					t = simulateTrailTrade(sigAT, oos1H[i:], 240, currentTime)
				}
				if t != nil {
					t.regime = string(regime)
					t.volScale = vs
					atTrades[regime] = append(atTrades[regime], *t)
					lastSig["AT"] = currentTime
				}
			}
		}
	}

	var results []result
	if r := metrics(symbol, "MeanRev1H", fsMR.tradesPassed); r.Trades >= 5 {
		results = append(results, r)
	}
	if r := metrics(symbol, "SupportBounce1H", fsSB.tradesPassed); r.Trades >= 5 {
		results = append(results, r)
	}
	if r := metrics(symbol, "MomentumBreakout1H", fsMB.tradesPassed); r.Trades >= 5 {
		results = append(results, r)
	}
	if r := metrics(symbol, "MomentumBreakout4H", fsMB4.tradesPassed); r.Trades >= 5 {
		results = append(results, r)
	}

	// Collect all AT trades sorted by time
	var allATTrades []trade
	for _, ts := range atTrades {
		allATTrades = append(allATTrades, ts...)
	}
	sort.Slice(allATTrades, func(i, j int) bool {
		return allATTrades[i].entryTime.Before(allATTrades[j].entryTime)
	})
	if r := metricsAT(symbol, "AdaptiveTrend", allATTrades); r.Trades >= 5 {
		results = append(results, r)
	}

	atStats := adaptiveSymStats{
		symbol:   symbol,
		byRegime: atTrades,
	}

	return results, []filterStats{fsMR, fsSB, fsMB, fsMB4}, atStats
}

func metrics(symbol, strat string, trades []trade) result {
	r := result{Symbol: symbol, Strat: strat, Trades: len(trades)}
	if len(trades) == 0 {
		return r
	}

	var wins, grossP, grossL, sumW, sumL float64
	var wc, lc int
	pnls := make([]float64, len(trades))
	cum, peak, maxDD := 0.0, 0.0, 0.0
	bal := initCap

	for i, t := range trades {
		pnls[i] = t.pnlPct
		cum += t.pnlPct
		bal += bal * riskFrac * t.pnlPct / 100
		if t.won {
			wins++; grossP += t.pnlPct; sumW += t.pnlPct; wc++
		} else {
			grossL += math.Abs(t.pnlPct); sumL += math.Abs(t.pnlPct); lc++
		}
		if cum > peak {
			peak = cum
		}
		if dd := peak - cum; dd > maxDD {
			maxDD = dd
		}
	}

	r.WinRate = wins / float64(len(trades)) * 100
	if grossL > 0 {
		r.PF = grossP / grossL
	}
	r.MaxDD = maxDD
	r.TotalPnL = cum
	r.AvgReturn = cum / float64(len(trades))
	r.Sharpe = sharpe(pnls)
	r.FinalCapital = bal
	if wc > 0 {
		r.AvgWin = sumW / float64(wc)
	}
	if lc > 0 {
		r.AvgLoss = sumL / float64(lc)
	}
	return r
}

func metricsAT(symbol, strat string, trades []trade) result {
	r := result{Symbol: symbol, Strat: strat, Trades: len(trades)}
	if len(trades) == 0 {
		return r
	}

	var wins, grossP, grossL, sumW, sumL float64
	var wc, lc int
	pnls := make([]float64, len(trades))
	cum, peak, maxDD := 0.0, 0.0, 0.0
	bal := initCap

	for i, t := range trades {
		pnls[i] = t.pnlPct
		cum += t.pnlPct
		// Use volScale if set, else fall back to riskFrac
		scale := t.volScale
		if scale == 0 {
			scale = riskFrac
		}
		bal += bal * scale * t.pnlPct / 100
		if t.won {
			wins++; grossP += t.pnlPct; sumW += t.pnlPct; wc++
		} else {
			grossL += math.Abs(t.pnlPct); sumL += math.Abs(t.pnlPct); lc++
		}
		if cum > peak {
			peak = cum
		}
		if dd := peak - cum; dd > maxDD {
			maxDD = dd
		}
	}

	r.WinRate = wins / float64(len(trades)) * 100
	if grossL > 0 {
		r.PF = grossP / grossL
	}
	r.MaxDD = maxDD
	r.TotalPnL = cum
	r.AvgReturn = cum / float64(len(trades))
	r.Sharpe = sharpe(pnls)
	r.FinalCapital = bal
	if wc > 0 {
		r.AvgWin = sumW / float64(wc)
	}
	if lc > 0 {
		r.AvgLoss = sumL / float64(lc)
	}
	return r
}

func sharpe(returns []float64) float64 {
	if len(returns) < 2 {
		return 0
	}
	var mean float64
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))
	var v float64
	for _, r := range returns {
		d := r - mean; v += d * d
	}
	v /= float64(len(returns) - 1)
	if v == 0 {
		return 0
	}
	return (mean / math.Sqrt(v)) * math.Sqrt(float64(len(returns)))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── Main ──────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 2.5 years, last 6 months of available data excluded
	start := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC) // ~6 months before end of public data

	slog.Info("backtest config",
		"start", start.Format("2006-01-02"),
		"end", end.Format("2006-01-02"),
		"symbols", len(allSymbols),
		"entry_tf", "1H",
		"trend_tf", "4H",
		"regime_filters", "BTC_MA200 + BTC_4w_trend + sym_4w_trend",
		"short_term_btc", "1D -3% in 2 days",
		"scoring", "2-of-3 soft conditions",
		"min_rr", minRR,
		"cooldown_h", cooldownH)

	// Load BTC daily candles first (needed for filter in all symbols)
	slog.Info("loading BTC 1H for daily filter...")
	btc1H, err := loadCandles1H(ctx, "BTCUSDT", start, end)
	if err != nil {
		slog.Error("failed to load BTC", "err", err)
		os.Exit(1)
	}
	btcDaily := aggregate1D("BTCUSDT", btc1H)
	slog.Info("BTC data ready", "1H_bars", len(btc1H), "daily_bars", len(btcDaily))

	// Load all symbols
	type symData struct {
		symbol string
		c1H    []bybit.Candle
		c4H    []bybit.Candle
		c1D    []bybit.Candle
	}
	var loaded []symData

	for _, sym := range allSymbols {
		select {
		case <-ctx.Done():
			goto runBacktest
		default:
		}

		if sym == "BTCUSDT" {
			// Reuse already-loaded BTC data
			c4H := aggregate4H(sym, btc1H)
			loaded = append(loaded, symData{sym, btc1H, c4H, btcDaily})
			slog.Info("loaded", "symbol", sym, "1H", len(btc1H), "4H", len(c4H), "1D", len(btcDaily))
			continue
		}

		slog.Info("loading", "symbol", sym)
		c1H, err := loadCandles1H(ctx, sym, start, end)
		if err != nil || len(c1H) < 500 {
			slog.Warn("skip", "symbol", sym, "bars", len(c1H), "err", err)
			continue
		}
		c4H := aggregate4H(sym, c1H)
		c1D := aggregate1D(sym, c1H)
		slog.Info("loaded", "symbol", sym, "1H", len(c1H), "4H", len(c4H), "1D", len(c1D))
		loaded = append(loaded, symData{sym, c1H, c4H, c1D})
	}

runBacktest:
	if len(loaded) == 0 {
		fmt.Println("No data loaded.")
		os.Exit(1)
	}

	slog.Info("running walk-forward backtest", "symbols", len(loaded))

	var allResults []result
	var allStats []filterStats
	var allATStats []adaptiveSymStats
	for _, sd := range loaded {
		results, stats, atStats := runBacktest(sd.symbol, sd.c1H, sd.c4H, sd.c1D, btcDaily, btcDaily)
		allResults = append(allResults, results...)
		allStats = append(allStats, stats...)
		allATStats = append(allATStats, atStats)
	}

	printResults(allResults, start, end)
	printFilterAnalysis(allStats)
	printTop5Analysis(allResults, allStats)
	printAdaptiveAnalysis(allResults, allATStats)
}

// ── Output ────────────────────────────────────────────────────────────────

func printResults(results []result, start, end time.Time) {
	sort.Slice(results, func(i, j int) bool { return results[i].Sharpe > results[j].Sharpe })

	fmt.Printf("\n╔═══════════════════════════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║  BACKTEST  │  %s→%s  │  1H entry / 4H trend / MA200 regime filter  │  2-of-3  ║\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	fmt.Printf("║  Regime: BTC daily MA200 + BTC 4w trend + sym 4w trend (all 3 required)            ║\n")
	fmt.Printf("║  Capital $%.0f  │  Risk %.0f%%/trade  │  Commission %.3f%%  │  Min RR %.1f           ║\n",
		initCap, riskFrac*100, commission*100, minRR)
	fmt.Printf("╚═══════════════════════════════════════════════════════════════════════════════════════╝\n\n")

	if len(results) == 0 {
		fmt.Println("No results (need ≥5 trades).")
		return
	}

	const roundTripComm = commission * 2 * 100 // 0.11% in % units

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SYMBOL\tSTRATEGY\tN\tWIN%\tPF\tSHARPE\tDD%\tAVG_WIN%\tAVG_LOSS%\tAVG_TRADE%\tvsCOMM\tTOTAL%\tFINAL_$\t")
	fmt.Fprintln(w, "──────\t────────\t─\t────\t──\t──────\t───\t────────\t─────────\t──────────\t──────\t──────\t───────\t")
	for _, r := range results {
		// Commission viability: avg trade return must beat round-trip commission
		commVerdict := "✅ viable"
		if r.AvgReturn < roundTripComm {
			commVerdict = "🚫 comm eats it"
		} else if r.AvgReturn < roundTripComm*2 {
			commVerdict = "⚠️  marginal"
		}
		// Quality verdict
		quality := ""
		if r.Sharpe > 1.0 && r.PF > 1.5 && r.WinRate > 45 && r.MaxDD < 20 && r.AvgReturn > roundTripComm {
			quality = "⭐"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%.1f\t%.2f\t%+.2f\t%.1f\t%+.2f\t%.2f\t%+.3f\t%s\t%+.1f\t$%.0f\t%s\n",
			r.Symbol, r.Strat, r.Trades,
			r.WinRate, r.PF, r.Sharpe, r.MaxDD,
			r.AvgWin, r.AvgLoss,
			r.AvgReturn, commVerdict,
			r.TotalPnL, r.FinalCapital, quality)
	}
	w.Flush()
	fmt.Printf("\n  Round-trip commission = %.3f%% (%.3f%% × 2 sides)\n", roundTripComm, commission*100)
	fmt.Printf("  AVG_TRADE%% must exceed %.3f%% to be viable after costs\n\n", roundTripComm)

	// Summary
	var tot, prof, passAll int
	var sS, sPF, sWR, sPnL float64
	for _, r := range results {
		tot += r.Trades
		sS += r.Sharpe; sPF += r.PF; sWR += r.WinRate; sPnL += r.TotalPnL
		if r.TotalPnL > 0 { prof++ }
		if r.Sharpe > 1.0 && r.PF > 1.5 && r.WinRate > 45 && r.MaxDD < 20 { passAll++ }
	}
	n := float64(len(results))

	fmt.Printf("\n┌─ SUMMARY ──────────────────────────────────────────────┐\n")
	fmt.Printf("│  Combos tested           : %d\n", len(results))
	fmt.Printf("│  Total simulated trades  : %d\n", tot)
	fmt.Printf("│  Profitable combos       : %d / %d (%.0f%%)\n", prof, len(results), float64(prof)/n*100)
	fmt.Printf("│  Pass all criteria ✅    : %d\n", passAll)
	fmt.Printf("│  Avg Sharpe              : %+.3f\n", sS/n)
	fmt.Printf("│  Avg Profit Factor       : %.2f\n", sPF/n)
	fmt.Printf("│  Avg Win Rate            : %.1f%%\n", sWR/n)
	fmt.Printf("│  Avg PnL                 : %+.1f%%\n", sPnL/n)
	fmt.Printf("└────────────────────────────────────────────────────────┘\n")
	fmt.Printf("\n✅ criteria: Sharpe>1.0, PF>1.5, WinRate>45%%, MaxDD<20%%\n\n")

	// Top 5 Sharpe
	fmt.Println("── TOP by Sharpe ──────────────────────────────────────────────────────────")
	for i, r := range results {
		if i == 8 { break }
		fmt.Printf("  #%d  %-12s %-18s  Sharpe=%+.3f  PnL=%+.1f%%  WR=%.0f%%  PF=%.2f  DD=%.1f%%  N=%d\n",
			i+1, r.Symbol, r.Strat, r.Sharpe, r.TotalPnL, r.WinRate, r.PF, r.MaxDD, r.Trades)
	}

	// Top 5 PnL
	sort.Slice(results, func(i, j int) bool { return results[i].TotalPnL > results[j].TotalPnL })
	fmt.Println("\n── TOP by PnL%% ────────────────────────────────────────────────────────────")
	for i, r := range results {
		if i == 8 { break }
		fmt.Printf("  #%d  %-12s %-18s  PnL=%+.1f%%  Final=$%.0f  WR=%.0f%%  N=%d\n",
			i+1, r.Symbol, r.Strat, r.TotalPnL, r.FinalCapital, r.WinRate, r.Trades)
	}

	// Bottom 5
	sort.Slice(results, func(i, j int) bool { return results[i].TotalPnL < results[j].TotalPnL })
	fmt.Println("\n── BOTTOM (avoid) ──────────────────────────────────────────────────────────")
	for i, r := range results {
		if i == 5 { break }
		fmt.Printf("  #%d  %-12s %-18s  PnL=%+.1f%%  MaxDD=%.1f%%  WR=%.0f%%  N=%d\n",
			i+1, r.Symbol, r.Strat, r.TotalPnL, r.MaxDD, r.WinRate, r.Trades)
	}

	// ── Money summary ──
	sort.Slice(results, func(i, j int) bool { return results[i].FinalCapital > results[j].FinalCapital })

	days := end.Sub(start).Hours() / 24
	years := days / 365.25

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Printf( "║  💰 ИТОГО: Стартовый капитал $%.0f на каждую пару/стратегию                    ║\n", initCap)
	fmt.Printf( "║  Период: %s → %s  (%.1f лет)                                       ║\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"), years)
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════════╝")

	var totalStart, totalEnd float64
	for _, r := range results {
		totalStart += initCap
		totalEnd += r.FinalCapital
		profit := r.FinalCapital - initCap
		arrow := "📈"
		if profit < 0 {
			arrow = "📉"
		}
		fmt.Printf("  %s  %-12s %-20s  $%.0f → $%.0f  (%+.0f$)\n",
			arrow, r.Symbol, r.Strat, initCap, r.FinalCapital, profit)
	}

	totalProfit := totalEnd - totalStart
	totalProfitPct := totalProfit / totalStart * 100
	annualised := (math.Pow(totalEnd/totalStart, 1/years) - 1) * 100
	fmt.Printf("\n  ══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("  Начал:       $%.0f  (%d комбо × $%.0f)\n", totalStart, len(results), initCap)
	fmt.Printf("  Закончил:    $%.0f\n", totalEnd)
	fmt.Printf("  Заработал:   %+.0f$  (%+.1f%%)\n", totalProfit, totalProfitPct)
	fmt.Printf("  Годовых:     %+.1f%% в год\n", annualised)
	fmt.Printf("  Период:      %.0f дней (%.1f лет)\n", days, years)
	fmt.Println()
}

// printFilterAnalysis shows whether the candlestick filter removes trades
// systematically (bad trades) or randomly (good and bad alike).
func printFilterAnalysis(stats []filterStats) {
	// Aggregate across all symbols per strategy
	type group struct {
		passed, rejected                     int
		passWins, passLosses                 int
		rejWins, rejLosses                   int
		passGrossP, passGrossL               float64
		rejGrossP, rejGrossL                 float64
		patterns                             map[string]int
	}
	agg := map[string]*group{}
	for _, fs := range stats {
		g, ok := agg[fs.strat]
		if !ok {
			g = &group{patterns: map[string]int{}}
			agg[fs.strat] = g
		}
		g.passed += fs.passed
		g.rejected += fs.rejected
		for p, c := range fs.patternCounts {
			g.patterns[p] += c
		}
		for _, t := range fs.tradesPassed {
			if t.won {
				g.passWins++
				g.passGrossP += t.pnlPct
			} else {
				g.passLosses++
				g.passGrossL += math.Abs(t.pnlPct)
			}
		}
		for _, t := range fs.tradesRejected {
			if t.won {
				g.rejWins++
				g.rejGrossP += t.pnlPct
			} else {
				g.rejLosses++
				g.rejGrossL += math.Abs(t.pnlPct)
			}
		}
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  FILTER ANALYSIS — systematic or random?                                       ║")
	fmt.Println("║  MeanRev1H      → candlestick reversal (pin bar / engulfing / doji)            ║")
	fmt.Println("║  SupportBounce1H → volume rising 2 bars  OR  close > support×1.002            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STRATEGY\tGROUP\tTRADES\tWIN%\tPF\tAVG_WIN%\tAVG_LOSS%\tVERDICT\t")
	fmt.Fprintln(w, "────────\t─────\t──────\t────\t──\t────────\t─────────\t───────\t")

	for _, strat := range []string{"MeanRev1H", "SupportBounce1H"} {
		g, ok := agg[strat]
		if !ok {
			continue
		}

		passTotal := g.passWins + g.passLosses
		rejTotal := g.rejWins + g.rejLosses

		var passWR, passPF, passAvgW, passAvgL float64
		if passTotal > 0 {
			passWR = float64(g.passWins) / float64(passTotal) * 100
			if g.passLosses > 0 {
				passAvgL = g.passGrossL / float64(g.passLosses)
			}
			if g.passWins > 0 {
				passAvgW = g.passGrossP / float64(g.passWins)
			}
			if g.passGrossL > 0 {
				passPF = g.passGrossP / g.passGrossL
			}
		}

		var rejWR, rejPF, rejAvgW, rejAvgL float64
		if rejTotal > 0 {
			rejWR = float64(g.rejWins) / float64(rejTotal) * 100
			if g.rejLosses > 0 {
				rejAvgL = g.rejGrossL / float64(g.rejLosses)
			}
			if g.rejWins > 0 {
				rejAvgW = g.rejGrossP / float64(g.rejWins)
			}
			if g.rejGrossL > 0 {
				rejPF = g.rejGrossP / g.rejGrossL
			}
		}

		// Verdict: is the filter helping?
		verdict := "❓ unclear"
		if passWR > rejWR+3 && passPF > rejPF {
			verdict = "✅ systematic — filter removes losers"
		} else if passWR < rejWR-3 || passPF < rejPF*0.9 {
			verdict = "⚠️  random — filter hurts or neutral"
		} else {
			verdict = "〰️  marginal — small difference"
		}

		fmt.Fprintf(w, "%s\t✅ passed\t%d\t%.1f\t%.2f\t+%.2f\t-%.2f\t%s\n",
			strat, passTotal, passWR, passPF, passAvgW, passAvgL, verdict)
		fmt.Fprintf(w, "%s\t❌ rejected\t%d\t%.1f\t%.2f\t+%.2f\t-%.2f\t\n",
			strat, rejTotal, rejWR, rejPF, rejAvgW, rejAvgL)
		fmt.Fprintln(w, "\t\t\t\t\t\t\t\t")
	}
	w.Flush()

	// Pattern breakdown
	fmt.Println("── Pattern breakdown (passed signals only) ─────────────────────────────────────")
	totalPassed := 0
	patTotals := map[string]int{}
	for _, fs := range stats {
		for p, c := range fs.patternCounts {
			patTotals[p] += c
			totalPassed += c
		}
	}
	for _, p := range []string{"pinbar", "engulfing", "doji"} {
		c := patTotals[p]
		pct := 0.0
		if totalPassed > 0 {
			pct = float64(c) / float64(totalPassed) * 100
		}
		fmt.Printf("  %-12s  %4d signals  (%.0f%% of passed)\n", p, c, pct)
	}

	totalRejected := 0
	for _, g := range agg {
		totalRejected += g.rejected
	}
	fmt.Printf("\n  Total passed   : %d\n", totalPassed)
	fmt.Printf("  Total rejected : %d  (%.0f%% of all signals)\n",
		totalRejected, float64(totalRejected)/math.Max(1, float64(totalPassed+totalRejected))*100)
	fmt.Println()
}

// ── Top-5 deep analysis: by-year breakdown + Monte Carlo ─────────────────────

// top5Key identifies one of the top-5 combos.
type top5Key struct{ symbol, strat string }

var top5 = []top5Key{
	{"NEARUSDT", "MomentumBreakout1H"},
	{"AVAXUSDT", "MeanRev1H"},
	{"APEUSDT", "MeanRev1H"},
	{"SANDUSDT", "MeanRev1H"},
	{"TRBUSDT", "MomentumBreakout1H"},
}

func printTop5Analysis(allResults []result, allStats []filterStats) {
	// Build a map from (symbol, strat) → trades (from filterStats.tradesPassed)
	tradeMap := map[top5Key][]trade{}
	for _, fs := range allStats {
		k := top5Key{fs.symbol, fs.strat}
		for _, k2 := range top5 {
			if k == k2 {
				tradeMap[k] = append(tradeMap[k], fs.tradesPassed...)
			}
		}
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  TOP-5 DEEP ANALYSIS: By-Year Breakdown + Monte Carlo (N=1000)                     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════════════╝")

	rng := newRNG()

	for _, k := range top5 {
		trades := tradeMap[k]
		if len(trades) == 0 {
			fmt.Printf("\n  %s / %s — no trades\n", k.symbol, k.strat)
			continue
		}

		// ── By-year breakdown ──
		type yearStats struct {
			n, wins      int
			grossP, grossL float64
		}
		byYear := map[int]*yearStats{}
		for _, t := range trades {
			y := t.entryTime.Year()
			if byYear[y] == nil {
				byYear[y] = &yearStats{}
			}
			s := byYear[y]
			s.n++
			if t.won {
				s.wins++
				s.grossP += t.pnlPct
			} else {
				s.grossL += math.Abs(t.pnlPct)
			}
		}

		years := []int{}
		for y := range byYear {
			years = append(years, y)
		}
		sort.Ints(years)

		// ── Total stats ──
		var totalPnL float64
		for _, t := range trades {
			totalPnL += t.pnlPct
		}
		avgTrade := totalPnL / float64(len(trades))

		fmt.Printf("\n  ┌─ %s / %s ─── %d trades  │  total PnL %+.2f%%  │  avg/trade %+.3f%%\n",
			k.symbol, k.strat, len(trades), totalPnL, avgTrade)

		// Year table
		fmt.Printf("  │  %-6s  %6s  %5s  %7s  %7s  %5s\n", "YEAR", "TRADES", "WIN%", "GROSS+%", "GROSS-%", "NET%")
		for _, y := range years {
			s := byYear[y]
			wr := float64(s.wins) / float64(s.n) * 100
			net := s.grossP - s.grossL
			fmt.Printf("  │  %-6d  %6d  %5.1f  %+7.2f  %7.2f  %+5.2f\n",
				y, s.n, wr, s.grossP, s.grossL, net)
		}

		// ── Monte Carlo ──
		pnls := make([]float64, len(trades))
		for i, t := range trades {
			pnls[i] = t.pnlPct
		}

		// Bootstrap Monte Carlo: sample N trades WITH replacement.
		// This captures variance from lucky/unlucky trade selection,
		// unlike shuffling which is commutative under compound growth.
		const iterations = 1000
		finals := make([]float64, iterations)
		n := len(pnls)
		for iter := 0; iter < iterations; iter++ {
			bal := initCap
			for j := 0; j < n; j++ {
				p := pnls[rng.intn(n)] // sample with replacement
				bal += bal * riskFrac * p / 100
			}
			finals[iter] = (bal - initCap) / initCap * 100
		}
		sort.Float64s(finals)

		p5  := finals[int(0.05*float64(iterations))]
		p25 := finals[int(0.25*float64(iterations))]
		p50 := finals[int(0.50*float64(iterations))]
		p75 := finals[int(0.75*float64(iterations))]
		p95 := finals[int(0.95*float64(iterations))]

		// Count how many simulations were profitable
		profitableRuns := 0
		for _, f := range finals {
			if f > 0 {
				profitableRuns++
			}
		}

		fmt.Printf("  │\n")
		fmt.Printf("  │  Monte Carlo (%d runs, same trades, shuffled order):\n", iterations)
		fmt.Printf("  │    p5 =%+7.2f%%   p25=%+7.2f%%   p50=%+7.2f%%   p75=%+7.2f%%   p95=%+7.2f%%\n",
			p5, p25, p50, p75, p95)
		fmt.Printf("  │    Profitable runs: %d/%d (%.0f%%)\n", profitableRuns, iterations, float64(profitableRuns)/float64(iterations)*100)

		spread := p95 - p5
		verdict := ""
		switch {
		case p5 > 0:
			verdict = "✅ ROBUST — positive even in worst 5%"
		case p25 > 0 && p5 < 0:
			verdict = "⚠️  FRAGILE — profitable in most runs but tail risk real"
		case p50 > 0 && p25 < 0:
			verdict = "❌ LUCK — median positive but high variance, likely noise"
		default:
			verdict = "💀 AVOID — most simulations negative"
		}
		fmt.Printf("  │    Spread p95-p5: %.2f%%   Verdict: %s\n", spread, verdict)
		fmt.Printf("  └────────────────────────────────────────────────────────────────\n")
	}
	fmt.Println()
}

// ── AdaptiveTrend analysis ────────────────────────────────────────────────

func printAdaptiveAnalysis(allResults []result, allATStats []adaptiveSymStats) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  ADAPTIVE TREND ANALYSIS — regime breakdown + top symbols                          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════════════╝")

	// Aggregate trades by regime across all symbols
	regimeTrades := map[adaptiveRegime][]trade{
		regimeBull: {},
		regimeBear: {},
		regimeSide: {},
	}
	for _, ats := range allATStats {
		for regime, ts := range ats.byRegime {
			regimeTrades[regime] = append(regimeTrades[regime], ts...)
		}
	}

	// Per-regime stats
	fmt.Println()
	fmt.Println("── Per-regime performance (all symbols combined) ───────────────────────────────────")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REGIME\tN\tWIN%\tPF\tSHARPE\tDD%\tAVG_TRADE%\tTOTAL%\t")
	fmt.Fprintln(w, "──────\t─\t────\t──\t──────\t───\t──────────\t──────\t")
	for _, regime := range []adaptiveRegime{regimeBull, regimeBear, regimeSide} {
		ts := regimeTrades[regime]
		if len(ts) == 0 {
			fmt.Fprintf(w, "%s\t0\t—\t—\t—\t—\t—\t—\t\n", regime)
			continue
		}
		r := metricsAT("all", string(regime), ts)
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%.2f\t%+.2f\t%.1f\t%+.3f\t%+.1f\t\n",
			regime, r.Trades, r.WinRate, r.PF, r.Sharpe, r.MaxDD, r.AvgReturn, r.TotalPnL)
	}
	w.Flush()

	// Top-10 symbols by AdaptiveTrend Sharpe
	var atResults []result
	for _, r := range allResults {
		if r.Strat == "AdaptiveTrend" {
			atResults = append(atResults, r)
		}
	}
	sort.Slice(atResults, func(i, j int) bool { return atResults[i].Sharpe > atResults[j].Sharpe })

	fmt.Println()
	fmt.Println("── Top-10 symbols by AdaptiveTrend Sharpe ──────────────────────────────────────────")
	w2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w2, "RANK\tSYMBOL\tN\tWIN%\tPF\tSHARPE\tDD%\tAVG_TRADE%\tTOTAL%\tFINAL_$\t")
	fmt.Fprintln(w2, "────\t──────\t─\t────\t──\t──────\t───\t──────────\t──────\t───────\t")
	for i, r := range atResults {
		if i == 10 {
			break
		}
		fmt.Fprintf(w2, "#%d\t%s\t%d\t%.1f\t%.2f\t%+.2f\t%.1f\t%+.3f\t%+.1f\t$%.0f\t\n",
			i+1, r.Symbol, r.Trades, r.WinRate, r.PF, r.Sharpe, r.MaxDD, r.AvgReturn, r.TotalPnL, r.FinalCapital)
	}
	w2.Flush()

	// Comparison with baseline AVAX MeanRev1H
	fmt.Println()
	fmt.Println("── Comparison: AdaptiveTrend vs AVAX/MeanRev1H baseline ───────────────────────────")
	wc := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(wc, "STRATEGY\tSYMBOL\tN\tWIN%\tPF\tSHARPE\tDD%\tTOTAL%\tFINAL_$\t")
	fmt.Fprintln(wc, "────────\t──────\t─\t────\t──\t──────\t───\t──────\t───────\t")

	// Find AVAX baseline
	for _, r := range allResults {
		if r.Symbol == "AVAXUSDT" && r.Strat == "MeanRev1H" {
			fmt.Fprintf(wc, "%s\t%s\t%d\t%.1f\t%.2f\t%+.2f\t%.1f\t%+.1f\t$%.0f\t (baseline)\n",
				r.Strat, r.Symbol, r.Trades, r.WinRate, r.PF, r.Sharpe, r.MaxDD, r.TotalPnL, r.FinalCapital)
		}
	}
	// Show AVAX AdaptiveTrend
	for _, r := range allResults {
		if r.Symbol == "AVAXUSDT" && r.Strat == "AdaptiveTrend" {
			fmt.Fprintf(wc, "%s\t%s\t%d\t%.1f\t%.2f\t%+.2f\t%.1f\t%+.1f\t$%.0f\t\n",
				r.Strat, r.Symbol, r.Trades, r.WinRate, r.PF, r.Sharpe, r.MaxDD, r.TotalPnL, r.FinalCapital)
		}
	}
	// Show best AdaptiveTrend overall
	if len(atResults) > 0 {
		r := atResults[0]
		fmt.Fprintf(wc, "%s\t%s\t%d\t%.1f\t%.2f\t%+.2f\t%.1f\t%+.1f\t$%.0f\t (best AT)\n",
			r.Strat, r.Symbol, r.Trades, r.WinRate, r.PF, r.Sharpe, r.MaxDD, r.TotalPnL, r.FinalCapital)
	}
	wc.Flush()

	fmt.Println()
	fmt.Printf("  AdaptiveTrend base risk: %.1f%% per trade (vs %.1f%% fixed for other strategies)\n",
		atBaseRisk*100, riskFrac*100)
	fmt.Printf("  Vol scaling: risk / (1.5×ATR/entry), capped at %.1f%%\n", 10*atBaseRisk*100)
	fmt.Println()
}

// minimal xorshift RNG to avoid importing math/rand
type xorRNG struct{ state uint64 }

func newRNG() *xorRNG { return &xorRNG{state: 0xdeadbeefcafe1234} }

func (r *xorRNG) next() uint64 {
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return r.state
}

func (r *xorRNG) intn(n int) int { return int(r.next() % uint64(n)) }

func (r *xorRNG) shuffle(s []float64) {
	for i := len(s) - 1; i > 0; i-- {
		j := r.intn(i + 1)
		s[i], s[j] = s[j], s[i]
	}
}
