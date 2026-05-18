package main

import (
	"context"
	"fmt"
	"log"
	"strings"
)

const rollingWindow = 10

// SafetyChecker runs after each paper trade closes and on equity checks.
type SafetyChecker struct {
	store *PaperStore
	bot   *Bot
	bybit *BybitClient
	cfg   *Config
}

func newSafetyChecker(store *PaperStore, bot *Bot, bybit *BybitClient, cfg *Config) *SafetyChecker {
	return &SafetyChecker{store: store, bot: bot, bybit: bybit, cfg: cfg}
}

// RunAfterClose is called every time a paper trade is closed.
func (s *SafetyChecker) RunAfterClose(ctx context.Context) {
	s.checkRollingWinRate(ctx)
	s.checkBacktestDeviation(ctx)
	s.checkMinEquity(ctx)
}

// CheckEquityOnly is called periodically (no trade needed).
func (s *SafetyChecker) CheckEquityOnly(ctx context.Context) {
	s.checkMinEquity(ctx)
}

// ── Rule 1: Rolling WinRate ───────────────────────────────────────────────────

func (s *SafetyChecker) checkRollingWinRate(ctx context.Context) {
	trades, err := s.store.RecentClosedTrades(ctx, rollingWindow)
	if err != nil {
		log.Printf("[safety] recent trades: %v", err)
		return
	}
	if len(trades) < rollingWindow {
		return // не хватает данных
	}

	wins := 0
	for _, t := range trades {
		if t.PnL != nil && *t.PnL > 0 {
			wins++
		}
	}
	wr := float64(wins) / float64(len(trades)) * 100

	if wr < 30.0 {
		reason := fmt.Sprintf("WinRate последних %d сделок упал до %.0f%%", rollingWindow, wr)
		s.pause(ctx, reason)
	}
}

// ── Rule 2: Backtest deviation every 20 trades ────────────────────────────────

func (s *SafetyChecker) checkBacktestDeviation(ctx context.Context) {
	if s.cfg.BacktestWinRate <= 0 {
		return
	}

	stats, err := s.store.Stats(ctx)
	if err != nil || stats.Total == 0 || stats.Total%20 != 0 {
		return
	}

	var issues []string

	if s.cfg.BacktestWinRate > 0 {
		wrDrop := (s.cfg.BacktestWinRate - stats.WinRate) / s.cfg.BacktestWinRate
		if wrDrop >= 0.30 {
			issues = append(issues, fmt.Sprintf(
				"WinRate %.1f%% vs бэктест %.1f%% (хуже на %.0f%%)",
				stats.WinRate, s.cfg.BacktestWinRate, wrDrop*100))
		}
	}

	if s.cfg.BacktestAvgPnL > 0 {
		pnlDrop := (s.cfg.BacktestAvgPnL - stats.AvgPnL) / s.cfg.BacktestAvgPnL
		if pnlDrop >= 0.30 {
			issues = append(issues, fmt.Sprintf(
				"AvgPnL $%.2f vs бэктест $%.2f (хуже на %.0f%%)",
				stats.AvgPnL, s.cfg.BacktestAvgPnL, pnlDrop*100))
		}
	}

	if len(issues) == 0 {
		return
	}

	reason := fmt.Sprintf("Реальные показатели хуже бэктеста на 30%%+ за %d сделок:\n• %s",
		stats.Total, strings.Join(issues, "\n• "))
	s.pause(ctx, reason)
}

// ── Rule 3: Capital stop-loss ─────────────────────────────────────────────────

func (s *SafetyChecker) checkMinEquity(ctx context.Context) {
	if s.cfg.MinEquity <= 0 {
		return
	}

	equity, err := s.currentEquity(ctx)
	if err != nil {
		log.Printf("[safety] equity check: %v", err)
		return
	}

	if equity < s.cfg.MinEquity {
		reason := fmt.Sprintf("Баланс упал до $%.2f (MIN_EQUITY=$%.2f)", equity, s.cfg.MinEquity)
		s.pauseHard(ctx, reason)
	}
}

func (s *SafetyChecker) currentEquity(ctx context.Context) (float64, error) {
	if s.cfg.PaperTrading {
		stats, err := s.store.Stats(ctx)
		if err != nil {
			return 0, err
		}
		return s.cfg.PaperInitialEquity + stats.TotalPnL, nil
	}
	wi, err := s.bybit.Balance()
	if err != nil {
		return 0, err
	}
	return wi.TotalEquity, nil
}

// ── Pause helpers ─────────────────────────────────────────────────────────────

// pause stops new signals but allows /resume.
func (s *SafetyChecker) pause(ctx context.Context, reason string) {
	if s.bot.IsPaused() {
		return // уже на паузе
	}
	s.bot.Pause(reason)
	log.Printf("[safety] ПАУЗA: %s", reason)
	s.bot.send(s.bot.chatID, 0,
		fmt.Sprintf("⚠️ <b>Бот на паузе</b>\n\n%s\n\n"+
			"Используй /resume для возобновления", esc(reason)), nil)
}

// pauseHard stops bot completely (no /resume — equity too low).
func (s *SafetyChecker) pauseHard(ctx context.Context, reason string) {
	if s.bot.IsPaused() {
		return
	}
	s.bot.Pause(reason)
	log.Printf("[safety] СТОП (капитал): %s", reason)
	s.bot.send(s.bot.chatID, 0,
		fmt.Sprintf("🛑 <b>Бот остановлен</b>\n\n%s\n\n"+
			"Пополни счёт и перезапусти бота.", esc(reason)), nil)
}
