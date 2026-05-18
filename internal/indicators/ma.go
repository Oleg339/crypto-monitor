package indicators

import (
	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// SMA calculates the Simple Moving Average for the given period.
// Returns a slice of the same length as values; positions where there are
// fewer than period elements are set to 0.
func SMA(values []float64, period int) []float64 {
	result := make([]float64, len(values))
	if period <= 0 || len(values) < period {
		return result
	}

	// Seed the first window.
	var sum float64
	for i := 0; i < period; i++ {
		sum += values[i]
	}
	result[period-1] = sum / float64(period)

	for i := period; i < len(values); i++ {
		sum += values[i] - values[i-period]
		result[i] = sum / float64(period)
	}

	return result
}

// EMA calculates the Exponential Moving Average for the given period.
// Uses the standard multiplier k = 2 / (period + 1).
func EMA(values []float64, period int) []float64 {
	result := make([]float64, len(values))
	if period <= 0 || len(values) < period {
		return result
	}

	k := 2.0 / float64(period+1)

	// Seed with the SMA of the first window.
	var seed float64
	for i := 0; i < period; i++ {
		seed += values[i]
	}
	seed /= float64(period)
	result[period-1] = seed

	for i := period; i < len(values); i++ {
		result[i] = values[i]*k + result[i-1]*(1-k)
	}

	return result
}

// MASignal summarises moving-average state for the latest candle.
type MASignal struct {
	MA10         float64
	MA20         float64
	MA60         float64
	PriceAboveMA10 bool
	PriceAboveMA20 bool
	PriceAboveMA60 bool
	GoldenCross  bool // MA10 just crossed above MA20
	DeathCross   bool // MA10 just crossed below MA20
}

// CalculateMA derives an MASignal from the provided candle slice.
// At least 61 candles are required to produce meaningful values.
func CalculateMA(candles []bybit.Candle) MASignal {
	if len(candles) == 0 {
		return MASignal{}
	}

	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}

	ma10 := SMA(closes, 10)
	ma20 := SMA(closes, 20)
	ma60 := SMA(closes, 60)

	n := len(closes)
	last := closes[n-1]

	sig := MASignal{
		MA10:         ma10[n-1],
		MA20:         ma20[n-1],
		MA60:         ma60[n-1],
		PriceAboveMA10: last > ma10[n-1] && ma10[n-1] != 0,
		PriceAboveMA20: last > ma20[n-1] && ma20[n-1] != 0,
		PriceAboveMA60: last > ma60[n-1] && ma60[n-1] != 0,
	}

	// Golden/death cross: check the previous bar's relationship.
	if n >= 2 && ma10[n-2] != 0 && ma20[n-2] != 0 {
		prevMA10AboveMA20 := ma10[n-2] > ma20[n-2]
		currMA10AboveMA20 := ma10[n-1] > ma20[n-1]
		sig.GoldenCross = !prevMA10AboveMA20 && currMA10AboveMA20
		sig.DeathCross = prevMA10AboveMA20 && !currMA10AboveMA20
	}

	return sig
}
