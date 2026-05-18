package strategy

import (
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// MeanReversionStrategy generates LONG signals when price is below MA60 on 4H,
// the current candle is bullish, volume is elevated, and RSI is near oversold.
type MeanReversionStrategy struct {
	minRR float64
}

// NewMeanReversionStrategy creates a new instance with the given minimum R:R ratio.
func NewMeanReversionStrategy(minRR float64) *MeanReversionStrategy {
	return &MeanReversionStrategy{minRR: minRR}
}

func (s *MeanReversionStrategy) Name() string      { return "Mean Reversion" }
func (s *MeanReversionStrategy) Timeframe() string { return "240" }

// Analyze checks for mean-reversion LONG conditions and returns a TradeSignal when met.
func (s *MeanReversionStrategy) Analyze(symbol string, candles []bybit.Candle, ind Indicators) *TradeSignal {
	if len(candles) < 3 {
		return nil
	}

	last := candles[len(candles)-1]
	entry := last.Close

	// Condition 1: price below MA60.
	if ind.MA.MA60 == 0 || entry >= ind.MA.MA60 {
		return nil
	}

	// Condition 2: current candle is bullish (close > open).
	if last.Close <= last.Open {
		return nil
	}

	// Condition 3: volume ratio > 1.5.
	if ind.Volume.Ratio <= 1.5 {
		return nil
	}

	// Condition 4: RSI < 40.
	if ind.RSI.Value == 0 || ind.RSI.Value >= 40 {
		return nil
	}

	// Stop: lowest low of last 3 candles minus 1%.
	lowestLow := candles[len(candles)-1].Low
	for _, c := range candles[len(candles)-3:] {
		if c.Low < lowestLow {
			lowestLow = c.Low
		}
	}
	stopLoss := lowestLow * 0.99

	// Take profit: MA20 price.
	tp1 := ind.MA.MA20
	if tp1 <= entry {
		return nil
	}

	// Second take profit: MA60 + (MA60 - entry) for extension.
	tp2 := ind.MA.MA60 + (ind.MA.MA60 - entry)

	// Calculate R:R.
	risk := entry - stopLoss
	if risk <= 0 {
		return nil
	}
	reward := tp1 - entry
	rrRatio := reward / risk

	if rrRatio < s.minRR {
		return nil
	}

	tps := []float64{tp1}
	if tp2 > tp1 {
		tps = append(tps, tp2)
	}

	return &TradeSignal{
		Type:        LongSignal,
		Symbol:      symbol,
		Strategy:    s.Name(),
		Timeframe:   s.Timeframe(),
		Entry:       entry,
		StopLoss:    stopLoss,
		TakeProfits: tps,
		RRRatio:     rrRatio,
		Indicators:  ind,
		Time:        time.Now().UTC(),
	}
}
