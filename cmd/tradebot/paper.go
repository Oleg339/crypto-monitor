package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── Schema ────────────────────────────────────────────────────────────────────

const createSignalsTableSQL = `
CREATE TABLE IF NOT EXISTS received_signals (
    id          SERIAL PRIMARY KEY,
    signal_id   INTEGER     NOT NULL,
    symbol      TEXT        NOT NULL,
    direction   TEXT        NOT NULL,
    entry_low   FLOAT8      NOT NULL,
    entry_high  FLOAT8      NOT NULL,
    sl          FLOAT8      NOT NULL,
    tps         TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status      TEXT        NOT NULL DEFAULT 'pending'
);`

const createTableSQL = `
CREATE TABLE IF NOT EXISTS paper_trades (
    id          SERIAL PRIMARY KEY,
    signal_id   TEXT        NOT NULL DEFAULT '',
    symbol      TEXT        NOT NULL,
    side        TEXT        NOT NULL DEFAULT 'Buy',
    entry_price FLOAT8      NOT NULL,
    qty         FLOAT8      NOT NULL,
    margin      FLOAT8      NOT NULL,
    leverage    FLOAT8      NOT NULL,
    sl          FLOAT8      NOT NULL,
    tp          FLOAT8      NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'open',
    open_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    close_at    TIMESTAMPTZ,
    close_price FLOAT8,
    pnl         FLOAT8
);`

// ── Types ─────────────────────────────────────────────────────────────────────

type PaperTrade struct {
	ID         int64
	SignalID   string
	Symbol     string
	Side       string
	EntryPrice float64
	Qty        float64
	Margin     float64
	Leverage   float64
	SL         float64
	TP         float64
	Status     string // open | tp_hit | sl_hit | ttl_expired
	OpenAt     time.Time
	CloseAt    *time.Time
	ClosePrice *float64
	PnL        *float64
}

// UnrealisedPnL calculates unrealised PnL given current mark price (LONG only).
func (t *PaperTrade) UnrealisedPnL(markPrice float64) float64 {
	return (markPrice - t.EntryPrice) * t.Qty
}

type PaperStats struct {
	Total     int
	Wins      int
	Losses    int
	WinRate   float64
	AvgPnL    float64
	TotalPnL  float64
	OpenCount int
}

// ── Store ─────────────────────────────────────────────────────────────────────

type PaperStore struct {
	pool *pgxpool.Pool
}

// ── ReceivedSignal ────────────────────────────────────────────────────────────

type ReceivedSignal struct {
	ID         int64
	SignalID   int
	Symbol     string
	Direction  string
	EntryLow   float64
	EntryHigh  float64
	SL         float64
	TPs        []float64
	ReceivedAt time.Time
	Status     string // pending | confirmed | rejected
}

func newPaperStore(ctx context.Context, dsn string) (*PaperStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgx pool: %w", err)
	}
	if _, err = pool.Exec(ctx, createSignalsTableSQL); err != nil {
		return nil, fmt.Errorf("create signals table: %w", err)
	}
	if _, err = pool.Exec(ctx, createTableSQL); err != nil {
		return nil, fmt.Errorf("create trades table: %w", err)
	}
	return &PaperStore{pool: pool}, nil
}

func (s *PaperStore) SaveSignal(ctx context.Context, sig *ParsedSignal) (int64, error) {
	tpsJSON, _ := json.Marshal(sig.TPs)
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO received_signals (signal_id, symbol, direction, entry_low, entry_high, sl, tps)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		sig.ID, sig.Symbol, sig.Direction, sig.EntryLow, sig.EntryHigh, sig.SL, string(tpsJSON),
	).Scan(&id)
	return id, err
}

func (s *PaperStore) UpdateSignalStatus(ctx context.Context, id int64, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE received_signals SET status=$1 WHERE id=$2`, status, id)
	return err
}

func (s *PaperStore) FindSignalByTraderID(ctx context.Context, traderSignalID int) (*ReceivedSignal, error) {
	var r ReceivedSignal
	var tpsStr string
	err := s.pool.QueryRow(ctx, `
		SELECT id, signal_id, symbol, direction, entry_low, entry_high, sl, tps, received_at, status
		FROM received_signals WHERE signal_id=$1
		ORDER BY received_at DESC LIMIT 1`,
		traderSignalID,
	).Scan(&r.ID, &r.SignalID, &r.Symbol, &r.Direction,
		&r.EntryLow, &r.EntryHigh, &r.SL, &tpsStr,
		&r.ReceivedAt, &r.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tpsStr), &r.TPs)
	return &r, nil
}

func (s *PaperStore) RecentSignals(ctx context.Context, hours int) ([]*ReceivedSignal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, signal_id, symbol, direction, entry_low, entry_high, sl, tps, received_at, status
		FROM received_signals
		WHERE received_at >= NOW() - ($1 || ' hours')::interval
		ORDER BY received_at DESC`,
		fmt.Sprintf("%d", hours),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReceivedSignal
	for rows.Next() {
		var r ReceivedSignal
		var tpsStr string
		if err := rows.Scan(&r.ID, &r.SignalID, &r.Symbol, &r.Direction,
			&r.EntryLow, &r.EntryHigh, &r.SL, &tpsStr,
			&r.ReceivedAt, &r.Status); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tpsStr), &r.TPs)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *PaperStore) OpenTrade(ctx context.Context, sig *pendingSignal) (*PaperTrade, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO paper_trades (symbol, side, entry_price, qty, margin, leverage, sl, tp)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		sig.Symbol, sig.Side, sig.EntryMid,
		sig.Qty, sig.Margin, sig.Leverage, sig.SL, sig.TP,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetTrade(ctx, id)
}

func (s *PaperStore) CloseTrade(ctx context.Context, id int64, status string, closePrice float64) error {
	// pnl = (close_price - entry_price) * qty  (for LONG)
	_, err := s.pool.Exec(ctx, `
		UPDATE paper_trades
		SET status      = $1,
		    close_at    = NOW(),
		    close_price = $2,
		    pnl         = (($2 - entry_price) * qty)
		WHERE id = $3 AND status = 'open'`,
		status, closePrice, id)
	return err
}

func (s *PaperStore) GetTrade(ctx context.Context, id int64) (*PaperTrade, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id,signal_id,symbol,side,entry_price,qty,margin,leverage,sl,tp,
		       status,open_at,close_at,close_price,pnl
		FROM paper_trades WHERE id=$1`, id)
	return scanTrade(row)
}

func (s *PaperStore) RecentClosedTrades(ctx context.Context, n int) ([]*PaperTrade, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,signal_id,symbol,side,entry_price,qty,margin,leverage,sl,tp,
		       status,open_at,close_at,close_price,pnl
		FROM paper_trades
		WHERE status <> 'open'
		ORDER BY close_at DESC
		LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PaperTrade
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PaperStore) OpenTrades(ctx context.Context) ([]*PaperTrade, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,signal_id,symbol,side,entry_price,qty,margin,leverage,sl,tp,
		       status,open_at,close_at,close_price,pnl
		FROM paper_trades WHERE status='open' ORDER BY open_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PaperTrade
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PaperStore) Stats(ctx context.Context) (*PaperStats, error) {
	var stats PaperStats

	// Closed trades stats
	err := s.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE status <> 'open') AS total,
		    COUNT(*) FILTER (WHERE pnl > 0)          AS wins,
		    COUNT(*) FILTER (WHERE pnl <= 0)         AS losses,
		    COALESCE(AVG(pnl) FILTER (WHERE status <> 'open'), 0) AS avg_pnl,
		    COALESCE(SUM(pnl) FILTER (WHERE status <> 'open'), 0) AS total_pnl,
		    COUNT(*) FILTER (WHERE status = 'open')  AS open_count
		FROM paper_trades`,
	).Scan(&stats.Total, &stats.Wins, &stats.Losses,
		&stats.AvgPnL, &stats.TotalPnL, &stats.OpenCount)
	if err != nil {
		return nil, err
	}
	if stats.Total > 0 {
		stats.WinRate = float64(stats.Wins) / float64(stats.Total) * 100
	}
	return &stats, nil
}

// ── Scanner ───────────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanTrade(row scanner) (*PaperTrade, error) {
	var t PaperTrade
	err := row.Scan(
		&t.ID, &t.SignalID, &t.Symbol, &t.Side,
		&t.EntryPrice, &t.Qty, &t.Margin, &t.Leverage,
		&t.SL, &t.TP, &t.Status, &t.OpenAt,
		&t.CloseAt, &t.ClosePrice, &t.PnL,
	)
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	return &t, nil
}

// ── Formatting helpers ────────────────────────────────────────────────────────

func formatPaperConfirm(t *PaperTrade) string {
	base := strings.TrimSuffix(t.Symbol, "USDT")
	return fmt.Sprintf(
		"📝 <b>[PAPER] Позиция открыта</b>\n\n"+
			"%s/USDT LONG  qty=<code>%g</code> @ <code>$%g</code>\n"+
			"SL: <code>$%g</code>  TP: <code>$%g</code>\n"+
			"Маржа: <code>$%.2f</code>  Плечо: <code>%.0fx</code>\n\n"+
			"<i>ID: %d</i>",
		base, t.Qty, t.EntryPrice,
		t.SL, t.TP, t.Margin, t.Leverage, t.ID,
	)
}

func formatPaperClose(t *PaperTrade, closePrice float64, status string) string {
	base := strings.TrimSuffix(t.Symbol, "USDT")
	pnl := (closePrice - t.EntryPrice) * t.Qty
	sign := "+"
	emoji := "✅"
	if pnl < 0 {
		sign = ""
		emoji = "❌"
	}
	label := "TP hit"
	if status == "sl_hit" {
		label = "SL hit"
	}
	return fmt.Sprintf(
		"%s <b>[PAPER] %s — %s/USDT</b>\n\n"+
			"Закрыто @ <code>$%g</code>\n"+
			"PnL: <code>%s$%.2f</code>\n"+
			"<i>Trade ID: %d</i>",
		emoji, label, base,
		closePrice, sign, pnl, t.ID,
	)
}
