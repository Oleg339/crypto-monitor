package strategy

import (
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
	"github.com/yourorg/crypto-monitor/internal/indicators"
)

// SignalType classifies a trade signal direction.
type SignalType int

const (
	NoSignal    SignalType = iota
	LongSignal
	ShortSignal
)

func (s SignalType) String() string {
	switch s {
	case LongSignal:
		return "LONG"
	case ShortSignal:
		return "SHORT"
	default:
		return "NONE"
	}
}

// Indicators aggregates all computed indicator values for a single bar.
type Indicators struct {
	MA     indicators.MASignal
	RSI    indicators.RSISignal
	ATR    indicators.ATRSignal
	Volume indicators.VolumeSignal
	Levels []indicators.Level
}

// TradeSignal represents an actionable trade opportunity.
type TradeSignal struct {
	Type        SignalType
	Symbol      string
	Strategy    string
	Timeframe   string
	Entry       float64
	StopLoss    float64
	TakeProfits []float64
	RRRatio     float64
	Indicators  Indicators
	Time        time.Time
}

// Strategy is the interface that all trading strategies must implement.
type Strategy interface {
	// Name returns the human-readable strategy name.
	Name() string
	// Timeframe returns the preferred candle interval (e.g. "240", "D").
	Timeframe() string
	// Analyze examines the supplied candles and indicators and returns a signal
	// when the entry conditions are met. Returns nil when there is no signal.
	Analyze(symbol string, candles []bybit.Candle, ind Indicators) *TradeSignal
}
