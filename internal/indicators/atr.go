package indicators

import (
	"math"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// ATR calculates the Average True Range using Wilder's smoothing.
// Returns a slice of the same length as candles; the first period values are 0.
func ATR(candles []bybit.Candle, period int) []float64 {
	result := make([]float64, len(candles))
	if period <= 0 || len(candles) <= period {
		return result
	}

	trueRanges := make([]float64, len(candles))
	for i := 1; i < len(candles); i++ {
		high := candles[i].High
		low := candles[i].Low
		prevClose := candles[i-1].Close

		tr := math.Max(high-low, math.Max(math.Abs(high-prevClose), math.Abs(low-prevClose)))
		trueRanges[i] = tr
	}

	// Seed: simple average of first period true ranges.
	var seedSum float64
	for i := 1; i <= period; i++ {
		seedSum += trueRanges[i]
	}
	result[period] = seedSum / float64(period)

	// Wilder smoothing.
	for i := period + 1; i < len(candles); i++ {
		result[i] = (result[i-1]*float64(period-1) + trueRanges[i]) / float64(period)
	}

	return result
}

// ATRSignal summarises ATR state including suggested stop levels.
type ATRSignal struct {
	Value      float64
	StopLong   float64 // entry - 1.5 * ATR
	StopShort  float64 // entry + 1.5 * ATR
}

// CalculateATR derives an ATRSignal from the provided candles.
func CalculateATR(candles []bybit.Candle, period int, entryPrice float64) ATRSignal {
	if len(candles) <= period {
		return ATRSignal{}
	}

	atrValues := ATR(candles, period)
	val := atrValues[len(atrValues)-1]

	return ATRSignal{
		Value:     val,
		StopLong:  entryPrice - 1.5*val,
		StopShort: entryPrice + 1.5*val,
	}
}
