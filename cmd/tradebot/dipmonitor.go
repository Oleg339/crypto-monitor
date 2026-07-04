package main

// ── Монитор просадки BTC ─────────────────────────────────────────────────────
// Бэктест по всем сигналам канала (78 шт, март–июль 2026) показал: закрытие
// открытых позиций при просадке BTC глубже порога от 10-дневного максимума и
// перезаход лимитником по цене старого входа на отбое улучшает итог во всех
// подпериодах (Σ +193% → +306%), включая падающий июнь (+5% → +46%).
// Полуручной режим: бот предлагает действия кнопками; авторежим — сам.

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// btcDistFrom10dHigh — дистанция BTC до его 10-дневного максимума, % (≤ 0).
func (b *Bot) btcDistFrom10dHigh() (float64, error) {
	closes, err := b.bybit.DailyCloses("BTCUSDT", 11)
	if err != nil {
		return 0, err
	}
	if len(closes) < 3 {
		return 0, fmt.Errorf("мало данных: %d свечей", len(closes))
	}
	last := closes[len(closes)-1]
	mx := closes[0]
	for _, c := range closes {
		if c > mx {
			mx = c
		}
	}
	return (last/mx - 1) * 100, nil
}

func (b *Bot) RunDipMonitor(ctx context.Context) {
	log.Printf("[dip] monitor started: порог −%.0f%% от 10-дневного максимума BTC", b.cfg.BTCDipExitPct)
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.dipTick(ctx)
		}
	}
}

// dipTick — переходы состояния «просадка BTC» (хранится в БД, переживает рестарт).
func (b *Bot) dipTick(ctx context.Context) {
	dist, err := b.btcDistFrom10dHigh()
	if err != nil {
		log.Printf("[dip] btc check: %v", err)
		return
	}
	active := dist <= -b.cfg.BTCDipExitPct
	prev, err := b.paper.GetSetting(ctx, "btc_dip_active")
	if err != nil {
		log.Printf("[dip] read state: %v", err)
		return
	}
	switch {
	case active && prev != "1":
		if err := b.paper.SetSetting(ctx, "btc_dip_active", "1"); err != nil {
			log.Printf("[dip] save state: %v", err)
			return
		}
		b.onDipStart(ctx, dist)
	case !active && prev == "1":
		if err := b.paper.SetSetting(ctx, "btc_dip_active", "0"); err != nil {
			log.Printf("[dip] save state: %v", err)
			return
		}
		b.onDipEnd(ctx, dist)
	}
}

func (b *Bot) onDipStart(ctx context.Context, dist float64) {
	log.Printf("[dip] триггер: BTC %.2f%% от 10-дневного максимума", dist)
	positions, err := b.bybit.Positions()
	if err != nil {
		b.send(b.chatID, 0, fmt.Sprintf(
			"⚠️ BTC %.1f%% от 10-дневного максимума, но не смог получить позиции: <code>%s</code>",
			dist, esc(err.Error())), nil)
		return
	}
	if len(positions) == 0 {
		log.Printf("[dip] открытых позиций нет — нечего закрывать")
		return
	}

	if b.IsAuto() {
		n, pnl := b.dipCloseAll(ctx)
		b.send(b.chatID, 0, fmt.Sprintf(
			"⚠️ <b>BTC %.1f%% от 10-дневного максимума</b>\n\n"+
				"🤖 Авторежим закрыл позиции: %d шт (нереализ. PnL %+.2f$).\n"+
				"На отбое перевыставлю лимитники по ценам старых входов.", dist, n, pnl), nil)
		return
	}

	var sb strings.Builder
	for _, p := range positions {
		fmt.Fprintf(&sb, "\n• %s %s  %+.2f$",
			strings.TrimSuffix(p.Symbol, "USDT"), strings.ToUpper(p.Side), p.UnrealisedPnl)
	}
	kb := inlineKB([][]kbBtn{{
		{Text: fmt.Sprintf("📉 Закрыть %d поз.", len(positions)), Data: "dipclose:x"},
		{Text: "Игнор", Data: "dipignore:x"},
	}})
	b.send(b.chatID, 0, fmt.Sprintf(
		"⚠️ <b>BTC %.1f%% от 10-дневного максимума</b> — зона, где лонги чаще всего едут в стоп.\n"+
			"По бэктесту выгоднее закрыть сейчас и перезайти на отбое (Σ +193%%→+306%%).\n%s\n\n"+
			"Закрыть и ждать отбоя?", dist, sb.String()), kb)
}

// dipCloseAll закрывает все позиции по рынку и паркует их сетапы для перезахода.
func (b *Bot) dipCloseAll(ctx context.Context) (int, float64) {
	positions, err := b.bybit.Positions()
	if err != nil {
		log.Printf("[dip] positions: %v", err)
		return 0, 0
	}
	n, pnl := 0, 0.0
	for _, p := range positions {
		closeSide := "Sell"
		if p.Side == "Sell" {
			closeSide = "Buy"
		}
		qtyStr := fmt.Sprintf("%g", p.Size)
		if info, err := b.bybit.InstrumentInfo(p.Symbol); err == nil {
			qtyStr = ff(roundStep(p.Size, info.QtyStep), decimals(info.QtyStep))
		}
		if err := b.bybit.CloseMarket(p.Symbol, closeSide, qtyStr); err != nil {
			log.Printf("[dip] close %s: %v", p.Symbol, err)
			b.send(b.chatID, 0, fmt.Sprintf("❌ Не смог закрыть %s: <code>%s</code>", p.Symbol, esc(err.Error())), nil)
			continue
		}
		if err := b.paper.ParkSetup(ctx, &ParkedSetup{
			Symbol: p.Symbol, Side: p.Side, Qty: p.Size,
			EntryPrice: p.EntryPrice, SL: p.StopLoss, TP: p.TakeProfit,
		}); err != nil {
			log.Printf("[dip] park %s: %v", p.Symbol, err)
		}
		log.Printf("[dip] closed %s %s qty=%s unrealised=%.2f", p.Symbol, p.Side, qtyStr, p.UnrealisedPnl)
		n++
		pnl += p.UnrealisedPnl
	}
	return n, pnl
}

// lowSince — минимальный дневной low символа с момента since (приближение по
// дневным свечам). При ошибке данных возвращает +Inf: сетап считаем живым,
// финальную защиту всё равно даёт SL самого перезахода.
func (b *Bot) lowSince(symbol string, since time.Time) float64 {
	days := int(time.Since(since).Hours()/24) + 2
	bars, err := b.bybit.DailyBars(symbol, days)
	if err != nil || len(bars) == 0 {
		log.Printf("[dip] lowSince %s: %v", symbol, err)
		return math.MaxFloat64
	}
	low := math.MaxFloat64
	for _, bar := range bars {
		if bar.Low > 0 && bar.Low < low {
			low = bar.Low
		}
	}
	return low
}

func (b *Bot) onDipEnd(ctx context.Context, dist float64) {
	log.Printf("[dip] отбой: BTC %.2f%% от 10-дневного максимума", dist)
	parked, err := b.paper.ParkedSetups(ctx)
	if err != nil {
		log.Printf("[dip] parked: %v", err)
		return
	}
	if len(parked) == 0 {
		return
	}

	// сетапы, где цена за время парковки пробила SL, — мертвы (как в бэктесте)
	var alive []*ParkedSetup
	var dead []string
	for _, p := range parked {
		if p.SL > 0 && b.lowSince(p.Symbol, p.ParkedAt) <= p.SL {
			if err := b.paper.SetParkedStatus(ctx, p.ID, "dead"); err != nil {
				log.Printf("[dip] mark dead %s: %v", p.Symbol, err)
			}
			dead = append(dead, strings.TrimSuffix(p.Symbol, "USDT"))
			continue
		}
		alive = append(alive, p)
	}
	deadNote := ""
	if len(dead) > 0 {
		deadNote = "\nБез перезахода (SL был пробит): " + strings.Join(dead, ", ")
	}

	if b.IsAuto() {
		n := b.dipReenterAll(ctx)
		b.send(b.chatID, 0, fmt.Sprintf(
			"📈 <b>BTC отбился</b> (%.1f%% от максимума)\n\n"+
				"🤖 Авторежим перевыставил %d лимитников по ценам старых входов.%s", dist, n, deadNote), nil)
		return
	}
	if len(alive) == 0 {
		b.send(b.chatID, 0, "📈 <b>BTC отбился</b>, но перезаходить не во что."+deadNote, nil)
		return
	}
	var sb strings.Builder
	for _, p := range alive {
		fmt.Fprintf(&sb, "\n• %s %s @ %g", strings.TrimSuffix(p.Symbol, "USDT"), strings.ToUpper(p.Side), p.EntryPrice)
	}
	kb := inlineKB([][]kbBtn{{
		{Text: fmt.Sprintf("📈 Перезайти (%d)", len(alive)), Data: "dipreenter:x"},
		{Text: "Не надо", Data: "dipdrop:x"},
	}})
	b.send(b.chatID, 0, fmt.Sprintf(
		"📈 <b>BTC отбился</b> (%.1f%% от максимума) — можно перезайти лимитниками по ценам старых входов:%s%s",
		dist, sb.String(), deadNote), kb)
}

// dipReenterAll перевыставляет лимитники по припаркованным сетапам.
func (b *Bot) dipReenterAll(ctx context.Context) int {
	parked, err := b.paper.ParkedSetups(ctx)
	if err != nil {
		log.Printf("[dip] parked: %v", err)
		return 0
	}
	n := 0
	for _, p := range parked {
		// свежая проверка SL: между сообщением и нажатием кнопки проходит время
		if p.SL > 0 && b.lowSince(p.Symbol, p.ParkedAt) <= p.SL {
			b.paper.SetParkedStatus(ctx, p.ID, "dead")
			continue
		}
		info, err := b.bybit.InstrumentInfo(p.Symbol)
		if err != nil {
			log.Printf("[dip] info %s: %v", p.Symbol, err)
			continue
		}
		pp, qp := decimals(info.TickSize), decimals(info.QtyStep)
		_, err = b.bybit.PlaceOrder(p.Symbol, p.Side,
			ff(roundStep(p.Qty, info.QtyStep), qp),
			ff(p.EntryPrice, pp), ff(p.SL, pp), ff(p.TP, pp))
		if err != nil {
			log.Printf("[dip] reenter %s: %v", p.Symbol, err)
			b.send(b.chatID, 0, fmt.Sprintf("❌ Перезаход %s: <code>%s</code>", p.Symbol, esc(err.Error())), nil)
			continue
		}
		if err := b.paper.SetParkedStatus(ctx, p.ID, "reentered"); err != nil {
			log.Printf("[dip] mark reentered %s: %v", p.Symbol, err)
		}
		log.Printf("[dip] reentered %s %s qty=%g @ %g", p.Symbol, p.Side, p.Qty, p.EntryPrice)
		n++
	}
	return n
}
