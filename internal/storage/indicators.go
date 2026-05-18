package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/crypto-monitor/internal/strategy"
)

// StoredIndicators is the database representation of computed indicator values.
type StoredIndicators struct {
	Time        time.Time
	Symbol      string
	Interval    string
	MA10        float64
	MA20        float64
	MA60        float64
	RSI14       float64
	ATR14       float64
	VolumeRatio float64
}

// IndicatorStore handles persistence of computed indicators.
type IndicatorStore struct {
	db *pgxpool.Pool
}

// NewIndicatorStore creates a new IndicatorStore backed by the given pool.
func NewIndicatorStore(db *pgxpool.Pool) *IndicatorStore {
	return &IndicatorStore{db: db}
}

// Upsert inserts or updates indicator values for the given symbol, interval, and time.
func (s *IndicatorStore) Upsert(ctx context.Context, symbol, interval string, t time.Time, ind strategy.Indicators) error {
	const q = `
INSERT INTO indicators (time, symbol, interval, ma10, ma20, ma60, rsi14, atr14, volume_ratio)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (time, symbol, interval) DO UPDATE SET
    ma10         = EXCLUDED.ma10,
    ma20         = EXCLUDED.ma20,
    ma60         = EXCLUDED.ma60,
    rsi14        = EXCLUDED.rsi14,
    atr14        = EXCLUDED.atr14,
    volume_ratio = EXCLUDED.volume_ratio`

	_, err := s.db.Exec(ctx, q,
		t, symbol, interval,
		ind.MA.MA10, ind.MA.MA20, ind.MA.MA60,
		ind.RSI.Value,
		ind.ATR.Value,
		ind.Volume.Ratio,
	)
	if err != nil {
		return fmt.Errorf("storage: upsert indicators: %w", err)
	}

	return nil
}

// GetLatest returns the most recent indicator row for the given symbol and interval.
func (s *IndicatorStore) GetLatest(ctx context.Context, symbol, interval string) (*StoredIndicators, error) {
	const q = `
SELECT time, symbol, interval, ma10, ma20, ma60, rsi14, atr14, volume_ratio
FROM indicators
WHERE symbol = $1 AND interval = $2
ORDER BY time DESC
LIMIT 1`

	row := s.db.QueryRow(ctx, q, symbol, interval)

	var ind StoredIndicators
	err := row.Scan(
		&ind.Time, &ind.Symbol, &ind.Interval,
		&ind.MA10, &ind.MA20, &ind.MA60,
		&ind.RSI14, &ind.ATR14, &ind.VolumeRatio,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get latest indicators: %w", err)
	}

	return &ind, nil
}
