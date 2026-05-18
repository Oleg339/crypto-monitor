package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/crypto-monitor/internal/strategy"
)

// StoredSignal is the database representation of a trade signal.
type StoredSignal struct {
	ID          int64
	Time        time.Time
	Symbol      string
	Strategy    string
	Direction   string
	Entry       float64
	StopLoss    float64
	TakeProfits []float64
	RRRatio     float64
	Timeframe   string
	Sent        bool
	SentAt      *time.Time
}

// SignalStore handles persistence of trade signals.
type SignalStore struct {
	db *pgxpool.Pool
}

// NewSignalStore creates a new SignalStore backed by the given pool.
func NewSignalStore(db *pgxpool.Pool) *SignalStore {
	return &SignalStore{db: db}
}

// Insert persists a new trade signal and returns its auto-generated ID.
func (s *SignalStore) Insert(ctx context.Context, sig strategy.TradeSignal) (int64, error) {
	const q = `
INSERT INTO signals
    (time, symbol, strategy, direction, entry, stop_loss, take_profits, rr_ratio, timeframe)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id`

	var id int64
	err := s.db.QueryRow(ctx, q,
		sig.Time,
		sig.Symbol,
		sig.Strategy,
		sig.Type.String(),
		sig.Entry,
		sig.StopLoss,
		sig.TakeProfits,
		sig.RRRatio,
		sig.Timeframe,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("storage: insert signal: %w", err)
	}

	return id, nil
}

// MarkSent marks a signal as sent and records the timestamp.
func (s *SignalStore) MarkSent(ctx context.Context, id int64) error {
	const q = `UPDATE signals SET sent = TRUE, sent_at = NOW() WHERE id = $1`
	_, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("storage: mark signal sent: %w", err)
	}
	return nil
}

// GetUnsent returns all signals that have not yet been sent.
func (s *SignalStore) GetUnsent(ctx context.Context) ([]StoredSignal, error) {
	const q = `
SELECT id, time, symbol, strategy, direction, entry, stop_loss, take_profits, rr_ratio, timeframe, sent, sent_at
FROM signals
WHERE NOT sent
ORDER BY time ASC`

	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("storage: get unsent signals: %w", err)
	}
	defer rows.Close()

	var signals []StoredSignal
	for rows.Next() {
		var sig StoredSignal
		if err := rows.Scan(
			&sig.ID, &sig.Time, &sig.Symbol, &sig.Strategy,
			&sig.Direction, &sig.Entry, &sig.StopLoss, &sig.TakeProfits,
			&sig.RRRatio, &sig.Timeframe, &sig.Sent, &sig.SentAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan signal: %w", err)
		}
		signals = append(signals, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: signal rows: %w", err)
	}

	return signals, nil
}

// WasRecentlySent returns true when any signal for the given symbol was sent
// within the specified duration.
func (s *SignalStore) WasRecentlySent(ctx context.Context, symbol string, within time.Duration) (bool, error) {
	const q = `
SELECT EXISTS (
    SELECT 1 FROM signals
    WHERE symbol = $1
      AND sent = TRUE
      AND sent_at >= NOW() - $2::interval
)`

	var exists bool
	err := s.db.QueryRow(ctx, q, symbol, within.String()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: was recently sent: %w", err)
	}

	return exists, nil
}
