package storage

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// CandleStore handles persistence of candlestick data.
type CandleStore struct {
	db *pgxpool.Pool
}

// NewCandleStore creates a new CandleStore backed by the given connection pool.
func NewCandleStore(db *pgxpool.Pool) *CandleStore {
	return &CandleStore{db: db}
}

// BatchInsert inserts a slice of candles using the COPY protocol for performance.
func (s *CandleStore) BatchInsert(ctx context.Context, candles []bybit.Candle) error {
	if len(candles) == 0 {
		return nil
	}

	rows := make([][]interface{}, 0, len(candles))
	for _, c := range candles {
		rows = append(rows, []interface{}{
			c.Time, c.Symbol, c.Interval,
			c.Open, c.High, c.Low, c.Close, c.Volume,
		})
	}

	// Use CopyFrom for maximum throughput.
	copyCount, err := s.db.CopyFrom(
		ctx,
		pgx.Identifier{"candles"},
		[]string{"time", "symbol", "interval", "open", "high", "low", "close", "volume"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		// Fall back to INSERT ON CONFLICT DO NOTHING if COPY fails (e.g., duplicates).
		return s.batchInsertFallback(ctx, candles)
	}

	if int(copyCount) != len(candles) {
		slog.Warn("storage: candle batch insert count mismatch",
			"expected", len(candles), "inserted", copyCount)
	}

	return nil
}

func (s *CandleStore) batchInsertFallback(ctx context.Context, candles []bybit.Candle) error {
	const q = `
INSERT INTO candles (time, symbol, interval, open, high, low, close, volume)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (time, symbol, interval) DO NOTHING`

	batch := &pgx.Batch{}
	for _, c := range candles {
		batch.Queue(q, c.Time, c.Symbol, c.Interval, c.Open, c.High, c.Low, c.Close, c.Volume)
	}

	br := s.db.SendBatch(ctx, batch)
	defer br.Close()

	for range candles {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("storage: candle fallback insert: %w", err)
		}
	}

	return nil
}

// GetLatest returns the most recent limit candles for the given symbol and interval,
// ordered ascending by time.
func (s *CandleStore) GetLatest(ctx context.Context, symbol, interval string, limit int) ([]bybit.Candle, error) {
	const q = `
SELECT time, symbol, interval, open, high, low, close, volume
FROM candles
WHERE symbol = $1 AND interval = $2
ORDER BY time DESC
LIMIT $3`

	rows, err := s.db.Query(ctx, q, symbol, interval, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: get latest candles: %w", err)
	}
	defer rows.Close()

	candles, err := scanCandles(rows)
	if err != nil {
		return nil, err
	}

	// Reverse to ascending order.
	for i, j := 0, len(candles)-1; i < j; i, j = i+1, j-1 {
		candles[i], candles[j] = candles[j], candles[i]
	}

	return candles, nil
}

// GetRange returns all candles for the given symbol and interval within [from, to].
func (s *CandleStore) GetRange(ctx context.Context, symbol, interval string, from, to time.Time) ([]bybit.Candle, error) {
	const q = `
SELECT time, symbol, interval, open, high, low, close, volume
FROM candles
WHERE symbol = $1 AND interval = $2 AND time >= $3 AND time <= $4
ORDER BY time ASC`

	rows, err := s.db.Query(ctx, q, symbol, interval, from, to)
	if err != nil {
		return nil, fmt.Errorf("storage: get range candles: %w", err)
	}
	defer rows.Close()

	return scanCandles(rows)
}

func scanCandles(rows pgx.Rows) ([]bybit.Candle, error) {
	var candles []bybit.Candle
	for rows.Next() {
		var c bybit.Candle
		if err := rows.Scan(&c.Time, &c.Symbol, &c.Interval, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			return nil, fmt.Errorf("storage: scan candle: %w", err)
		}
		candles = append(candles, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: candle rows: %w", err)
	}
	return candles, nil
}

// BatchBuffer accumulates candles and flushes them to the store periodically
// or when the buffer reaches maxSize.
type BatchBuffer struct {
	store   *CandleStore
	mu      sync.Mutex
	buffer  []bybit.Candle
	maxSize int
	ticker  *time.Ticker
}

// NewBatchBuffer creates a new BatchBuffer with a 5-second flush interval and
// a maximum buffer size of 1000 records.
func NewBatchBuffer(store *CandleStore) *BatchBuffer {
	return &BatchBuffer{
		store:   store,
		buffer:  make([]bybit.Candle, 0, 1000),
		maxSize: 1000,
		ticker:  time.NewTicker(5 * time.Second),
	}
}

// Add appends a candle to the buffer, flushing immediately if maxSize is reached.
func (b *BatchBuffer) Add(c bybit.Candle) {
	b.mu.Lock()
	b.buffer = append(b.buffer, c)
	shouldFlush := len(b.buffer) >= b.maxSize
	b.mu.Unlock()

	if shouldFlush {
		b.flush(context.Background())
	}
}

// Size returns the current number of buffered candles.
func (b *BatchBuffer) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer)
}

// Run starts the periodic flush loop. It blocks until ctx is cancelled.
func (b *BatchBuffer) Run(ctx context.Context) {
	defer b.ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.flush(context.Background())
			return
		case <-b.ticker.C:
			b.flush(ctx)
		}
	}
}

func (b *BatchBuffer) flush(ctx context.Context) {
	b.mu.Lock()
	if len(b.buffer) == 0 {
		b.mu.Unlock()
		return
	}
	toFlush := make([]bybit.Candle, len(b.buffer))
	copy(toFlush, b.buffer)
	b.buffer = b.buffer[:0]
	b.mu.Unlock()

	if err := b.store.BatchInsert(ctx, toFlush); err != nil {
		slog.Error("storage: batch buffer flush", "err", err, "count", len(toFlush))
	} else {
		slog.Debug("storage: batch buffer flushed", "count", len(toFlush))
	}
}
