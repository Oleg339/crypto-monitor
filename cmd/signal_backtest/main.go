// signal_backtest — evaluates trader#3 signals from signals_trader3.csv.
//
// For each signal:
//   1. Load 1H candles from TimescaleDB (falls back to Bybit REST API).
//   2. Find the first 1H bar AFTER the signal timestamp (no look-ahead bias).
//   3. Wait for price to touch the entry zone [entry_low, entry_high].
//   4. Once filled, track up to 60 days:
//      - SL hit → close remaining position at SL price.
//      - TP1-TP8 hit sequentially → close 1/N of position at each TP.
//      - 60-day deadline reached → mark expired, close at last candle close.
//
// Usage:
//
//	go run ./cmd/signal_backtest/ -csv signals_trader3.csv
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/crypto-monitor/internal/config"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type Signal struct {
	Date      time.Time
	SignalID  int
	Symbol    string
	Direction string // "LONG" | "SHORT"
	EntryLow  float64
	EntryHigh float64
	SL        float64
	TPs       []float64 // non-zero entries from tp1..tp8
	Desc      string
}

type Candle struct {
	Time  time.Time
	Open  float64
	High  float64
	Low   float64
	Close float64
}

// ── Market phase detection ────────────────────────────────────────────────────

type BTCPhase int

const (
	PhaseBull    BTCPhase = iota
	PhaseNeutral
	PhaseBear
)

func (p BTCPhase) String() string {
	switch p {
	case PhaseBull:
		return "Bull"
	case PhaseNeutral:
		return "Neutral"
	case PhaseBear:
		return "Bear"
	}
	return "?"
}

type AltPhase int

const (
	AltStrong  AltPhase = iota
	AltNeutral
	AltWeak
)

func (p AltPhase) String() string {
	switch p {
	case AltStrong:
		return "Strong"
	case AltNeutral:
		return "Neutral"
	case AltWeak:
		return "Weak"
	}
	return "?"
}

// MA returns the simple moving average of the last `period` closes.
func MA(candles []Candle, period int) float64 {
	if len(candles) < period {
		return 0
	}
	sum := 0.0
	for _, c := range candles[len(candles)-period:] {
		sum += c.Close
	}
	return sum / float64(period)
}

// candlesBefore returns candles with Time <= t (no look-ahead).
func candlesBefore(candles []Candle, t time.Time) []Candle {
	i := sort.Search(len(candles), func(j int) bool {
		return candles[j].Time.After(t)
	})
	return candles[:i]
}

func detectBTCPhase(weeklyCandles, dailyCandles []Candle) BTCPhase {
	if len(weeklyCandles) < 200 || len(dailyCandles) < 55 {
		return PhaseNeutral
	}
	price := dailyCandles[len(dailyCandles)-1].Close
	ma200w := MA(weeklyCandles, 200)
	ma50d := MA(dailyCandles, 50)
	ma50dPrev := MA(dailyCandles[:len(dailyCandles)-5], 50)

	if price > ma200w && ma50d > ma50dPrev {
		return PhaseBull
	}
	if price < ma200w {
		return PhaseBear
	}
	return PhaseNeutral
}

func detectAltPhase(altCandles, btcDailyCandles []Candle) AltPhase {
	if len(altCandles) < 14 || len(btcDailyCandles) < 14 {
		return AltNeutral
	}
	altReturn := altCandles[len(altCandles)-1].Close/altCandles[len(altCandles)-14].Close - 1
	btcReturn := btcDailyCandles[len(btcDailyCandles)-1].Close/btcDailyCandles[len(btcDailyCandles)-14].Close - 1
	rs := altReturn - btcReturn
	if rs > 0.05 {
		return AltStrong
	}
	if rs < -0.05 {
		return AltWeak
	}
	return AltNeutral
}

func getDynLeverage(btc BTCPhase, alt AltPhase) float64 {
	matrix := [3][3]float64{
		{4.0, 3.0, 2.0}, // Bull:    Strong / Neutral / Weak
		{3.0, 2.0, 1.0}, // Neutral: Strong / Neutral / Weak
		{1.0, 0.0, 0.0}, // Bear:    Strong / Neutral / Weak
	}
	return matrix[btc][alt]
}

type Result struct {
	Signal      Signal
	EntryFilled bool
	EntryDate   time.Time
	EntryPrice  float64
	HitSL       bool
	Expired     bool
	MaxTPHit    int     // 0 = none hit
	PnLPct      float64 // realised PnL % on total position
	DaysToFill  float64
	DaysToClose float64
	CloseTime   time.Time // zero if not filled or still open
	BTCPhase    BTCPhase
	AltPhase    AltPhase
	DynLeverage float64 // 0 = skipped
	Skipped     bool    // true when DynLeverage == 0
}

// ── CSV parsing ───────────────────────────────────────────────────────────────

func parseSignals(path string) ([]Signal, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}

	get := func(row []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	getF := func(row []string, name string) float64 {
		v, _ := strconv.ParseFloat(get(row, name), 64)
		return v
	}

	var out []Signal
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		dt, err := time.ParseInLocation("2006-01-02 15:04",
			get(row, "date")+" "+get(row, "time"), time.UTC)
		if err != nil {
			log.Printf("skip: bad date in row %v: %v", row, err)
			continue
		}

		id, _ := strconv.Atoi(get(row, "signal_id"))

		tps := make([]float64, 0, 8)
		for i := 1; i <= 8; i++ {
			v := getF(row, fmt.Sprintf("tp%d", i))
			if v > 0 {
				tps = append(tps, v)
			}
		}

		out = append(out, Signal{
			Date:      dt,
			SignalID:  id,
			Symbol:    get(row, "symbol"),
			Direction: strings.ToUpper(get(row, "direction")),
			EntryLow:  getF(row, "entry_low"),
			EntryHigh: getF(row, "entry_high"),
			SL:        getF(row, "sl"),
			TPs:       tps,
			Desc:      get(row, "description"),
		})
	}
	return out, nil
}

// ── DB candle loading ─────────────────────────────────────────────────────────

func dbCandles(ctx context.Context, db *pgxpool.Pool, symbol string, from, to time.Time) ([]Candle, error) {
	rows, err := db.Query(ctx, `
		SELECT time, open, high, low, close
		FROM candles
		WHERE symbol=$1 AND interval='60' AND time>=$2 AND time<=$3
		ORDER BY time ASC`,
		symbol, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candle
	for rows.Next() {
		var c Candle
		if err := rows.Scan(&c.Time, &c.Open, &c.High, &c.Low, &c.Close); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── Bybit REST fetch ──────────────────────────────────────────────────────────

type bybitResp struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		List [][]string `json:"list"` // [startMs, open, high, low, close, vol, turnover]
	} `json:"result"`
}

func bybitFetch(symbol, interval string, from, to time.Time) ([]Candle, error) {
	const endpoint = "https://api.bybit.com/v5/market/kline"
	client := &http.Client{Timeout: 15 * time.Second}

	var all []Candle
	cur := to

	for {
		p := url.Values{}
		p.Set("category", "linear")
		p.Set("symbol", symbol)
		p.Set("interval", interval)
		p.Set("limit", "200")
		p.Set("start", strconv.FormatInt(from.UnixMilli(), 10))
		p.Set("end", strconv.FormatInt(cur.UnixMilli(), 10))

		resp, err := client.Get(endpoint + "?" + p.Encode())
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var parsed bybitResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		if parsed.RetCode != 0 {
			return nil, fmt.Errorf("bybit: %s", parsed.RetMsg)
		}
		if len(parsed.Result.List) == 0 {
			break
		}

		for _, item := range parsed.Result.List {
			if len(item) < 5 {
				continue
			}
			ms, _ := strconv.ParseInt(item[0], 10, 64)
			t := time.UnixMilli(ms).UTC()
			if t.Before(from) {
				continue
			}
			o, _ := strconv.ParseFloat(item[1], 64)
			h, _ := strconv.ParseFloat(item[2], 64)
			l, _ := strconv.ParseFloat(item[3], 64)
			c, _ := strconv.ParseFloat(item[4], 64)
			all = append(all, Candle{Time: t, Open: o, High: h, Low: l, Close: c})
		}

		last := parsed.Result.List[len(parsed.Result.List)-1]
		ms, _ := strconv.ParseInt(last[0], 10, 64)
		cur = time.UnixMilli(ms).UTC()
		if len(parsed.Result.List) < 200 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	seen := map[int64]bool{}
	deduped := all[:0]
	for _, c := range all {
		k := c.Time.UnixMilli()
		if !seen[k] {
			seen[k] = true
			deduped = append(deduped, c)
		}
	}
	sort.Slice(deduped, func(i, j int) bool { return deduped[i].Time.Before(deduped[j].Time) })
	return deduped, nil
}

func bybitCandles(symbol string, from, to time.Time) ([]Candle, error) {
	return bybitFetch(symbol, "60", from, to)
}

func bybitDailyCandles(symbol string, from, to time.Time) ([]Candle, error) {
	return bybitFetch(symbol, "D", from, to)
}

func bybitWeeklyCandles(symbol string, from, to time.Time) ([]Candle, error) {
	return bybitFetch(symbol, "W", from, to)
}

// ── Simulation ────────────────────────────────────────────────────────────────

const maxHoldDays = 60

func simulate(sig Signal, candles []Candle) Result {
	res := Result{Signal: sig}
	if len(candles) == 0 || len(sig.TPs) == 0 {
		return res
	}

	// First bar strictly after the signal (no look-ahead bias)
	startIdx := -1
	for i, c := range candles {
		if c.Time.After(sig.Date) {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return res
	}

	entryMid := (sig.EntryLow + sig.EntryHigh) / 2.0
	deadline := sig.Date.Add(maxHoldDays * 24 * time.Hour)
	n := len(sig.TPs)
	frac := 1.0 / float64(n) // fraction closed at each TP

	// Phase 1 — find entry
	entryIdx := -1
	for i := startIdx; i < len(candles); i++ {
		c := candles[i]
		if c.Time.After(deadline) {
			return res // never filled
		}
		if c.Low <= sig.EntryHigh && c.High >= sig.EntryLow {
			entryIdx = i
			res.EntryFilled = true
			res.EntryDate = c.Time
			res.EntryPrice = entryMid
			res.DaysToFill = c.Time.Sub(sig.Date).Hours() / 24
			break
		}
	}
	if !res.EntryFilled {
		return res
	}

	// Phase 2 — track from next bar
	tpHit := make([]bool, n)
	remaining := 1.0
	pnl := 0.0

	sign := 1.0
	if sig.Direction == "SHORT" {
		sign = -1.0
	}

	for i := entryIdx + 1; i < len(candles) && remaining > 1e-9; i++ {
		c := candles[i]

		if c.Time.After(deadline) {
			// Expire — close remaining at candle close
			res.Expired = true
			pnl += sign * (c.Close-entryMid)/entryMid*100 * remaining
			res.DaysToClose = c.Time.Sub(sig.Date).Hours() / 24
			res.CloseTime = c.Time
			break
		}

		// Check SL
		slTriggered := (sig.Direction == "LONG" && c.Low <= sig.SL) ||
			(sig.Direction == "SHORT" && c.High >= sig.SL)
		if slTriggered {
			res.HitSL = true
			pnl += sign * (sig.SL-entryMid)/entryMid*100 * remaining
			res.DaysToClose = c.Time.Sub(sig.Date).Hours() / 24
			res.CloseTime = c.Time
			break
		}

		// Check TPs in order
		for j := 0; j < n; j++ {
			if tpHit[j] {
				continue
			}
			tp := sig.TPs[j]
			hit := (sig.Direction == "LONG" && c.High >= tp) ||
				(sig.Direction == "SHORT" && c.Low <= tp)
			if hit {
				tpHit[j] = true
				if j+1 > res.MaxTPHit {
					res.MaxTPHit = j + 1
					res.DaysToClose = c.Time.Sub(sig.Date).Hours() / 24
					res.CloseTime = c.Time
				}
				pnl += sign * (tp-entryMid)/entryMid*100 * frac
				remaining = math.Max(remaining-frac, 0)
			}
		}
	}

	res.PnLPct = pnl
	return res
}

// ── ExitPartial33 simulation ──────────────────────────────────────────────────

// ExitPartial33Result holds the result for the 33/33/33 partial-exit strategy.
type ExitPartial33Result struct {
	Signal      Signal
	EntryFilled bool
	EntryPrice  float64
	PnLPct      float64  // realised PnL % on total position
	TPs         int      // number of TPs hit (0–3)
	HitSL       bool
	Expired     bool
	CloseTime   time.Time
}

// simulatePartial33 runs the partial-exit strategy:
//   - 1/3 closed at TP1, 1/3 at TP2, 1/3 at TP3
//   - SL applied to remaining open portion
//   - Requires signal to have at least 3 TPs; otherwise returns EntryFilled=false.
func simulatePartial33(sig Signal, candles []Candle) ExitPartial33Result {
	res := ExitPartial33Result{Signal: sig}
	if len(sig.TPs) < 3 || len(candles) == 0 {
		return res
	}

	// First bar strictly after the signal
	startIdx := -1
	for i, c := range candles {
		if c.Time.After(sig.Date) {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return res
	}

	deadline := sig.Date.Add(maxHoldDays * 24 * time.Hour)

	// Phase 1 — find entry
	entryIdx := -1
	for i := startIdx; i < len(candles); i++ {
		c := candles[i]
		if c.Time.After(deadline) {
			return res
		}
		if c.Low <= sig.EntryHigh && c.High >= sig.EntryLow {
			entryIdx = i
			res.EntryFilled = true
			res.EntryPrice = (sig.EntryLow + sig.EntryHigh) / 2.0
			break
		}
	}
	if !res.EntryFilled {
		return res
	}

	sign := 1.0
	if sig.Direction == "SHORT" {
		sign = -1.0
	}

	const frac = 1.0 / 3.0
	tpPrices := sig.TPs[:3] // only first 3
	tpHit := [3]bool{}
	remaining := 1.0
	pnl := 0.0

	// Phase 2 — track from next bar
	for i := entryIdx + 1; i < len(candles) && remaining > 1e-9; i++ {
		c := candles[i]

		if c.Time.After(deadline) {
			res.Expired = true
			pnl += sign * (c.Close-res.EntryPrice)/res.EntryPrice*100 * remaining
			res.CloseTime = c.Time
			break
		}

		// SL checked first
		slTriggered := (sig.Direction == "LONG" && c.Low <= sig.SL) ||
			(sig.Direction == "SHORT" && c.High >= sig.SL)
		if slTriggered {
			res.HitSL = true
			pnl += sign * (sig.SL-res.EntryPrice)/res.EntryPrice*100 * remaining
			res.CloseTime = c.Time
			break
		}

		// TPs in order (TP1 → TP2 → TP3)
		for j := 0; j < 3; j++ {
			if tpHit[j] {
				continue
			}
			tp := tpPrices[j]
			hit := (sig.Direction == "LONG" && c.High >= tp) ||
				(sig.Direction == "SHORT" && c.Low <= tp)
			if hit {
				tpHit[j] = true
				res.TPs = j + 1
				pnl += sign * (tp-res.EntryPrice)/res.EntryPrice*100 * frac
				remaining = math.Max(remaining-frac, 0)
				if j == 2 {
					res.CloseTime = c.Time
				}
			}
		}
	}

	res.PnLPct = pnl
	return res
}

// ── ExitPartialBreakeven simulation ──────────────────────────────────────────

// PartialBEResult holds the result for the Breakeven partial-exit strategy.
type PartialBEResult struct {
	Signal      Signal
	EntryFilled bool
	EntryPrice  float64
	PnLPct      float64
	TPs         int  // 0–3
	HitSL       bool // original SL (before TP1)
	HitBE       bool // breakeven stop triggered (after TP1, before TP2/TP3)
	Expired     bool
	CloseTime   time.Time
}

// simulateBreakeven runs:
//   - 1/3 closed at TP1 → SL moves to entry (breakeven)
//   - 1/3 closed at TP2
//   - 1/3 (actually 34%) closed at TP3
//   - If price hits breakeven after TP1 → close remainder at entry (0% on that portion)
func simulateBreakeven(sig Signal, candles []Candle) PartialBEResult {
	res := PartialBEResult{Signal: sig}
	if len(sig.TPs) < 3 || len(candles) == 0 {
		return res
	}

	startIdx := -1
	for i, c := range candles {
		if c.Time.After(sig.Date) {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return res
	}

	deadline := sig.Date.Add(maxHoldDays * 24 * time.Hour)

	// Phase 1 — find entry
	entryIdx := -1
	for i := startIdx; i < len(candles); i++ {
		c := candles[i]
		if c.Time.After(deadline) {
			return res
		}
		if c.Low <= sig.EntryHigh && c.High >= sig.EntryLow {
			entryIdx = i
			res.EntryFilled = true
			res.EntryPrice = (sig.EntryLow + sig.EntryHigh) / 2.0
			break
		}
	}
	if !res.EntryFilled {
		return res
	}

	sign := 1.0
	if sig.Direction == "SHORT" {
		sign = -1.0
	}

	tpPrices := sig.TPs[:3]
	tpHit := [3]bool{}
	remaining := 1.0
	pnl := 0.0
	activeSL := sig.SL     // starts as original SL; moves to entry after TP1
	beActive := false       // true after TP1 is hit

	for i := entryIdx + 1; i < len(candles) && remaining > 1e-9; i++ {
		c := candles[i]

		if c.Time.After(deadline) {
			res.Expired = true
			pnl += sign * (c.Close-res.EntryPrice)/res.EntryPrice*100 * remaining
			res.CloseTime = c.Time
			break
		}

		// SL / BE check
		slTriggered := (sig.Direction == "LONG" && c.Low <= activeSL) ||
			(sig.Direction == "SHORT" && c.High >= activeSL)
		if slTriggered {
			closePrice := activeSL
			pnl += sign * (closePrice-res.EntryPrice)/res.EntryPrice*100 * remaining
			res.CloseTime = c.Time
			if beActive {
				res.HitBE = true
			} else {
				res.HitSL = true
			}
			break
		}

		// TPs in order
		for j := 0; j < 3; j++ {
			if tpHit[j] {
				continue
			}
			tp := tpPrices[j]
			hit := (sig.Direction == "LONG" && c.High >= tp) ||
				(sig.Direction == "SHORT" && c.Low <= tp)
			if !hit {
				continue
			}
			tpHit[j] = true
			res.TPs = j + 1

			frac := 1.0 / 3.0
			pnl += sign * (tp-res.EntryPrice)/res.EntryPrice*100 * frac
			remaining = math.Max(remaining-frac, 0)

			// After TP1: move SL to breakeven
			if j == 0 {
				activeSL = res.EntryPrice
				beActive = true
			}
			if j == 2 {
				res.CloseTime = c.Time
			}
		}
	}

	res.PnLPct = pnl
	return res
}

// ── Exit strategy comparison ──────────────────────────────────────────────────

func compareExitStrategies(results []Result, partial33 []ExitPartial33Result, breakeven []PartialBEResult, startCapital float64) {
	type stratStats struct {
		eligible int
		wins     int
		losses   int
		sumPnL   float64
		final    float64
		maxDD    float64
	}

	// ── ExitTP3: 100% at TP3 ──────────────────────────────────────────────────
	var tp3 stratStats
	tp3Events := []struct {
		t     time.Time
		open  bool
		idx   int
		pnlPct float64
	}{}

	for i, r := range results {
		if !r.EntryFilled || len(r.Signal.TPs) < 3 {
			continue
		}
		tp3.eligible++
		sign := 1.0
		if r.Signal.Direction == "SHORT" {
			sign = -1.0
		}
		var pnl float64
		if r.MaxTPHit >= 3 {
			tp3.wins++
			pnl = sign * (r.Signal.TPs[2] - r.EntryPrice) / r.EntryPrice * 100
		} else if r.HitSL {
			tp3.losses++
			pnl = sign * (r.Signal.SL - r.EntryPrice) / r.EntryPrice * 100
		} else {
			// expired before TP3 — breakeven
			pnl = 0
		}
		tp3.sumPnL += pnl
		if !r.CloseTime.IsZero() {
			tp3Events = append(tp3Events, struct {
				t     time.Time
				open  bool
				idx   int
				pnlPct float64
			}{r.EntryDate, true, i, 0})
			tp3Events = append(tp3Events, struct {
				t     time.Time
				open  bool
				idx   int
				pnlPct float64
			}{r.CloseTime, false, i, pnl})
		}
	}
	sort.Slice(tp3Events, func(i, j int) bool {
		if tp3Events[i].t.Equal(tp3Events[j].t) {
			return !tp3Events[i].open && tp3Events[j].open
		}
		return tp3Events[i].t.Before(tp3Events[j].t)
	})

	// portfolio for tp3
	{
		bal := startCapital
		peak := startCapital
		alloc := map[int]float64{}
		for _, e := range tp3Events {
			if e.open {
				alloc[e.idx] = bal * 0.10
			} else {
				raw := alloc[e.idx] * 3.0 * e.pnlPct / 100
				gain := math.Max(raw, -alloc[e.idx])
				bal = math.Max(bal+gain, 0)
				if bal > peak {
					peak = bal
				}
				if peak > 0 {
					dd := (peak - bal) / peak * 100
					if dd > tp3.maxDD {
						tp3.maxDD = dd
					}
				}
			}
		}
		tp3.final = bal
	}

	// ── ExitPartial33 ─────────────────────────────────────────────────────────
	var p33 stratStats
	type p33ev struct {
		t      time.Time
		open   bool
		idx    int
		pnlPct float64
	}
	var p33Events []p33ev

	for i, r := range partial33 {
		if !r.EntryFilled {
			continue
		}
		p33.eligible++
		if r.PnLPct > 0 {
			p33.wins++
		} else {
			p33.losses++
		}
		p33.sumPnL += r.PnLPct
		if !r.CloseTime.IsZero() {
			p33Events = append(p33Events, p33ev{r.Signal.Date, true, i, 0})
			p33Events = append(p33Events, p33ev{r.CloseTime, false, i, r.PnLPct})
		}
	}
	sort.Slice(p33Events, func(i, j int) bool {
		if p33Events[i].t.Equal(p33Events[j].t) {
			return !p33Events[i].open && p33Events[j].open
		}
		return p33Events[i].t.Before(p33Events[j].t)
	})

	// portfolio for partial33
	{
		bal := startCapital
		peak := startCapital
		alloc := map[int]float64{}
		for _, e := range p33Events {
			if e.open {
				alloc[e.idx] = bal * 0.10
			} else {
				raw := alloc[e.idx] * 3.0 * e.pnlPct / 100
				gain := math.Max(raw, -alloc[e.idx])
				bal = math.Max(bal+gain, 0)
				if bal > peak {
					peak = bal
				}
				if peak > 0 {
					dd := (peak - bal) / peak * 100
					if dd > p33.maxDD {
						p33.maxDD = dd
					}
				}
			}
		}
		p33.final = bal
	}

	// ── ExitPartialBreakeven ──────────────────────────────────────────────────
	var pbe stratStats
	type pbeev struct {
		t      time.Time
		open   bool
		idx    int
		pnlPct float64
	}
	var pbeEvents []pbeev

	for i, r := range breakeven {
		if !r.EntryFilled {
			continue
		}
		pbe.eligible++
		if r.PnLPct > 0 {
			pbe.wins++
		} else {
			pbe.losses++
		}
		pbe.sumPnL += r.PnLPct
		if !r.CloseTime.IsZero() {
			pbeEvents = append(pbeEvents, pbeev{r.Signal.Date, true, i, 0})
			pbeEvents = append(pbeEvents, pbeev{r.CloseTime, false, i, r.PnLPct})
		}
	}
	sort.Slice(pbeEvents, func(i, j int) bool {
		if pbeEvents[i].t.Equal(pbeEvents[j].t) {
			return !pbeEvents[i].open && pbeEvents[j].open
		}
		return pbeEvents[i].t.Before(pbeEvents[j].t)
	})
	{
		bal := startCapital
		peak := startCapital
		alloc := map[int]float64{}
		for _, e := range pbeEvents {
			if e.open {
				alloc[e.idx] = bal * 0.10
			} else {
				raw := alloc[e.idx] * 3.0 * e.pnlPct / 100
				gain := math.Max(raw, -alloc[e.idx])
				bal = math.Max(bal+gain, 0)
				if bal > peak {
					peak = bal
				}
				if peak > 0 {
					dd := (peak - bal) / peak * 100
					if dd > pbe.maxDD {
						pbe.maxDD = dd
					}
				}
			}
		}
		pbe.final = bal
	}

	// ── Print comparison ──────────────────────────────────────────────────────
	wr := func(s stratStats) float64 {
		d := s.wins + s.losses
		if d == 0 {
			return 0
		}
		return float64(s.wins) / float64(d) * 100
	}
	avg := func(s stratStats) float64 {
		if s.eligible == 0 {
			return 0
		}
		return s.sumPnL / float64(s.eligible)
	}
	ret := func(f float64) float64 { return (f - startCapital) / startCapital * 100 }

	sep := strings.Repeat("═", 88)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  EXIT STRATEGY COMPARISON  (start $%.0f | 10%% per trade | 3x leverage)\n", startCapital)
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-22s  %8s  %5s  %5s  %7s  %8s  %8s  %7s  %7s\n",
		"Strategy", "Eligible", "Wins", "Loss", "WR%", "AvgPnL%", "Final$", "Return%", "MaxDD%")
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-22s  %8d  %5d  %5d  %6.1f%%  %+7.2f%%  %8.2f  %+6.1f%%  %6.1f%%\n",
		"ExitTP3 (100%@TP3)",
		tp3.eligible, tp3.wins, tp3.losses, wr(tp3), avg(tp3), tp3.final, ret(tp3.final), tp3.maxDD)
	fmt.Printf("  %-22s  %8d  %5d  %5d  %6.1f%%  %+7.2f%%  %8.2f  %+6.1f%%  %6.1f%%\n",
		"ExitPartial33",
		p33.eligible, p33.wins, p33.losses, wr(p33), avg(p33), p33.final, ret(p33.final), p33.maxDD)
	fmt.Printf("  %-22s  %8d  %5d  %5d  %6.1f%%  %+7.2f%%  %8.2f  %+6.1f%%  %6.1f%%\n",
		"ExitPartialBreakeven",
		pbe.eligible, pbe.wins, pbe.losses, wr(pbe), avg(pbe), pbe.final, ret(pbe.final), pbe.maxDD)
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-22s  %8s  %5s  %5s  %6.1f%%  %+7.2f%%  %+8.2f  %+6.1f%%\n",
		"P33 - TP3", "", "", "", wr(p33)-wr(tp3), avg(p33)-avg(tp3), p33.final-tp3.final, ret(p33.final)-ret(tp3.final))
	fmt.Printf("  %-22s  %8s  %5s  %5s  %6.1f%%  %+7.2f%%  %+8.2f  %+6.1f%%\n",
		"BE  - TP3", "", "", "", wr(pbe)-wr(tp3), avg(pbe)-avg(tp3), pbe.final-tp3.final, ret(pbe.final)-ret(tp3.final))
	fmt.Printf("  %-22s  %8s  %5s  %5s  %6.1f%%  %+7.2f%%  %+8.2f  %+6.1f%%\n",
		"BE  - P33", "", "", "", wr(pbe)-wr(p33), avg(pbe)-avg(p33), pbe.final-p33.final, ret(pbe.final)-ret(p33.final))
	fmt.Printf("%s\n", sep)

	// ── Per-signal detail for all three strategies ────────────────────────────
	outcomeP33 := func(r ExitPartial33Result) string {
		if !r.EntryFilled {
			return "not filled"
		}
		if r.HitSL && r.TPs == 0 {
			return "SL only"
		}
		if r.HitSL {
			return fmt.Sprintf("TP%d→SL", r.TPs)
		}
		if r.Expired {
			return fmt.Sprintf("TP%d→exp", r.TPs)
		}
		return fmt.Sprintf("TP%d full", r.TPs)
	}
	outcomeBE := func(r PartialBEResult) string {
		if !r.EntryFilled {
			return "not filled"
		}
		if r.HitSL && r.TPs == 0 {
			return "SL only"
		}
		if r.HitBE {
			return fmt.Sprintf("TP%d→BE", r.TPs)
		}
		if r.Expired {
			return fmt.Sprintf("TP%d→exp", r.TPs)
		}
		return fmt.Sprintf("TP%d full", r.TPs)
	}

	fmt.Printf("\n  Per-signal detail (filled trades only):\n")
	fmt.Printf("  %-6s %-12s  %9s  %12s  %14s\n",
		"ID", "Symbol", "ExitTP3%", "ExitP33%", "ExitBE%")
	fmt.Printf("  %s\n", strings.Repeat("─", 60))

	for i, r := range results {
		if !r.EntryFilled || len(r.Signal.TPs) < 3 {
			continue
		}
		sign := 1.0
		if r.Signal.Direction == "SHORT" {
			sign = -1.0
		}
		var tp3pnl float64
		if r.MaxTPHit >= 3 {
			tp3pnl = sign * (r.Signal.TPs[2] - r.EntryPrice) / r.EntryPrice * 100
		} else if r.HitSL {
			tp3pnl = sign * (r.Signal.SL - r.EntryPrice) / r.EntryPrice * 100
		}

		p33r := partial33[i]
		ber := breakeven[i]

		fmt.Printf("  %-6d %-12s  %+7.2f%%     %+7.2f%% %-12s  %+7.2f%% %s\n",
			r.Signal.SignalID, r.Signal.Symbol,
			tp3pnl,
			p33r.PnLPct, "("+outcomeP33(p33r)+")",
			ber.PnLPct, "("+outcomeBE(ber)+")")
	}
	fmt.Printf("%s\n", sep)
}

// ── Stats printing ────────────────────────────────────────────────────────────

func printStats(results []Result) {
	total := len(results)
	var filled, wins, losses, expired int
	tpDist := make([]int, 9)
	var sumPnLAll, sumPnLFilled, sumDays float64
	var daysN int

	type mk struct{ y, m int }
	type ms struct{ sigs, filled, wins int; pnl float64 }
	monthly := map[mk]*ms{}

	best := Result{PnLPct: -math.MaxFloat64}
	worst := Result{PnLPct: math.MaxFloat64}

	for _, r := range results {
		k := mk{r.Signal.Date.Year(), int(r.Signal.Date.Month())}
		if monthly[k] == nil {
			monthly[k] = &ms{}
		}
		monthly[k].sigs++
		sumPnLAll += r.PnLPct

		if !r.EntryFilled {
			continue
		}
		filled++
		monthly[k].filled++
		sumPnLFilled += r.PnLPct

		if r.HitSL {
			losses++
		} else if r.PnLPct > 0 {
			wins++
			monthly[k].wins++
		} else {
			losses++
		}
		if r.Expired {
			expired++
		}
		tpDist[r.MaxTPHit]++

		if r.DaysToClose > 0 {
			sumDays += r.DaysToClose
			daysN++
		}
		monthly[k].pnl += r.PnLPct

		if r.PnLPct > best.PnLPct {
			best = r
		}
		if r.PnLPct < worst.PnLPct {
			worst = r
		}
	}

	decided := wins + losses
	wr := 0.0
	if decided > 0 {
		wr = float64(wins) / float64(decided) * 100
	}
	avgAll := pct2(sumPnLAll, total)
	avgFilled := pct2(sumPnLFilled, filled)
	avgDays := 0.0
	if daysN > 0 {
		avgDays = sumDays / float64(daysN)
	}

	sep := strings.Repeat("═", 56)
	fmt.Println(sep)
	fmt.Println("        BACKTEST RESULTS — trader#3 signals")
	fmt.Println(sep)
	fmt.Printf("  Total signals        : %d\n", total)
	fmt.Printf("  Entry filled         : %d (%.0f%%)\n", filled, pct2(float64(filled)*100, total))
	fmt.Printf("  Entry NOT filled     : %d\n", total-filled)
	fmt.Printf("  Wins                 : %d\n", wins)
	fmt.Printf("  Losses               : %d\n", losses)
	fmt.Printf("  Expired (>60d)       : %d\n", expired)
	fmt.Printf("  Win Rate             : %.1f%%\n", wr)
	fmt.Printf("  Avg PnL (all sigs)   : %+.2f%%\n", avgAll)
	fmt.Printf("  Avg PnL (filled)     : %+.2f%%\n", avgFilled)
	fmt.Printf("  Avg days to close    : %.1f\n", avgDays)

	fmt.Printf("\n── TP distribution %s\n", strings.Repeat("─", 36))
	for i, cnt := range tpDist {
		if cnt == 0 {
			continue
		}
		label := fmt.Sprintf("TP%d", i)
		if i == 0 {
			label = "no TP"
		}
		bar := strings.Repeat("█", cnt)
		fmt.Printf("  %-6s : %2d  %s\n", label, cnt, bar)
	}

	fmt.Printf("\n── Best / Worst %s\n", strings.Repeat("─", 40))
	if best.Signal.SignalID > 0 {
		fmt.Printf("  Best : #%d %s %s  PnL=%+.2f%%  maxTP=%d\n",
			best.Signal.SignalID, best.Signal.Symbol,
			best.Signal.Date.Format("2006-01-02"), best.PnLPct, best.MaxTPHit)
	}
	if worst.Signal.SignalID > 0 {
		fmt.Printf("  Worst: #%d %s %s  PnL=%+.2f%%  maxTP=%d\n",
			worst.Signal.SignalID, worst.Signal.Symbol,
			worst.Signal.Date.Format("2006-01-02"), worst.PnLPct, worst.MaxTPHit)
	}

	fmt.Printf("\n── Monthly %s\n", strings.Repeat("─", 45))
	keys := make([]mk, 0, len(monthly))
	for k := range monthly {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].y != keys[j].y {
			return keys[i].y < keys[j].y
		}
		return keys[i].m < keys[j].m
	})
	for _, k := range keys {
		m := monthly[k]
		w := 0.0
		if m.filled > 0 {
			w = float64(m.wins) / float64(m.filled) * 100
		}
		fmt.Printf("  %d-%02d  sigs=%-3d filled=%-3d wins=%-3d WR=%-5.0f%% PnL=%+.2f%%\n",
			k.y, k.m, m.sigs, m.filled, m.wins, w, m.pnl)
	}

	fmt.Printf("\n── Per-signal detail %s\n", strings.Repeat("─", 35))
	fmt.Printf("  %-6s %-12s %-5s %-5s %-5s %-5s %8s  %-10s\n",
		"ID", "Symbol", "Dir", "Fill", "SL", "maxTP", "PnL%", "EntryDate")
	for _, r := range results {
		fillS := "no"
		if r.EntryFilled {
			fillS = "YES"
		}
		slS := "-"
		if r.HitSL {
			slS = "YES"
		}
		ed := ""
		if r.EntryFilled {
			ed = r.EntryDate.Format("2006-01-02")
		}
		fmt.Printf("  %-6d %-12s %-5s %-5s %-5s %-5d %+8.2f%%  %s\n",
			r.Signal.SignalID, r.Signal.Symbol, r.Signal.Direction,
			fillS, slS, r.MaxTPHit, r.PnLPct, ed)
	}
}

// ── TP exit strategy analysis ────────────────────────────────────────────────

// analyzeTPStrategies shows what happens if you always exit 100% of position at TPn.
// Uses existing simulation results: MaxTPHit >= n → win, SL before TPn → loss.
func analyzeTPStrategies(results []Result) {
	sep := strings.Repeat("─", 72)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  TP EXIT STRATEGY — what if you always closed 100%% at TPn?\n")
	fmt.Printf("  (signals without TPn are excluded from that row)\n")
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-10s %8s %5s %5s %6s  %7s  %7s  %7s\n",
		"Strategy", "Eligible", "Wins", "Loss", "WR%", "AvgWin%", "AvgLoss%", "AvgPnL%")
	fmt.Printf("%s\n", sep)

	for n := 1; n <= 8; n++ {
		var eligible, wins, losses, expired int
		var sumWin, sumLoss, sumAll float64

		for _, r := range results {
			if !r.EntryFilled {
				continue
			}
			if len(r.Signal.TPs) < n {
				continue // signal has fewer than n TPs
			}
			eligible++

			sign := 1.0
			if r.Signal.Direction == "SHORT" {
				sign = -1.0
			}

			if r.MaxTPHit >= n {
				wins++
				pnl := sign * (r.Signal.TPs[n-1] - r.EntryPrice) / r.EntryPrice * 100
				sumWin += pnl
				sumAll += pnl
			} else if r.HitSL {
				losses++
				pnl := sign * (r.Signal.SL - r.EntryPrice) / r.EntryPrice * 100
				sumLoss += pnl
				sumAll += pnl
			} else {
				// expired without hitting TPn — count as loss at 0 (breakeven close)
				expired++
				sumAll += 0
			}
		}

		decided := wins + losses
		wr := 0.0
		if decided > 0 {
			wr = float64(wins) / float64(decided) * 100
		}
		avgWin := safeDiv(sumWin, float64(wins))
		avgLoss := safeDiv(sumLoss, float64(losses))
		avgAll := safeDiv(sumAll, float64(eligible))

		expStr := ""
		if expired > 0 {
			expStr = fmt.Sprintf(" +%dexp", expired)
		}
		fmt.Printf("  Exit@TP%-3d %8d %5d %4d%-6s %5.1f%%  %+6.2f%%  %+6.2f%%  %+6.2f%%\n",
			n, eligible, wins, losses, expStr, wr, avgWin, avgLoss, avgAll)
	}
	fmt.Printf("%s\n", sep)
}

func safeDiv(v, n float64) float64 {
	if n == 0 {
		return 0
	}
	return v / n
}

// ── Phase analysis ────────────────────────────────────────────────────────────

func analyzePhases(results []Result) {
	type key struct {
		btc BTCPhase
		alt AltPhase
	}
	type stats struct {
		total, filled, wins, losses int
		sumPnL                     float64
		leverage                   float64
	}
	m := map[key]*stats{}

	for _, r := range results {
		if r.Skipped {
			continue
		}
		k := key{r.BTCPhase, r.AltPhase}
		if m[k] == nil {
			m[k] = &stats{leverage: r.DynLeverage}
		}
		s := m[k]
		s.total++
		if !r.EntryFilled {
			continue
		}
		s.filled++
		s.sumPnL += r.PnLPct
		if r.HitSL {
			s.losses++
		} else if r.PnLPct > 0 {
			s.wins++
		} else {
			s.losses++
		}
	}

	// Skipped analysis
	var skipped, skippedLosses int
	for _, r := range results {
		if !r.Skipped {
			continue
		}
		skipped++
		if r.EntryFilled && r.HitSL {
			skippedLosses++
		}
	}

	sep := strings.Repeat("─", 78)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  PHASE BREAKDOWN\n")
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-16s %5s %6s %5s %5s %6s %7s %7s\n",
		"BTC/Alt", "Sigs", "Filled", "Wins", "Loss", "WR%", "Lev", "AvgPnL%")
	fmt.Printf("%s\n", sep)

	order := []key{
		{PhaseBull, AltStrong}, {PhaseBull, AltNeutral}, {PhaseBull, AltWeak},
		{PhaseNeutral, AltStrong}, {PhaseNeutral, AltNeutral}, {PhaseNeutral, AltWeak},
		{PhaseBear, AltStrong}, {PhaseBear, AltNeutral}, {PhaseBear, AltWeak},
	}
	for _, k := range order {
		s := m[k]
		if s == nil {
			continue
		}
		wr := 0.0
		if s.wins+s.losses > 0 {
			wr = float64(s.wins) / float64(s.wins+s.losses) * 100
		}
		avgPnL := safeDiv(s.sumPnL, float64(s.filled))
		label := fmt.Sprintf("%s/%s", k.btc, k.alt)
		fmt.Printf("  %-16s %5d %6d %5d %5d %5.1f%% %6.1fx %+7.2f%%\n",
			label, s.total, s.filled, s.wins, s.losses, wr, s.leverage, avgPnL)
	}
	fmt.Printf("%s\n", sep)
	fmt.Printf("  Skipped (Bear/skip): %d signals\n", skipped)
	if skipped > 0 {
		fmt.Printf("  Of skipped, would have been SL: %d (%.0f%%) — post-hoc validation\n",
			skippedLosses, float64(skippedLosses)/float64(skipped)*100)
	}
	fmt.Printf("%s\n", sep)
}

func compareStrategies(results []Result, startCapital float64) {
	// Baseline: fixed 3x, all filled signals
	baseEvents := buildEvents(results)
	baseStats := runPortfolio(results, baseEvents, startCapital, 0.10, 3.0)

	// Adaptive: per-trade leverage, skip when Skipped=true
	type ev = tradeEvent
	var adaptEvents []ev
	for i, r := range results {
		if r.Skipped || !r.EntryFilled || r.DynLeverage == 0 {
			continue
		}
		adaptEvents = append(adaptEvents, ev{r.EntryDate, true, i})
		if !r.CloseTime.IsZero() {
			adaptEvents = append(adaptEvents, ev{r.CloseTime, false, i})
		}
	}
	sort.Slice(adaptEvents, func(i, j int) bool {
		if adaptEvents[i].t.Equal(adaptEvents[j].t) {
			return !adaptEvents[i].open && adaptEvents[j].open
		}
		return adaptEvents[i].t.Before(adaptEvents[j].t)
	})

	// Run adaptive
	balance := startCapital
	peak := startCapital
	minBal := startCapital
	maxDD := 0.0
	alloc := make([]float64, len(results))

	for _, e := range adaptEvents {
		r := results[e.idx]
		if e.open {
			alloc[e.idx] = balance * 0.10
		} else {
			raw := alloc[e.idx] * r.DynLeverage * r.PnLPct / 100
			gain := math.Max(raw, -alloc[e.idx])
			balance += gain
			if balance < 0 {
				balance = 0
			}
			if balance < minBal {
				minBal = balance
			}
			if balance > peak {
				peak = balance
			}
			if peak > 0 {
				dd := (peak - balance) / peak * 100
				if dd > maxDD {
					maxDD = dd
				}
			}
		}
	}
	adaptStats := portfolioStats{finalBalance: balance, minBalance: minBal, maxDrawdown: maxDD}

	baseRet := (baseStats.finalBalance - startCapital) / startCapital * 100
	adaptRet := (adaptStats.finalBalance - startCapital) / startCapital * 100

	sep := strings.Repeat("─", 62)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  STRATEGY COMPARISON (start $%.0f, 10%% per trade)\n", startCapital)
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-26s %10s %8s %8s\n", "Strategy", "Final$", "Return%", "MaxDD%")
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-26s %10.2f %+7.1f%% %7.1f%%\n",
		"Fixed 3x (baseline)", baseStats.finalBalance, baseRet, baseStats.maxDrawdown)
	fmt.Printf("  %-26s %10.2f %+7.1f%% %7.1f%%\n",
		"Adaptive (dyn leverage)", adaptStats.finalBalance, adaptRet, adaptStats.maxDrawdown)
	fmt.Printf("%s\n", sep)
	fmt.Printf("  Delta: %+.2f$ / %+.1f%% return / %+.1f%% DD\n",
		adaptStats.finalBalance-baseStats.finalBalance,
		adaptRet-baseRet,
		adaptStats.maxDrawdown-baseStats.maxDrawdown)
	fmt.Printf("%s\n", sep)
}

// ── Portfolio simulation ──────────────────────────────────────────────────────

type portfolioStats struct {
	finalBalance float64
	minBalance   float64
	maxDrawdown  float64 // peak-to-trough %
}

type tradeEvent struct {
	t    time.Time
	open bool
	idx  int
}

// buildEvents returns a sorted event list for the results (filled trades only).
func buildEvents(results []Result) []tradeEvent {
	var events []tradeEvent
	for i, r := range results {
		if !r.EntryFilled {
			continue
		}
		events = append(events, tradeEvent{r.EntryDate, true, i})
		if !r.CloseTime.IsZero() {
			events = append(events, tradeEvent{r.CloseTime, false, i})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].t.Equal(events[j].t) {
			return !events[i].open && events[j].open
		}
		return events[i].t.Before(events[j].t)
	})
	return events
}

// runPortfolio computes equity curve without printing.
// Liquidation cap: loss per trade capped at the allocated margin (can't lose more than 100% of margin).
func runPortfolio(results []Result, events []tradeEvent, startCapital, fraction, leverage float64) portfolioStats {
	balance := startCapital
	peak := startCapital
	minBal := startCapital
	maxDD := 0.0
	alloc := make([]float64, len(results))

	for _, e := range events {
		r := results[e.idx]
		if e.open {
			alloc[e.idx] = balance * fraction
		} else {
			raw := alloc[e.idx] * leverage * r.PnLPct / 100
			gain := math.Max(raw, -alloc[e.idx]) // liquidation cap
			balance += gain
			if balance < 0 {
				balance = 0
			}
			if balance < minBal {
				minBal = balance
			}
			if balance > peak {
				peak = balance
			}
			if peak > 0 {
				dd := (peak - balance) / peak * 100
				if dd > maxDD {
					maxDD = dd
				}
			}
		}
	}
	return portfolioStats{finalBalance: balance, minBalance: minBal, maxDrawdown: maxDD}
}

// simulatePortfolio prints a detailed equity curve for a single leverage/fraction combo.
func simulatePortfolio(results []Result, startCapital, fraction, leverage float64) {
	events := buildEvents(results)

	balance := startCapital
	alloc := make([]float64, len(results))

	sep := strings.Repeat("─", 58)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  PORTFOLIO SIMULATION\n")
	fmt.Printf("  Start $%.2f | %.0f%% per trade | %.0fx leverage\n", startCapital, fraction*100, leverage)
	fmt.Printf("  (liquidation cap: max loss = allocated margin)\n")
	fmt.Printf("%s\n", sep)
	fmt.Printf("  %-11s %-5s %-12s %8s %10s  %s\n", "Date", "Type", "Symbol", "PnL%", "Effect$", "Balance$")
	fmt.Printf("%s\n", sep)

	openCount := 0
	minBal := startCapital
	peak := startCapital
	maxDD := 0.0

	for _, e := range events {
		r := results[e.idx]
		if e.open {
			openCount++
			alloc[e.idx] = balance * fraction
			fmt.Printf("  %s OPEN  %-12s %8s  alloc $%-8.2f  $%.2f  (open:%d)\n",
				e.t.Format("2006-01-02"), r.Signal.Symbol, "",
				alloc[e.idx], balance, openCount)
		} else {
			openCount--
			raw := alloc[e.idx] * leverage * r.PnLPct / 100
			gain := math.Max(raw, -alloc[e.idx]) // liquidation cap
			balance += gain
			if balance < 0 {
				balance = 0
			}
			if balance < minBal {
				minBal = balance
			}
			if balance > peak {
				peak = balance
			}
			if peak > 0 {
				dd := (peak - balance) / peak * 100
				if dd > maxDD {
					maxDD = dd
				}
			}
			liqNote := ""
			if raw < -alloc[e.idx] {
				liqNote = " [LIQ]"
			}
			status := "CLOSE"
			if r.HitSL {
				status = "SL   "
			} else if r.Expired {
				status = "EXP  "
			}
			fmt.Printf("  %s %s %-12s %+7.2f%%  $%+-9.2f  $%.2f%s\n",
				e.t.Format("2006-01-02"), status, r.Signal.Symbol,
				r.PnLPct, gain, balance, liqNote)
		}
	}

	ret := 0.0
	if startCapital > 0 {
		ret = (balance - startCapital) / startCapital * 100
	}
	fmt.Printf("%s\n", sep)
	fmt.Printf("  Final balance : $%.2f  (%+.1f%% total return)\n", balance, ret)
	fmt.Printf("  Min balance   : $%.2f\n", minBal)
	fmt.Printf("  Max drawdown  : %.1f%%\n", maxDD)
	fmt.Printf("%s\n", sep)
}

// ── Time-to-fill distribution ─────────────────────────────────────────────────

func analyzeFillTime(results []Result) {
	var days []float64
	for _, r := range results {
		if r.EntryFilled {
			days = append(days, r.DaysToFill)
		}
	}
	if len(days) == 0 {
		return
	}

	sort.Float64s(days)
	n := len(days)

	pct := func(p float64) float64 {
		idx := p / 100 * float64(n-1)
		lo := int(idx)
		hi := lo + 1
		if hi >= n {
			return days[n-1]
		}
		return days[lo] + (idx-float64(lo))*(days[hi]-days[lo])
	}

	sum := 0.0
	for _, d := range days {
		sum += d
	}
	mean := sum / float64(n)

	// bucket by day
	buckets := map[int]int{}
	for _, d := range days {
		b := int(math.Floor(d))
		buckets[b]++
	}
	maxCnt := 0
	for _, c := range buckets {
		if c > maxCnt {
			maxCnt = c
		}
	}

	// immediate = same candle as signal (< 4h)
	var immediate, sameDay, within3d, within7d, over7d int
	for _, d := range days {
		switch {
		case d < 0.17: // ~4h
			immediate++
		case d < 1:
			sameDay++
		case d <= 3:
			within3d++
		case d <= 7:
			within7d++
		default:
			over7d++
		}
	}

	sep := strings.Repeat("─", 60)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  TIME TO FILL  —  days from signal to entry zone touch\n")
	fmt.Printf("  (filled signals only, n=%d)\n", n)
	fmt.Printf("%s\n", sep)
	fmt.Printf("  Mean          : %.1f days\n", mean)
	fmt.Printf("  Median (p50)  : %.1f days\n", pct(50))
	fmt.Printf("  p75           : %.1f days\n", pct(75))
	fmt.Printf("  p90           : %.1f days\n", pct(90))
	fmt.Printf("  Min / Max     : %.1f / %.1f days\n", days[0], days[n-1])
	fmt.Printf("%s\n", sep)
	fmt.Printf("  < 4h (instant): %d  (%.0f%%)\n", immediate, float64(immediate)/float64(n)*100)
	fmt.Printf("  same day      : %d  (%.0f%%)\n", sameDay, float64(sameDay)/float64(n)*100)
	fmt.Printf("  1–3 days      : %d  (%.0f%%)\n", within3d, float64(within3d)/float64(n)*100)
	fmt.Printf("  4–7 days      : %d  (%.0f%%)\n", within7d, float64(within7d)/float64(n)*100)
	fmt.Printf("  > 7 days      : %d  (%.0f%%)\n", over7d, float64(over7d)/float64(n)*100)
	fmt.Printf("%s\n", sep)

	// histogram by day bucket
	fmt.Printf("  Distribution by day:\n")
	maxKey := 0
	for k := range buckets {
		if k > maxKey {
			maxKey = k
		}
	}
	barScale := 1
	if maxCnt > 20 {
		barScale = maxCnt / 20
	}
	for d := 0; d <= maxKey; d++ {
		cnt := buckets[d]
		if cnt == 0 {
			continue
		}
		bar := strings.Repeat("█", (cnt+barScale-1)/barScale)
		label := fmt.Sprintf("day %2d", d)
		if d == 0 {
			label = "day  0"
		}
		fmt.Printf("  %s : %2d  %s\n", label, cnt, bar)
	}
	fmt.Printf("%s\n", sep)

	// Per-signal detail
	fmt.Printf("  %-6s %-12s %8s  %-10s  %-10s\n", "ID", "Symbol", "DaysToFill", "SignalDate", "EntryDate")
	for _, r := range results {
		if !r.EntryFilled {
			continue
		}
		fmt.Printf("  %-6d %-12s %8.1f  %-10s  %-10s\n",
			r.Signal.SignalID, r.Signal.Symbol,
			r.DaysToFill,
			r.Signal.Date.Format("2006-01-02"),
			r.EntryDate.Format("2006-01-02"))
	}
	fmt.Printf("%s\n", sep)
}

// gridSearch prints a matrix of final balances for combinations of leverage × fraction.
func gridSearch(results []Result, startCapital float64) {
	leverages  := []float64{2, 3, 5, 7, 10, 15, 20, 25, 50}
	fractions  := []float64{0.05, 0.10, 0.15, 0.20, 0.25, 0.30, 0.40, 0.50}

	events := buildEvents(results)
	type cell struct {
		finalPct float64
		maxDD    float64
	}
	grid := make([][]cell, len(leverages))
	for i := range grid {
		grid[i] = make([]cell, len(fractions))
		for j, f := range fractions {
			s := runPortfolio(results, events, startCapital, f, leverages[i])
			ret := (s.finalBalance - startCapital) / startCapital * 100
			grid[i][j] = cell{ret, s.maxDrawdown}
		}
	}

	sep := strings.Repeat("─", 90)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("  GRID SEARCH — Final return%% / MaxDrawdown%%\n")
	fmt.Printf("  Start $%.0f | liquidation cap enabled\n", startCapital)
	fmt.Printf("%s\n", sep)

	// header
	fmt.Printf("  %-8s", "Lev\\Frac")
	for _, f := range fractions {
		fmt.Printf("  %6.0f%%", f*100)
	}
	fmt.Println()
	fmt.Printf("%s\n", sep)

	bestRet := -math.MaxFloat64
	var bestLev, bestFrac float64

	for i, lev := range leverages {
		fmt.Printf("  %-8.0fx", lev)
		for j, f := range fractions {
			c := grid[i][j]
			fmt.Printf("  %+5.0f%%", c.finalPct)
			if c.finalPct > bestRet {
				bestRet = c.finalPct
				bestLev = lev
				bestFrac = f
			}
		}
		fmt.Println()
		// second row: drawdown
		fmt.Printf("  %-8s", "  DD")
		for j := range fractions {
			fmt.Printf("  %5.0f%%↓", grid[i][j].maxDD)
		}
		fmt.Println()
	}

	fmt.Printf("%s\n", sep)
	fmt.Printf("  Best combo: %.0fx leverage, %.0f%% per trade → %+.1f%% return\n",
		bestLev, bestFrac*100, bestRet)
	fmt.Printf("%s\n", sep)
}

func pct2(v float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return v / float64(n)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	csvPath  := flag.String("csv", "signals_trader3.csv", "signals CSV file")
	leverage := flag.Float64("leverage", 2.0, "leverage multiplier")
	flag.Parse()

	signals, err := parseSignals(*csvPath)
	if err != nil {
		log.Fatalf("load signals: %v", err)
	}
	fmt.Printf("[*] Loaded %d signals from %s\n\n", len(signals), *csvPath)

	// Optional DB connection
	cfg, _ := config.Load()
	var db *pgxpool.Pool
	if cfg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		db, err = pgxpool.New(ctx, cfg.DatabaseURL)
		cancel()
		if err != nil {
			log.Printf("[!] DB unavailable — using Bybit API for all candles")
			db = nil
		} else {
			fmt.Println("[*] Connected to TimescaleDB")
		}
	}

	ctx := context.Background()
	results := make([]Result, 0, len(signals))
	partial33 := make([]ExitPartial33Result, 0, len(signals))
	bResults := make([]PartialBEResult, 0, len(signals))

	// Pre-fetch BTC candles for phase detection (covers all signal dates + lookback)
	firstSig := signals[0].Date
	lastSig := signals[len(signals)-1].Date
	fmt.Println("[*] Fetching BTC weekly candles for phase detection…")
	btcWeekly, _ := bybitWeeklyCandles("BTCUSDT",
		firstSig.AddDate(-4, 0, 0), lastSig.Add(24*time.Hour))
	fmt.Printf("    got %d weekly candles\n", len(btcWeekly))

	fmt.Println("[*] Fetching BTC daily candles for phase detection…")
	btcDaily, _ := bybitDailyCandles("BTCUSDT",
		firstSig.AddDate(0, 0, -70), lastSig.Add(24*time.Hour))
	fmt.Printf("    got %d daily candles\n", len(btcDaily))

	// Cache for alt daily candles: symbol → full daily series
	altDailyCache := map[string][]Candle{}

	for i, sig := range signals {
		from := sig.Date
		to := sig.Date.Add((maxHoldDays + 2) * 24 * time.Hour)

		fmt.Printf("[%d/%d] #%d %-12s %s … ", i+1, len(signals), sig.SignalID, sig.Symbol, sig.Date.Format("2006-01-02"))

		// ── Phase detection (no look-ahead: slice up to sig.Date) ──────────
		wkSlice := candlesBefore(btcWeekly, sig.Date)
		daySlice := candlesBefore(btcDaily, sig.Date)

		if altDailyCache[sig.Symbol] == nil {
			ac, _ := bybitDailyCandles(sig.Symbol,
				sig.Date.AddDate(0, 0, -30), lastSig.Add(24*time.Hour))
			altDailyCache[sig.Symbol] = ac
		}
		altSlice := candlesBefore(altDailyCache[sig.Symbol], sig.Date)

		btcPhase := detectBTCPhase(wkSlice, daySlice)
		altPhase := detectAltPhase(altSlice, daySlice)
		dynLev := getDynLeverage(btcPhase, altPhase)

		// ── 1H candles for simulation ───────────────────────────────────────
		var candles []Candle

		if db != nil {
			candles, _ = dbCandles(ctx, db, sig.Symbol, from, to)
		}

		if len(candles) < 10 {
			fmt.Print("(Bybit) ")
			candles, err = bybitCandles(sig.Symbol, from, to)
			if err != nil {
				fmt.Printf("ERROR: %v\n", err)
				continue
			}
		}

		res := simulate(sig, candles)
		res.BTCPhase = btcPhase
		res.AltPhase = altPhase
		res.DynLeverage = dynLev
		res.Skipped = dynLev == 0
		results = append(results, res)
		partial33 = append(partial33, simulatePartial33(sig, candles))
		bResults = append(bResults, simulateBreakeven(sig, candles))

		phaseTag := fmt.Sprintf("[BTC:%s Alt:%s lev:%.0fx]", btcPhase, altPhase, dynLev)
		switch {
		case res.Skipped:
			fmt.Printf("SKIP %s\n", phaseTag)
		case !res.EntryFilled:
			fmt.Printf("not filled %s\n", phaseTag)
		case res.HitSL:
			fmt.Printf("SL hit  maxTP=%-2d pnl=%+.2f%% %s\n", res.MaxTPHit, res.PnLPct, phaseTag)
		case res.Expired:
			fmt.Printf("expired maxTP=%-2d pnl=%+.2f%% %s\n", res.MaxTPHit, res.PnLPct, phaseTag)
		default:
			fmt.Printf("maxTP=%-2d pnl=%+.2f%% %s\n", res.MaxTPHit, res.PnLPct, phaseTag)
		}
	}

	fmt.Println()
	printStats(results)

	fmt.Println()
	analyzeTPStrategies(results)

	fmt.Println()
	analyzePhases(results)

	fmt.Println()
	compareStrategies(results, 1000.0)

	fmt.Println()
	simulatePortfolio(results, 1000.0, 0.10, *leverage)

	fmt.Println()
	analyzeFillTime(results)

	fmt.Println()
	gridSearch(results, 1000.0)

	fmt.Println()
	compareExitStrategies(results, partial33, bResults, 1000.0)
}
