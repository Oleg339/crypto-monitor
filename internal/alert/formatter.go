package alert

import (
	"fmt"
	"math"
	"strings"

	"github.com/yourorg/crypto-monitor/internal/strategy"
)

// Formatter converts a TradeSignal into a human-readable Telegram message.
type Formatter struct {
	totalCapital float64
	riskPct      float64
	leverage     int
}

// NewFormatter creates a new Formatter.
// capital is the total portfolio capital in USD.
// riskPct is the percentage of capital to risk per trade (e.g. 1.0 for 1%).
// leverage is the applied leverage multiplier.
func NewFormatter(capital, riskPct float64, leverage int) *Formatter {
	return &Formatter{
		totalCapital: capital,
		riskPct:      riskPct,
		leverage:     leverage,
	}
}

// Format produces a formatted alert string for the supplied TradeSignal.
func (f *Formatter) Format(sig strategy.TradeSignal) string {
	direction := sig.Type.String()
	pair := toDisplayPair(sig.Symbol)

	rsiStr := formatRSI(sig.Indicators.RSI.Value)
	volumeStr := fmt.Sprintf("%.1fx від середнього", sig.Indicators.Volume.Ratio)
	if sig.Indicators.Volume.IsHigh {
		volumeStr += " ✅"
	}

	// Position sizing.
	riskUSD := f.totalCapital * f.riskPct / 100
	riskPct := (sig.Entry - sig.StopLoss) / sig.Entry * 100
	qty := 0.0
	if sig.StopLoss > 0 && sig.Entry > sig.StopLoss {
		qty = riskUSD / (sig.Entry - sig.StopLoss)
	}
	margin := (qty * sig.Entry) / float64(f.leverage)

	stopPct := -(sig.Entry - sig.StopLoss) / sig.Entry * 100

	var sb strings.Builder

	fmt.Fprintf(&sb, "🚨 СИГНАЛ: %s %s\n\n", pair, direction)
	fmt.Fprintf(&sb, "📊 Технічний аналіз (%s):\n", formatTimeframe(sig.Timeframe))
	fmt.Fprintf(&sb, "• Ціна: $%s (нижче MA60: $%s)\n",
		formatPrice(sig.Entry), formatPrice(sig.Indicators.MA.MA60))
	fmt.Fprintf(&sb, "• RSI: %s\n", rsiStr)
	fmt.Fprintf(&sb, "• Об'єм: %s\n", volumeStr)
	fmt.Fprintf(&sb, "• MA10: $%s | MA20: $%s | MA60: $%s\n\n",
		formatPrice(sig.Indicators.MA.MA10),
		formatPrice(sig.Indicators.MA.MA20),
		formatPrice(sig.Indicators.MA.MA60),
	)

	fmt.Fprintf(&sb, "🎯 Параметри угоди:\n")
	fmt.Fprintf(&sb, "• Вхід: $%s\n", formatPrice(sig.Entry))
	fmt.Fprintf(&sb, "• Стоп: $%s (%.1f%%)\n", formatPrice(sig.StopLoss), stopPct)

	for i, tp := range sig.TakeProfits {
		tpPct := (tp - sig.Entry) / sig.Entry * 100
		fmt.Fprintf(&sb, "• TP%d: $%s (+%.1f%%)\n", i+1, formatPrice(tp), tpPct)
	}
	fmt.Fprintf(&sb, "• R:R: %.1f:1\n\n", sig.RRRatio)

	fmt.Fprintf(&sb, "💰 Розмір позиції (%.0f%% ризику від $%.0f):\n",
		f.riskPct, f.totalCapital)
	fmt.Fprintf(&sb, "• Ризик: $%.0f\n", riskUSD)
	fmt.Fprintf(&sb, "• Кількість: %.0f %s\n", qty, baseAsset(sig.Symbol))
	fmt.Fprintf(&sb, "• Маржа (%dx): $%.0f\n\n", f.leverage, margin)

	_ = riskPct // used implicitly above
	fmt.Fprintf(&sb, "⏰ Таймфрейм: %s | Стратегія: %s",
		formatTimeframe(sig.Timeframe), sig.Strategy)

	return sb.String()
}

func toDisplayPair(symbol string) string {
	if strings.HasSuffix(symbol, "USDT") {
		base := symbol[:len(symbol)-4]
		return base + "/USDT"
	}
	return symbol
}

func baseAsset(symbol string) string {
	if strings.HasSuffix(symbol, "USDT") {
		return symbol[:len(symbol)-4]
	}
	return symbol
}

func formatTimeframe(tf string) string {
	switch tf {
	case "60":
		return "1H"
	case "240":
		return "4H"
	case "D", "1D":
		return "1D"
	case "W":
		return "1W"
	default:
		return tf + "m"
	}
}

func formatRSI(val float64) string {
	s := fmt.Sprintf("%.0f", val)
	if val < 30 {
		return s + " (перепроданість)"
	}
	if val < 40 {
		return s + " (близько до перепроданості)"
	}
	if val > 70 {
		return s + " (перекупленість)"
	}
	return s
}

// formatPrice formats a price with an appropriate number of decimal places.
func formatPrice(p float64) string {
	if p == 0 {
		return "0"
	}
	abs := math.Abs(p)
	switch {
	case abs >= 10000:
		return fmt.Sprintf("%.0f", p)
	case abs >= 1000:
		return fmt.Sprintf("%.1f", p)
	case abs >= 1:
		return fmt.Sprintf("%.4f", p)
	default:
		return fmt.Sprintf("%.6f", p)
	}
}
