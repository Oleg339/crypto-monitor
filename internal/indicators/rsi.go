package indicators

import (
	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// RSI calculates the Relative Strength Index using Wilder's smoothing method.
// Returns a slice of the same length as closes; values before period are 0.
func RSI(closes []float64, period int) []float64 {
	result := make([]float64, len(closes))
	if period <= 0 || len(closes) <= period {
		return result
	}

	// Compute initial average gain/loss over the first period.
	var avgGain, avgLoss float64
	for i := 1; i <= period; i++ {
		delta := closes[i] - closes[i-1]
		if delta > 0 {
			avgGain += delta
		} else {
			avgLoss -= delta
		}
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	rs := 0.0
	if avgLoss != 0 {
		rs = avgGain / avgLoss
	}
	result[period] = 100 - 100/(1+rs)

	// Wilder smoothing for subsequent values.
	for i := period + 1; i < len(closes); i++ {
		delta := closes[i] - closes[i-1]
		gain, loss := 0.0, 0.0
		if delta > 0 {
			gain = delta
		} else {
			loss = -delta
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)

		if avgLoss == 0 {
			result[i] = 100
		} else {
			rs = avgGain / avgLoss
			result[i] = 100 - 100/(1+rs)
		}
	}

	return result
}

// RSISignal summarises RSI state for the latest candle.
type RSISignal struct {
	Value        float64
	Oversold     bool // < 30
	Overbought   bool // > 70
	NearOversold bool // < 40
}

// CalculateRSI derives an RSISignal from the provided candle slice.
func CalculateRSI(candles []bybit.Candle, period int) RSISignal {
	if len(candles) <= period {
		return RSISignal{}
	}

	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}

	rsiValues := RSI(closes, period)
	val := rsiValues[len(rsiValues)-1]

	return RSISignal{
		Value:        val,
		Oversold:     val < 30,
		Overbought:   val > 70,
		NearOversold: val < 40,
	}
}
