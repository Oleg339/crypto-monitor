package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type trade struct {
	ID         int64      `json:"id"`
	Symbol     string     `json:"symbol"`
	Direction  string     `json:"direction"`
	Entry      float64    `json:"entry"`
	ClosePrice *float64   `json:"closePrice"`
	PnlPct     *float64   `json:"pnlPct"`
	OpenedAt   time.Time  `json:"openedAt"`
	ClosedAt   *time.Time `json:"closedAt"`
	Status     string     `json:"status"`
}

func loadTrades(ctx context.Context, db *pgxpool.Pool, limit int) ([]trade, error) {
	const q = `
		SELECT id, symbol, direction, entry, close_price, pnl_pct, opened_at, closed_at, status
		FROM positions
		ORDER BY opened_at DESC
		LIMIT $1`
	rows, err := db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []trade{}
	for rows.Next() {
		var t trade
		if err := rows.Scan(&t.ID, &t.Symbol, &t.Direction, &t.Entry,
			&t.ClosePrice, &t.PnlPct, &t.OpenedAt, &t.ClosedAt, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type signal struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"time"`
	Symbol      string    `json:"symbol"`
	Strategy    string    `json:"strategy"`
	Direction   string    `json:"direction"`
	Entry       float64   `json:"entry"`
	StopLoss    float64   `json:"stopLoss"`
	TakeProfits []float64 `json:"takeProfits"`
	RRRatio     float64   `json:"rrRatio"`
	Timeframe   string    `json:"timeframe"`
	Sent        bool      `json:"sent"`
}

func loadSignals(ctx context.Context, db *pgxpool.Pool, limit int) ([]signal, error) {
	const q = `
		SELECT id, time, symbol, strategy, direction, entry, stop_loss, take_profits, rr_ratio, timeframe, sent
		FROM signals
		ORDER BY time DESC
		LIMIT $1`
	rows, err := db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []signal{}
	for rows.Next() {
		var s signal
		if err := rows.Scan(&s.ID, &s.Time, &s.Symbol, &s.Strategy, &s.Direction,
			&s.Entry, &s.StopLoss, &s.TakeProfits, &s.RRRatio, &s.Timeframe, &s.Sent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type stats struct {
	Closed   int      `json:"closed"`
	Open     int      `json:"open"`
	Wins     int      `json:"wins"`
	WinRate  *float64 `json:"winRate"` // nil until there is at least one closed trade
	TotalPct float64  `json:"totalPct"`
	AvgPct   float64  `json:"avgPct"`
	BestPct  float64  `json:"bestPct"`
	WorstPct float64  `json:"worstPct"`
}

func loadStats(ctx context.Context, db *pgxpool.Pool) (*stats, error) {
	const q = `
		SELECT
			COUNT(*) FILTER (WHERE status = 'closed'),
			COUNT(*) FILTER (WHERE status <> 'closed'),
			COUNT(*) FILTER (WHERE status = 'closed' AND pnl_pct > 0),
			COALESCE(SUM(pnl_pct) FILTER (WHERE status = 'closed'), 0),
			COALESCE(AVG(pnl_pct) FILTER (WHERE status = 'closed'), 0),
			COALESCE(MAX(pnl_pct) FILTER (WHERE status = 'closed'), 0),
			COALESCE(MIN(pnl_pct) FILTER (WHERE status = 'closed'), 0)
		FROM positions`
	var st stats
	if err := db.QueryRow(ctx, q).Scan(&st.Closed, &st.Open, &st.Wins,
		&st.TotalPct, &st.AvgPct, &st.BestPct, &st.WorstPct); err != nil {
		return nil, err
	}
	if st.Closed > 0 {
		wr := float64(st.Wins) / float64(st.Closed) * 100
		st.WinRate = &wr
	}
	return &st, nil
}

type equityPoint struct {
	Time   time.Time `json:"time"`
	CumPct float64   `json:"cumPct"`
}

// loadEquity returns the cumulative sum of closed-trade PnL% over time.
func loadEquity(ctx context.Context, db *pgxpool.Pool) ([]equityPoint, error) {
	const q = `
		SELECT closed_at, pnl_pct
		FROM positions
		WHERE status = 'closed' AND closed_at IS NOT NULL AND pnl_pct IS NOT NULL
		ORDER BY closed_at`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []equityPoint{}
	cum := 0.0
	for rows.Next() {
		var ts time.Time
		var pct float64
		if err := rows.Scan(&ts, &pct); err != nil {
			return nil, err
		}
		cum += pct
		out = append(out, equityPoint{Time: ts, CumPct: cum})
	}
	return out, rows.Err()
}
