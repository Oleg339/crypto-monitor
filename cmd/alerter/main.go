package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/crypto-monitor/internal/alert"
	"github.com/yourorg/crypto-monitor/internal/config"
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

	// Connect to PostgreSQL (for fallback polling).
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

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

	// Create dependencies.
	bot := alert.NewTelegramBot(cfg.TelegramBotToken, cfg.TelegramChatID)
	formatter := alert.NewFormatter(cfg.TotalCapital, cfg.RiskPerTradePct, 3)
	signalStore := storage.NewSignalStore(pool)

	slog.Info("alerter started")

	// Subscribe to Redis channel for real-time signals.
	pubsub := redisClient.Subscribe(ctx, "signals")
	defer pubsub.Close()

	// Fallback poller.
	go runFallbackPoller(ctx, signalStore, bot, formatter)

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			slog.Info("alerter shutting down")
			return
		case msg, ok := <-ch:
			if !ok {
				slog.Info("Redis pubsub channel closed")
				return
			}
			handleRedisSignal(ctx, msg.Payload, bot, formatter)
		}
	}
}

func handleRedisSignal(ctx context.Context, payload string, bot *alert.TelegramBot, formatter *alert.Formatter) {
	var sig strategy.TradeSignal
	if err := json.Unmarshal([]byte(payload), &sig); err != nil {
		slog.Error("failed to unmarshal signal", "err", err)
		return
	}

	text := formatter.Format(sig)

	if err := bot.SendMessage(ctx, text); err != nil {
		slog.Error("failed to send Telegram message", "err", err, "symbol", sig.Symbol)
		return
	}

	slog.Info("alert sent", "symbol", sig.Symbol, "strategy", sig.Strategy)
}

func runFallbackPoller(
	ctx context.Context,
	signalStore *storage.SignalStore,
	bot *alert.TelegramBot,
	formatter *alert.Formatter,
) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollUnsent(ctx, signalStore, bot, formatter)
		}
	}
}

func pollUnsent(
	ctx context.Context,
	signalStore *storage.SignalStore,
	bot *alert.TelegramBot,
	formatter *alert.Formatter,
) {
	unsent, err := signalStore.GetUnsent(ctx)
	if err != nil {
		slog.Error("failed to get unsent signals", "err", err)
		return
	}

	for _, stored := range unsent {
		sig := storedToTradeSignal(stored)
		text := formatter.Format(sig)

		if err := bot.SendMessage(ctx, text); err != nil {
			slog.Error("failed to send fallback alert", "err", err, "id", stored.ID)
			continue
		}

		if err := signalStore.MarkSent(ctx, stored.ID); err != nil {
			slog.Error("failed to mark signal sent", "err", err, "id", stored.ID)
		}

		slog.Info("fallback alert sent", "id", stored.ID, "symbol", stored.Symbol)
	}
}

func storedToTradeSignal(s storage.StoredSignal) strategy.TradeSignal {
	var sigType strategy.SignalType
	switch s.Direction {
	case "LONG":
		sigType = strategy.LongSignal
	case "SHORT":
		sigType = strategy.ShortSignal
	}

	return strategy.TradeSignal{
		Type:        sigType,
		Symbol:      s.Symbol,
		Strategy:    s.Strategy,
		Timeframe:   s.Timeframe,
		Entry:       s.Entry,
		StopLoss:    s.StopLoss,
		TakeProfits: s.TakeProfits,
		RRRatio:     s.RRRatio,
		Time:        s.Time,
	}
}
