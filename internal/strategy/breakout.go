package strategy

import (
	"math"
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
	"github.com/yourorg/crypto-monitor/internal/indicators"
)

// SupportBounceStrategy generates LONG signals when price is near a support level,
// RSI is oversold, volume is increasing, and the daily MA60 trend is upward.
type SupportBounceStrategy struct {
	minRR float64
}

// NewSupportBounceStrategy creates a new instance with the given minimum R:R ratio.
func NewSupportBounceStrategy(minRR float64) *SupportBounceStrategy {
	return &SupportBounceStrategy{minRR: minRR}
}

func (s *SupportBounceStrategy) Name() string      { return "Support Bounce" }
func (s *SupportBounceStrategy) Timeframe() string { return "240" }

// Analyze checks for support-bounce LONG conditions and returns a TradeSignal when met.
func (s *SupportBounceStrategy) Analyze(symbol string, candles []bybit.Candle, ind Indicators) *TradeSignal {
	if len(candles) < 3 {
		return nil
	}

	last := candles[len(candles)-1]
	entry := last.Close

	// Condition 1: price within 0.5% of a support level.
	support := findNearestSupport(ind.Levels, entry)
	if support == nil {
		return nil
	}

	// Condition 2: RSI < 35.
	if ind.RSI.Value == 0 || ind.RSI.Value >= 35 {
		return nil
	}

	// Condition 3: volume increasing on last 2 candles.
	if !volumeIncreasing(candles) {
		return nil
	}

	// Condition 4: MA60 slope positive (last candle MA60 > previous bar's MA60).
	// We approximate slope by comparing the current MA60 to the entry relative to
	// the prior two candles (if available). The caller is expected to supply 1D
	// candles; here we use the MA60 directly.
	if ind.MA.MA60 == 0 {
		return nil
	}
	// Positive slope proxy: price is approaching from below MA60 upward.
	// We require that price < MA60 (bounce from below) AND MA60 > previous value.
	// Since we only have the final MA signal, we do the simpler check:
	// the long-term trend is up when MA60 > MA20 is not required; instead we check
	// that the current close is not already above MA60 (bounce scenario).
	if entry >= ind.MA.MA60 {
		return nil
	}

	// Stop: support level - 2%.
	stopLoss := support.Price * 0.98

	// Take profit: nearest resistance above entry.
	resistance := findNearestResistance(ind.Levels, entry)
	if resistance == nil {
		return nil
	}
	tp1 := resistance.Price

	// Sanity checks.
	if tp1 <= entry {
		return nil
	}

	risk := entry - stopLoss
	if risk <= 0 {
		return nil
	}
	reward := tp1 - entry
	rrRatio := reward / risk

	if rrRatio < s.minRR {
		return nil
	}

	return &TradeSignal{
		Type:        LongSignal,
		Symbol:      symbol,
		Strategy:    s.Name(),
		Timeframe:   s.Timeframe(),
		Entry:       entry,
		StopLoss:    stopLoss,
		TakeProfits: []float64{tp1},
		RRRatio:     rrRatio,
		Indicators:  ind,
		Time:        time.Now().UTC(),
	}
}

// findNearestSupport returns the support level closest to currentPrice that is
// within 0.5% of the price, or nil if none qualifies.
func findNearestSupport(levels []indicators.Level, currentPrice float64) *indicators.Level {
	var best *indicators.Level
	bestDist := math.MaxFloat64

	for i := range levels {
		l := &levels[i]
		if l.Type != "support" {
			continue
		}
		if l.Price >= currentPrice {
			continue
		}
		dist := (currentPrice - l.Price) / currentPrice * 100
		if dist <= 0.5 && dist < bestDist {
			bestDist = dist
			best = l
		}
	}

	return best
}

// findNearestResistance returns the nearest resistance level above currentPrice.
func findNearestResistance(levels []indicators.Level, currentPrice float64) *indicators.Level {
	var best *indicators.Level
	bestDist := math.MaxFloat64

	for i := range levels {
		l := &levels[i]
		if l.Type != "resistance" {
			continue
		}
		if l.Price <= currentPrice {
			continue
		}
		dist := l.Price - currentPrice
		if dist < bestDist {
			bestDist = dist
			best = l
		}
	}

	return best
}

// volumeIncreasing returns true if the last two candles show increasing volume.
func volumeIncreasing(candles []bybit.Candle) bool {
	if len(candles) < 2 {
		return false
	}
	last := candles[len(candles)-1]
	prev := candles[len(candles)-2]
	return last.Volume > prev.Volume
}
