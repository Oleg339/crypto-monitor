package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/crypto-monitor/internal/bybit"
	"github.com/yourorg/crypto-monitor/internal/config"
	"github.com/yourorg/crypto-monitor/internal/indicators"
	"github.com/yourorg/crypto-monitor/internal/storage"
	"github.com/yourorg/crypto-monitor/internal/strategy"
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

	slog.Info("analyzer started", "symbols", len(cfg.Symbols))

	// Initialize stores.
	candleStore := storage.NewCandleStore(pool)
	signalStore := storage.NewSignalStore(pool)
	indicatorStore := storage.NewIndicatorStore(pool)

	// Initialize strategies.
	strategies := []strategy.Strategy{
		strategy.NewMeanReversionStrategy(cfg.MinRRRatio),
		strategy.NewSupportBounceStrategy(cfg.MinRRRatio),
	}

	// Timeframes to analyze.
	timeframes := []string{"240", "D"}

	// Run immediately on startup, then every 5 minutes.
	runAnalysis(ctx, cfg, candleStore, signalStore, indicatorStore, redisClient, strategies, timeframes)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("analyzer shutting down")
			return
		case <-ticker.C:
			runAnalysis(ctx, cfg, candleStore, signalStore, indicatorStore, redisClient, strategies, timeframes)
		}
	}
}

func runAnalysis(
	ctx context.Context,
	cfg *config.Config,
	candleStore *storage.CandleStore,
	signalStore *storage.SignalStore,
	indicatorStore *storage.IndicatorStore,
	redisClient *redis.Client,
	strategies []strategy.Strategy,
	timeframes []string,
) {
	slog.Info("running analysis cycle", "symbols", len(cfg.Symbols), "timeframes", timeframes)

	for _, sym := range cfg.Symbols {
		for _, tf := range timeframes {
			if err := analyzeSymbol(ctx, sym, tf, candleStore, signalStore, indicatorStore, redisClient, strategies); err != nil {
				slog.Error("analysis error", "symbol", sym, "timeframe", tf, "err", err)
			}
		}
	}
}

func analyzeSymbol(
	ctx context.Context,
	symbol, timeframe string,
	candleStore *storage.CandleStore,
	signalStore *storage.SignalStore,
	indicatorStore *storage.IndicatorStore,
	redisClient *redis.Client,
	strategies []strategy.Strategy,
) error {
	// Load last 100 candles.
	candles, err := candleStore.GetLatest(ctx, symbol, timeframe, 100)
	if err != nil {
		return err
	}

	if len(candles) < 65 {
		slog.Debug("not enough candles", "symbol", symbol, "timeframe", timeframe, "count", len(candles))
		return nil
	}

	// Calculate all indicators.
	ind := calculateIndicators(candles)

	// Store indicators.
	lastTime := candles[len(candles)-1].Time
	if err := indicatorStore.Upsert(ctx, symbol, timeframe, lastTime, ind); err != nil {
		slog.Warn("failed to store indicators", "symbol", symbol, "err", err)
	}

	// Run each strategy.
	for _, strat := range strategies {
		if strat.Timeframe() != timeframe {
			continue
		}

		sig := strat.Analyze(symbol, candles, ind)
		if sig == nil {
			continue
		}

		// Check deduplication: skip if signal was sent in last 4 hours.
		recentlySent, err := signalStore.WasRecentlySent(ctx, symbol, 4*time.Hour)
		if err != nil {
			slog.Warn("dedup check failed", "symbol", symbol, "err", err)
		} else if recentlySent {
			slog.Info("skipping duplicate signal", "symbol", symbol, "strategy", strat.Name())
			continue
		}

		// Persist signal.
		id, err := signalStore.Insert(ctx, *sig)
		if err != nil {
			slog.Error("failed to insert signal", "symbol", symbol, "err", err)
			continue
		}

		slog.Info("signal generated",
			"id", id,
			"symbol", symbol,
			"strategy", strat.Name(),
			"timeframe", timeframe,
			"type", sig.Type.String(),
			"entry", sig.Entry,
			"rr", sig.RRRatio,
		)

		// Publish to Redis.
		payload, err := json.Marshal(sig)
		if err == nil {
			if err := redisClient.Publish(ctx, "signals", payload).Err(); err != nil {
				slog.Warn("failed to publish signal to Redis", "err", err)
			}
		}
	}

	return nil
}

func calculateIndicators(candles []bybit.Candle) strategy.Indicators {
	ma := indicators.CalculateMA(candles)
	rsi := indicators.CalculateRSI(candles, 14)
	lastPrice := candles[len(candles)-1].Close
	atr := indicators.CalculateATR(candles, 14, lastPrice)
	vol := indicators.CalculateVolume(candles, 20)
	levels := indicators.FindSupportResistance(candles, 5)

	return strategy.Indicators{
		MA:     ma,
		RSI:    rsi,
		ATR:    atr,
		Volume: vol,
		Levels: levels,
	}
}
