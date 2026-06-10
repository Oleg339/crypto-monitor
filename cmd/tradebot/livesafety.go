package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// liveSafety is the live-mode counterpart of SafetyChecker (paper mode):
// it pauses trading based on real closed trades synced from Bybit into the
// positions table. Winrate/streak checks run after each sync that found new
// closed trades; the equity check runs on every sync pass.

type liveSafety struct {
	pool        *pgxpool.Pool
	bot         *Bot
	bybit       *BybitClient
	cfg         *Config
	streakLimit int // LOSS_STREAK_PAUSE, 0 = off
}

func newLiveSafety(pool *pgxpool.Pool, bot *Bot, bybit *BybitClient, cfg *Config) *liveSafety {
	return &liveSafety{
		pool:        pool,
		bot:         bot,
		bybit:       bybit,
		cfg:         cfg,
		streakLimit: int(getenvF("LOSS_STREAK_PAUSE", 4)),
	}
}

// runChecks is called from the trade sync loop. newTrades tells whether the
// pass found newly closed trades (trade-based rules only fire then).
func (ls *liveSafety) runChecks(ctx context.Context, newTrades bool) {
	if ls.bot.IsPaused() {
		return
	}
	if newTrades {
		ls.checkLossStreak(ctx)
		ls.checkRollingWinRate(ctx)
	}
	ls.checkMinEquity()
}

// recentResults returns win/loss flags of the latest closed trades, newest first.
func (ls *liveSafety) recentResults(ctx context.Context, n int) ([]bool, error) {
	rows, err := ls.pool.Query(ctx, `
		SELECT pnl > 0
		FROM positions
		WHERE status = 'closed' AND pnl IS NOT NULL
		ORDER BY closed_at DESC
		LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bool
	for rows.Next() {
		var win bool
		if err := rows.Scan(&win); err != nil {
			return nil, err
		}
		out = append(out, win)
	}
	return out, rows.Err()
}

func (ls *liveSafety) checkLossStreak(ctx context.Context) {
	if ls.streakLimit <= 0 {
		return
	}
	results, err := ls.recentResults(ctx, ls.streakLimit)
	if err != nil {
		log.Printf("[livesafety] loss streak: %v", err)
		return
	}
	if len(results) < ls.streakLimit {
		return
	}
	for _, win := range results {
		if win {
			return
		}
	}
	ls.pause(fmt.Sprintf("%d убыточных сделок подряд (по данным Bybit)", ls.streakLimit), false)
}

func (ls *liveSafety) checkRollingWinRate(ctx context.Context) {
	results, err := ls.recentResults(ctx, rollingWindow)
	if err != nil {
		log.Printf("[livesafety] rolling winrate: %v", err)
		return
	}
	if len(results) < rollingWindow {
		return // мало данных
	}
	wins := 0
	for _, win := range results {
		if win {
			wins++
		}
	}
	wr := float64(wins) / float64(len(results)) * 100
	if wr < 30.0 {
		ls.pause(fmt.Sprintf("WinRate последних %d реальных сделок упал до %.0f%%", rollingWindow, wr), false)
	}
}

func (ls *liveSafety) checkMinEquity() {
	if ls.cfg.MinEquity <= 0 {
		return
	}
	wi, err := ls.bybit.Balance()
	if err != nil {
		log.Printf("[livesafety] equity check: %v", err)
		return
	}
	if wi.TotalEquity < ls.cfg.MinEquity {
		ls.pause(fmt.Sprintf("Баланс упал до $%.2f (MIN_EQUITY=$%.2f)", wi.TotalEquity, ls.cfg.MinEquity), true)
	}
}

func (ls *liveSafety) pause(reason string, hard bool) {
	if ls.bot.IsPaused() {
		return
	}
	ls.bot.Pause(reason)
	if hard {
		log.Printf("[livesafety] СТОП (капитал): %s", reason)
		ls.bot.send(ls.bot.chatID, 0,
			fmt.Sprintf("🛑 <b>Бот остановлен</b>\n\n%s\n\n"+
				"Пополни счёт и используй /resume.", esc(reason)), nil)
		return
	}
	log.Printf("[livesafety] ПАУЗА: %s", reason)
	ls.bot.send(ls.bot.chatID, 0,
		fmt.Sprintf("⚠️ <b>Бот на паузе — новые сделки не открываются</b>\n\n%s\n\n"+
			"Открытые позиции не тронуты (SL/TP работают на бирже).\n"+
			"Используй /resume после проверки системы.", esc(reason)), nil)
}
