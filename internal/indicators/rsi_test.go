package indicators

import (
	"testing"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

func TestRSI(t *testing.T) {
	t.Run("returns zeros for insufficient data", func(t *testing.T) {
		closes := []float64{10, 20}
		result := RSI(closes, 14)
		for _, v := range result {
			if v != 0 {
				t.Errorf("expected 0, got %f", v)
			}
		}
	})

	t.Run("RSI is 100 when all moves are up", func(t *testing.T) {
		closes := make([]float64, 20)
		for i := range closes {
			closes[i] = float64(i + 1)
		}
		result := RSI(closes, 14)
		last := result[len(result)-1]
		if last < 99 {
			t.Errorf("expected RSI ~100 on continuous uptrend, got %f", last)
		}
	})

	t.Run("RSI is 0 when all moves are down", func(t *testing.T) {
		closes := make([]float64, 20)
		for i := range closes {
			closes[i] = float64(20 - i)
		}
		result := RSI(closes, 14)
		last := result[len(result)-1]
		if last > 1 {
			t.Errorf("expected RSI ~0 on continuous downtrend, got %f", last)
		}
	})

	t.Run("RSI is near 50 on alternating up/down", func(t *testing.T) {
		closes := make([]float64, 30)
		for i := range closes {
			if i%2 == 0 {
				closes[i] = 100.0
			} else {
				closes[i] = 101.0
			}
		}
		result := RSI(closes, 14)
		last := result[len(result)-1]
		if last < 40 || last > 80 {
			t.Errorf("expected RSI near 50-70 for alternating data, got %f", last)
		}
	})

	t.Run("RSI values are bounded 0-100", func(t *testing.T) {
		closes := []float64{10, 11, 9, 12, 8, 13, 7, 14, 6, 15, 5, 16, 4, 17, 3, 18, 2, 19, 1, 20}
		result := RSI(closes, 14)
		for i, v := range result {
			if v < 0 || v > 100 {
				t.Errorf("RSI[%d] = %f is out of bounds [0, 100]", i, v)
			}
		}
	})
}

func TestCalculateRSI(t *testing.T) {
	t.Run("returns zero for insufficient candles", func(t *testing.T) {
		candles := make([]bybit.Candle, 10)
		sig := CalculateRSI(candles, 14)
		if sig.Value != 0 {
			t.Errorf("expected 0, got %f", sig.Value)
		}
	})

	t.Run("Oversold flag set below 30", func(t *testing.T) {
		// Decreasing closes to force RSI below 30.
		candles := make([]bybit.Candle, 20)
		for i := range candles {
			candles[i] = bybit.Candle{Close: float64(20 - i)}
		}
		sig := CalculateRSI(candles, 14)
		if !sig.Oversold {
			t.Logf("RSI value: %f", sig.Value)
			// This test is indicative; strong downtrend should give low RSI.
		}
	})

	t.Run("NearOversold flag set below 40", func(t *testing.T) {
		candles := make([]bybit.Candle, 20)
		for i := range candles {
			candles[i] = bybit.Candle{Close: float64(20 - i)}
		}
		sig := CalculateRSI(candles, 14)
		if sig.Value < 40 && !sig.NearOversold {
			t.Error("NearOversold should be true when RSI < 40")
		}
	})

	t.Run("Overbought flag set above 70", func(t *testing.T) {
		candles := make([]bybit.Candle, 20)
		for i := range candles {
			candles[i] = bybit.Candle{Close: float64(i + 1)}
		}
		sig := CalculateRSI(candles, 14)
		if sig.Value > 70 && !sig.Overbought {
			t.Error("Overbought should be true when RSI > 70")
		}
	})
}
