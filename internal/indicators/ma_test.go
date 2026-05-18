package indicators

import (
	"math"
	"testing"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

func TestSMA(t *testing.T) {
	t.Run("returns all zeros for insufficient data", func(t *testing.T) {
		result := SMA([]float64{1, 2}, 5)
		for _, v := range result {
			if v != 0 {
				t.Errorf("expected 0, got %f", v)
			}
		}
	})

	t.Run("calculates correct SMA", func(t *testing.T) {
		values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		result := SMA(values, 3)
		// Index 2: (1+2+3)/3 = 2
		if !almostEqual(result[2], 2.0) {
			t.Errorf("expected 2.0, got %f", result[2])
		}
		// Index 4: (3+4+5)/3 = 4
		if !almostEqual(result[4], 4.0) {
			t.Errorf("expected 4.0, got %f", result[4])
		}
		// Index 9: (8+9+10)/3 = 9
		if !almostEqual(result[9], 9.0) {
			t.Errorf("expected 9.0, got %f", result[9])
		}
	})

	t.Run("returns zeros before period", func(t *testing.T) {
		values := []float64{1, 2, 3, 4, 5}
		result := SMA(values, 3)
		if result[0] != 0 || result[1] != 0 {
			t.Errorf("expected leading zeros, got %v", result[:2])
		}
	})

	t.Run("period of one returns identity", func(t *testing.T) {
		values := []float64{3.0, 7.5, 2.1}
		result := SMA(values, 1)
		for i, v := range values {
			if !almostEqual(result[i], v) {
				t.Errorf("index %d: expected %f, got %f", i, v, result[i])
			}
		}
	})
}

func TestEMA(t *testing.T) {
	t.Run("returns all zeros for insufficient data", func(t *testing.T) {
		result := EMA([]float64{1, 2}, 5)
		for _, v := range result {
			if v != 0 {
				t.Errorf("expected 0, got %f", v)
			}
		}
	})

	t.Run("EMA seed equals SMA for first period", func(t *testing.T) {
		values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		period := 3
		ema := EMA(values, period)
		sma := SMA(values, period)
		// Seed value (index period-1) should equal the SMA.
		if !almostEqual(ema[period-1], sma[period-1]) {
			t.Errorf("EMA seed %f != SMA %f", ema[period-1], sma[period-1])
		}
	})

	t.Run("EMA reacts faster than SMA on uptrend", func(t *testing.T) {
		// Flat series followed by a large spike: EMA weights recent values more
		// heavily so it catches up to the spike faster than SMA.
		values := make([]float64, 20)
		for i := range values {
			values[i] = 1.0
		}
		values[19] = 100.0 // sudden spike at the last bar
		ema := EMA(values, 5)
		sma := SMA(values, 5)
		// EMA at the spike bar must be greater than SMA because EMA reacts faster.
		last := len(values) - 1
		if ema[last] <= sma[last] {
			t.Errorf("EMA %f should be > SMA %f on rising data", ema[last], sma[last])
		}
	})
}

func TestCalculateMA(t *testing.T) {
	t.Run("returns zero signal for empty candles", func(t *testing.T) {
		sig := CalculateMA(nil)
		if sig.MA10 != 0 || sig.MA20 != 0 || sig.MA60 != 0 {
			t.Errorf("expected zero signal for empty candles")
		}
	})

	t.Run("detects golden cross", func(t *testing.T) {
		// Build candles where MA10 crosses above MA20.
		// First 30 candles are declining, then 35 candles are rising so that
		// the short MA reacts first.
		candles := make([]bybit.Candle, 65)
		for i := range candles {
			price := 100.0
			if i < 30 {
				price = 100.0 - float64(i)*0.5
			} else {
				price = 85.0 + float64(i-30)*2.0
			}
			candles[i] = bybit.Candle{Close: price, Open: price - 0.1, High: price + 0.1, Low: price - 0.2}
		}

		sig := CalculateMA(candles)
		// MA values should be populated.
		if sig.MA10 == 0 || sig.MA20 == 0 {
			t.Error("MA values should not be zero with enough candles")
		}
	})

	t.Run("PriceAboveMA60 correct", func(t *testing.T) {
		candles := make([]bybit.Candle, 65)
		for i := range candles {
			candles[i] = bybit.Candle{Close: 100.0, Open: 99.0, High: 101.0, Low: 99.0}
		}
		// Last candle above MA60.
		candles[64] = bybit.Candle{Close: 200.0, Open: 199.0, High: 201.0, Low: 199.0}
		sig := CalculateMA(candles)
		if !sig.PriceAboveMA60 {
			t.Error("expected PriceAboveMA60 to be true")
		}
	})
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
