package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Trade sync mirrors real closed trades from Bybit (/v5/position/closed-pnl)
// into the positions table, which feeds the webapp (history, stats, equity).
// Bybit keeps closed-pnl history for 2 years max, so we persist it locally.

const (
	tradeSyncBackfill = 14 * 24 * time.Hour // history pulled on startup
	tradeSyncInterval = 5 * time.Minute
	tradeSyncWindow   = 25 * time.Hour // periodic re-scan; overlap is deduped by order_id
	bybitMaxSpan      = 7 * 24 * time.Hour
)

// Schema bootstrap runs in code (like paper.go) because the SQL migrations in
// /migrations only apply on a fresh DB init.
var tradeSyncSchema = []string{
	`ALTER TABLE positions ADD COLUMN IF NOT EXISTS order_id TEXT`,
	`ALTER TABLE positions ADD COLUMN IF NOT EXISTS qty FLOAT8`,
	`ALTER TABLE positions ADD COLUMN IF NOT EXISTS pnl FLOAT8`,
	`CREATE UNIQUE INDEX IF NOT EXISTS positions_order_id_uniq ON positions (order_id)`,
}

type tradeSync struct {
	pool  *pgxpool.Pool
	bybit *BybitClient
}

func runTradeSync(ctx context.Context, pool *pgxpool.Pool, bybit *BybitClient) {
	ts := &tradeSync{pool: pool, bybit: bybit}

	for _, stmt := range tradeSyncSchema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			log.Printf("[tradesync] schema: %v", err)
			return
		}
	}

	if n, err := ts.syncRange(ctx, tradeSyncBackfill); err != nil {
		log.Printf("[tradesync] backfill: %v", err)
	} else {
		log.Printf("[tradesync] backfill %s: %d new closed trades", tradeSyncBackfill, n)
	}

	tick := time.NewTicker(tradeSyncInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if n, err := ts.syncRange(ctx, tradeSyncWindow); err != nil {
				log.Printf("[tradesync] sync: %v", err)
			} else if n > 0 {
				log.Printf("[tradesync] %d new closed trades", n)
			}
		}
	}
}

// syncRange upserts all closed trades within [now-span, now], walking the
// range in 7-day windows (Bybit's per-request limit) with cursor pagination.
func (ts *tradeSync) syncRange(ctx context.Context, span time.Duration) (int, error) {
	end := time.Now()
	start := end.Add(-span)
	total := 0
	for winEnd := end; winEnd.After(start); winEnd = winEnd.Add(-bybitMaxSpan) {
		winStart := winEnd.Add(-bybitMaxSpan)
		if winStart.Before(start) {
			winStart = start
		}
		cursor := ""
		for {
			recs, next, err := ts.bybit.ClosedPnlRange(winStart.UnixMilli(), winEnd.UnixMilli(), cursor)
			if err != nil {
				return total, err
			}
			for _, r := range recs {
				inserted, err := ts.upsert(ctx, r)
				if err != nil {
					return total, err
				}
				total += inserted
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return total, nil
}

func (ts *tradeSync) upsert(ctx context.Context, r ClosedPnlRecord) (int, error) {
	if r.OrderID == "" {
		return 0, nil
	}
	tag, err := ts.pool.Exec(ctx, `
		INSERT INTO positions
			(symbol, direction, entry, stop_loss, opened_at, closed_at,
			 close_price, pnl_pct, status, order_id, qty, pnl)
		VALUES ($1,$2,$3,0,$4,$5,$6,$7,'closed',$8,$9,$10)
		ON CONFLICT (order_id) DO NOTHING`,
		r.Symbol, inferDirection(r), r.EntryPrice, r.CreatedAt, r.ClosedAt,
		r.ExitPrice, pnlPct(r), r.OrderID, r.Qty, r.ClosedPnl)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// pnlPct is the ROE: closed PnL relative to the position margin.
func pnlPct(r ClosedPnlRecord) float64 {
	if r.Leverage > 0 && r.CumEntryValue > 0 {
		return r.ClosedPnl / (r.CumEntryValue / r.Leverage) * 100
	}
	if r.EntryPrice > 0 && r.Qty > 0 {
		return r.ClosedPnl / (r.EntryPrice * r.Qty) * 100
	}
	return 0
}

// inferDirection derives the position side from the price move vs PnL sign;
// when the move is ambiguous it falls back to the closing-order side
// (a long is closed by a Sell order).
func inferDirection(r ClosedPnlRecord) string {
	if r.EntryPrice > 0 && r.ExitPrice != r.EntryPrice && r.ClosedPnl != 0 {
		if (r.ExitPrice > r.EntryPrice) == (r.ClosedPnl > 0) {
			return "long"
		}
		return "short"
	}
	if r.Side == "Sell" {
		return "long"
	}
	return "short"
}
