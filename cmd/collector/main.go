package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/crypto-monitor/internal/bybit"
	"github.com/yourorg/crypto-monitor/internal/config"
	"github.com/yourorg/crypto-monitor/internal/storage"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Connect to PostgreSQL.
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		slog.Error("database ping failed", "err", err)
		os.Exit(1)
	}
	slog.Info("connected to database")

	// Run migrations.
	if err := runMigrations(ctx, pool); err != nil {
		slog.Error("migrations failed", "err", err)
		os.Exit(1)
	}

	// Connect to Redis.
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		slog.Error("failed to parse Redis URL", "err", err)
		os.Exit(1)
	}
	redisClient := redis.NewClient(redisOpts)
	defer redisClient.Close()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		slog.Error("Redis ping failed", "err", err)
		os.Exit(1)
	}
	slog.Info("connected to Redis")

	// Set up storage.
	candleStore := storage.NewCandleStore(pool)
	buf := storage.NewBatchBuffer(candleStore)

	// Start the batch buffer flush loop.
	go buf.Run(ctx)

	// Set up WebSocket client.
	wsClient := bybit.NewWSClient(cfg.WebSocketURL)

	// Register kline handler.
	wsClient.OnMessage("kline.", func(topic string, data json.RawMessage) {
		// topic format: kline.{interval}.{symbol}
		parts := strings.SplitN(topic, ".", 3)
		if len(parts) != 3 {
			return
		}
		interval := parts[1]
		symbol := parts[2]

		// data is the raw "data" field from the WS envelope: an array of KlineData.
		var klines []bybit.KlineData
		if err := json.Unmarshal(data, &klines); err != nil {
			slog.Debug("kline unmarshal error", "err", err)
			return
		}

		for _, kd := range klines {
			// Only process confirmed (closed) candles to avoid duplicates.
			if !kd.Confirm {
				continue
			}

			t := time.UnixMilli(kd.Start).UTC()
			open, _ := strconv.ParseFloat(kd.Open, 64)
			high, _ := strconv.ParseFloat(kd.High, 64)
			low, _ := strconv.ParseFloat(kd.Low, 64)
			close_, _ := strconv.ParseFloat(kd.Close, 64)
			vol, _ := strconv.ParseFloat(kd.Volume, 64)

			c := bybit.Candle{
				Time:     t,
				Symbol:   symbol,
				Interval: interval,
				Open:     open,
				High:     high,
				Low:      low,
				Close:    close_,
				Volume:   vol,
			}
			buf.Add(c)
		}
	})

	// Register ticker handler for price updates stored in Redis.
	// The data field is the TickerData object directly.
	wsClient.OnMessage("tickers.", func(topic string, data json.RawMessage) {
		var td bybit.TickerData
		if err := json.Unmarshal(data, &td); err != nil {
			return
		}
		if td.Symbol == "" || td.LastPrice == "" {
			return
		}

		key := "ticker:" + td.Symbol
		if err := redisClient.HSet(ctx, key,
			"last_price", td.LastPrice,
			"volume_24h", td.Volume24h,
			"price_change_pct", td.Price24hPcnt,
			"updated_at", time.Now().UnixMilli(),
		).Err(); err != nil {
			slog.Debug("redis ticker set error", "err", err)
		}
	})

	// Build topic list.
	topics := buildTopics(cfg.Symbols, cfg.Intervals)
	slog.Info("subscribing to topics", "count", len(topics))

	// Stats logger.
	go runStatsLogger(ctx, wsClient, buf)

	// Run the websocket client (blocking, handles reconnects).
	// We subscribe topics after each connect by wrapping in a goroutine that
	// waits for the connection and then subscribes.
	go func() {
		// Give the client time to establish the first connection.
		time.Sleep(2 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := wsClient.Subscribe(topics); err != nil {
				slog.Warn("subscribe error", "err", err)
				time.Sleep(3 * time.Second)
			} else {
				slog.Info("subscribed to topics", "count", len(topics))
				// Wait until reconnect happens (simple approach: re-subscribe periodically).
				time.Sleep(55 * time.Second)
			}
		}
	}()

	slog.Info("collector starting", "symbols", len(cfg.Symbols), "intervals", cfg.Intervals)
	if err := wsClient.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("websocket run error", "err", err)
		os.Exit(1)
	}

	slog.Info("collector shutting down")
}

func buildTopics(symbols, intervals []string) []string {
	topics := make([]string, 0, len(symbols)*len(intervals)+len(symbols))
	for _, sym := range symbols {
		for _, iv := range intervals {
			topics = append(topics, "kline."+iv+"."+sym)
		}
		topics = append(topics, "tickers."+sym)
	}
	return topics
}

func runStatsLogger(ctx context.Context, ws *bybit.WSClient, buf *storage.BatchBuffer) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var prevMessages int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msgs := ws.Messages()
			msgsPerSec := float64(msgs-prevMessages) / 30
			prevMessages = msgs

			slog.Info("collector stats",
				"messages_total", msgs,
				"messages_per_sec", msgsPerSec,
				"reconnects", ws.Reconnects(),
				"buffer_size", buf.Size(),
			)
		}
	}
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS candles (
			time        TIMESTAMPTZ NOT NULL,
			symbol      TEXT NOT NULL,
			interval    TEXT NOT NULL,
			open        DECIMAL(20,8) NOT NULL,
			high        DECIMAL(20,8) NOT NULL,
			low         DECIMAL(20,8) NOT NULL,
			close       DECIMAL(20,8) NOT NULL,
			volume      DECIMAL(20,8) NOT NULL,
			PRIMARY KEY (time, symbol, interval)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_candles_symbol_interval ON candles (symbol, interval, time DESC)`,
		`CREATE TABLE IF NOT EXISTS signals (
			id           BIGSERIAL PRIMARY KEY,
			time         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			symbol       TEXT NOT NULL,
			strategy     TEXT NOT NULL,
			direction    TEXT NOT NULL,
			entry        DECIMAL(20,8) NOT NULL,
			stop_loss    DECIMAL(20,8) NOT NULL,
			take_profits DECIMAL(20,8)[] NOT NULL,
			rr_ratio     DECIMAL(10,4) NOT NULL,
			timeframe    TEXT NOT NULL,
			sent         BOOLEAN NOT NULL DEFAULT FALSE,
			sent_at      TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_signals_symbol_time ON signals (symbol, time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_signals_sent ON signals (sent) WHERE NOT sent`,
		`CREATE TABLE IF NOT EXISTS indicators (
			time         TIMESTAMPTZ NOT NULL,
			symbol       TEXT NOT NULL,
			interval     TEXT NOT NULL,
			ma10         DECIMAL(20,8),
			ma20         DECIMAL(20,8),
			ma60         DECIMAL(20,8),
			rsi14        DECIMAL(10,4),
			atr14        DECIMAL(20,8),
			volume_ratio DECIMAL(10,4),
			PRIMARY KEY (time, symbol, interval)
		)`,
		`CREATE TABLE IF NOT EXISTS positions (
			id           BIGSERIAL PRIMARY KEY,
			signal_id    BIGINT REFERENCES signals(id),
			symbol       TEXT NOT NULL,
			direction    TEXT NOT NULL,
			entry        DECIMAL(20,8) NOT NULL,
			stop_loss    DECIMAL(20,8) NOT NULL,
			take_profits DECIMAL(20,8)[],
			opened_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			closed_at    TIMESTAMPTZ,
			close_price  DECIMAL(20,8),
			pnl_pct      DECIMAL(10,4),
			status       TEXT NOT NULL DEFAULT 'open'
		)`,
	}

	for _, m := range migrations {
		if _, err := pool.Exec(ctx, m); err != nil {
			slog.Warn("migration error (may be non-fatal)", "err", err)
		}
	}

	// Attempt to create hypertables (requires TimescaleDB).
	hypertables := []string{
		`SELECT create_hypertable('candles', 'time', if_not_exists => TRUE)`,
		`SELECT create_hypertable('indicators', 'time', if_not_exists => TRUE)`,
	}
	for _, h := range hypertables {
		if _, err := pool.Exec(ctx, h); err != nil {
			slog.Warn("hypertable creation skipped (TimescaleDB not available?)", "err", err)
		}
	}

	slog.Info("migrations completed")
	return nil
}
