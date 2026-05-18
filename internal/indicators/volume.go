package indicators

import (
	"math"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// VolumeSignal summarises volume state relative to the moving average.
type VolumeSignal struct {
	CurrentVolume float64
	AvgVolume     float64
	Ratio         float64
	IsHigh        bool // ratio > 1.5
	IsLow         bool // ratio < 0.5
}

// Level represents a support or resistance price level.
type Level struct {
	Price    float64
	Type     string  // "support" | "resistance"
	Strength int     // how many times price touched this level
	Distance float64 // % distance from current price (absolute)
}

// CalculateVolume derives a VolumeSignal from candle data.
// period controls how many candles are used to compute the average.
func CalculateVolume(candles []bybit.Candle, period int) VolumeSignal {
	if len(candles) == 0 {
		return VolumeSignal{}
	}

	current := candles[len(candles)-1].Volume

	start := len(candles) - period
	if start < 0 {
		start = 0
	}

	var sum float64
	for i := start; i < len(candles)-1; i++ {
		sum += candles[i].Volume
	}

	count := len(candles) - 1 - start
	avg := 0.0
	if count > 0 {
		avg = sum / float64(count)
	}

	ratio := 0.0
	if avg > 0 {
		ratio = current / avg
	}

	return VolumeSignal{
		CurrentVolume: current,
		AvgVolume:     avg,
		Ratio:         ratio,
		IsHigh:        ratio > 1.5,
		IsLow:         ratio < 0.5,
	}
}

// FindSupportResistance identifies local highs and lows in the candle series.
// lookback controls how many candles on each side must be lower/higher to
// qualify as a pivot.
func FindSupportResistance(candles []bybit.Candle, lookback int) []Level {
	if len(candles) < 2*lookback+1 {
		return nil
	}

	currentPrice := candles[len(candles)-1].Close

	// Tolerance used to cluster levels together (0.5% of current price).
	tol := currentPrice * 0.005

	type pivot struct {
		price float64
		kind  string
	}

	var pivots []pivot

	for i := lookback; i < len(candles)-lookback; i++ {
		high := candles[i].High
		low := candles[i].Low

		// Check resistance (local high).
		isHigh := true
		for j := i - lookback; j <= i+lookback; j++ {
			if j == i {
				continue
			}
			if candles[j].High >= high {
				isHigh = false
				break
			}
		}
		if isHigh {
			pivots = append(pivots, pivot{price: high, kind: "resistance"})
		}

		// Check support (local low).
		isLow := true
		for j := i - lookback; j <= i+lookback; j++ {
			if j == i {
				continue
			}
			if candles[j].Low <= low {
				isLow = false
				break
			}
		}
		if isLow {
			pivots = append(pivots, pivot{price: low, kind: "support"})
		}
	}

	// Cluster pivots by proximity.
	type cluster struct {
		price    float64
		kind     string
		strength int
	}

	var clusters []cluster

nextPivot:
	for _, p := range pivots {
		for i := range clusters {
			if clusters[i].kind == p.kind && math.Abs(clusters[i].price-p.price) <= tol {
				// Average the price into the cluster.
				clusters[i].price = (clusters[i].price*float64(clusters[i].strength) + p.price) / float64(clusters[i].strength+1)
				clusters[i].strength++
				continue nextPivot
			}
		}
		clusters = append(clusters, cluster{price: p.price, kind: p.kind, strength: 1})
	}

	levels := make([]Level, 0, len(clusters))
	for _, cl := range clusters {
		dist := 0.0
		if currentPrice > 0 {
			dist = math.Abs(cl.price-currentPrice) / currentPrice * 100
		}
		levels = append(levels, Level{
			Price:    cl.price,
			Type:     cl.kind,
			Strength: cl.strength,
			Distance: dist,
		})
	}

	return levels
}
