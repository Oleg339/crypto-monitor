package indicators

import (
	"testing"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

func makeCandlesATR(n int, high, low, close_ float64) []bybit.Candle {
	candles := make([]bybit.Candle, n)
	for i := range candles {
		candles[i] = bybit.Candle{
			High:  high,
			Low:   low,
			Close: close_,
			Open:  (high + low) / 2,
		}
	}
	return candles
}

func TestATR(t *testing.T) {
	t.Run("returns zeros for insufficient data", func(t *testing.T) {
		candles := makeCandlesATR(5, 10, 8, 9)
		result := ATR(candles, 14)
		for _, v := range result {
			if v != 0 {
				t.Errorf("expected 0, got %f", v)
			}
		}
	})

	t.Run("constant range produces constant ATR", func(t *testing.T) {
		// Each candle: high=10, low=8, close=9 → TR = max(2, |10-9|, |8-9|) = 2
		candles := makeCandlesATR(20, 10, 8, 9)
		result := ATR(candles, 14)
		last := result[len(result)-1]
		if last < 1.9 || last > 2.1 {
			t.Errorf("expected ATR ~2.0 for constant range candles, got %f", last)
		}
	})

	t.Run("ATR increases with widening ranges", func(t *testing.T) {
		candles := make([]bybit.Candle, 30)
		for i := range candles {
			spread := float64(i+1) * 0.5
			candles[i] = bybit.Candle{
				High:  100 + spread,
				Low:   100 - spread,
				Close: 100,
			}
		}
		result := ATR(candles, 14)
		// ATR should grow as spreads increase.
		early := result[14]
		late := result[29]
		if late <= early {
			t.Errorf("ATR should increase: early=%f late=%f", early, late)
		}
	})
}

func TestCalculateATR(t *testing.T) {
	t.Run("returns zero signal for insufficient candles", func(t *testing.T) {
		candles := makeCandlesATR(10, 10, 8, 9)
		sig := CalculateATR(candles, 14, 9)
		if sig.Value != 0 {
			t.Errorf("expected 0 value, got %f", sig.Value)
		}
	})

	t.Run("StopLong is entry minus 1.5 ATR", func(t *testing.T) {
		candles := makeCandlesATR(20, 10, 8, 9)
		entry := 9.0
		sig := CalculateATR(candles, 14, entry)
		expected := entry - 1.5*sig.Value
		if !almostEqual(sig.StopLong, expected) {
			t.Errorf("StopLong: expected %f, got %f", expected, sig.StopLong)
		}
	})

	t.Run("StopShort is entry plus 1.5 ATR", func(t *testing.T) {
		candles := makeCandlesATR(20, 10, 8, 9)
		entry := 9.0
		sig := CalculateATR(candles, 14, entry)
		expected := entry + 1.5*sig.Value
		if !almostEqual(sig.StopShort, expected) {
			t.Errorf("StopShort: expected %f, got %f", expected, sig.StopShort)
		}
	})
}
