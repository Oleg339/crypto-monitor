package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// runLiveTTL checks Bybit open orders every hour and cancels ones older than ORDER_TTL_DAYS.
func runLiveTTL(ctx context.Context, bybit *BybitClient, bot *Bot, cfg *Config) {
	ttl := time.Duration(cfg.OrderTTLDays * float64(24*time.Hour))
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	check := func() {
		orders, err := bybit.OpenOrders()
		if err != nil {
			log.Printf("[ttl] open orders: %v", err)
			return
		}
		now := time.Now()
		for _, o := range orders {
			age := now.Sub(o.CreatedTime)
			if age < ttl {
				continue
			}
			if err := bybit.CancelOrder(o.Symbol, o.OrderID); err != nil {
				log.Printf("[ttl] cancel %s %s: %v", o.Symbol, o.OrderID, err)
				continue
			}
			base := strings.TrimSuffix(o.Symbol, "USDT")
			log.Printf("[ttl] cancelled order %s %s (age %.1fd)", o.Symbol, o.OrderID, age.Hours()/24)
			bot.send(bot.chatID, 0, fmt.Sprintf(
				"⏰ <b>Ордер отменён по TTL — %s/USDT</b>\n\n"+
					"Ордер висел <b>%.1f дней</b> без исполнения (ORDER_TTL_DAYS=%.0f)\n"+
					"%s %s @ <code>$%.6g</code>  qty=<code>%g</code>\n"+
					"<code>%s</code>",
				base,
				age.Hours()/24, cfg.OrderTTLDays,
				o.Side, base, o.Price, o.Qty,
				o.OrderID,
			), nil)
		}
	}

	check() // run once immediately on start
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
