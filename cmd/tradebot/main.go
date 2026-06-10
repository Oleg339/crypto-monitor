// tradebot — minimal Bybit trading bot controlled via Telegram.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/joho/godotenv"
)

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	Testnet   bool
	APIKey    string
	APISecret string
	TGToken   string
	TGChatID  int64
	RiskPct   float64
	Leverage  float64
	// Paper trading
	PaperTrading       bool
	DatabaseURL        string
	PaperInitialEquity float64
	// Safety rules
	MinEquity       float64
	BacktestWinRate float64
	BacktestAvgPnL  float64
	OrderTTLDays    float64
	// MTProto userbot
	AppID           int
	AppHash         string
	Phone           string
	Trader3Channel  string
	Trader3ThreadID int
}

func loadConfig() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("[config] .env not found, using environment variables")
	}

	chatID, _ := strconv.ParseInt(getenv("TELEGRAM_CHAT_ID", "0"), 10, 64)
	appID, _ := strconv.Atoi(getenv("TELEGRAM_APP_ID", "0"))

	return &Config{
		Testnet:        getenv("BYBIT_TESTNET", "true") == "true",
		APIKey:         getenv("BYBIT_API_KEY", ""),
		APISecret:      getenv("BYBIT_API_SECRET", ""),
		TGToken:        getenv("TELEGRAM_BOT_TOKEN", ""),
		TGChatID:       chatID,
		RiskPct:        getenvF("RISK_PCT", 10),
		Leverage:       getenvF("LEVERAGE", 3),
		PaperTrading:       getenv("PAPER_TRADING", "false") == "true",
		DatabaseURL:        getenv("DATABASE_URL", ""),
		PaperInitialEquity: getenvF("PAPER_INITIAL_EQUITY", 1000),
		MinEquity:          getenvF("MIN_EQUITY", 0),
		BacktestWinRate:    getenvF("BACKTEST_WIN_RATE", 60.7),
		BacktestAvgPnL:     getenvF("BACKTEST_AVG_PNL", 4.80),
		OrderTTLDays:       getenvF("ORDER_TTL_DAYS", 8),
		AppID:           appID,
		AppHash:         getenv("TELEGRAM_APP_HASH", ""),
		Phone:           getenv("TELEGRAM_PHONE", ""),
		Trader3Channel:  getenv("TRADER3_CHANNEL", ""),
		Trader3ThreadID: func() int { v, _ := strconv.Atoi(getenv("TRADER3_THREAD_ID", "8")); return v }(),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvF(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	if cfg.APIKey == "" || cfg.APISecret == "" {
		log.Fatal("[config] BYBIT_API_KEY and BYBIT_API_SECRET must be set")
	}
	if cfg.TGToken == "" {
		log.Fatal("[config] TELEGRAM_BOT_TOKEN must be set")
	}
	if cfg.TGChatID == 0 {
		log.Fatal("[config] TELEGRAM_CHAT_ID must be set")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Println("[bot] shutting down…")
		cancel()
	}()

	bybit := newBybitClient(cfg)

	// DB store — init whenever DATABASE_URL is set (required for paper trading, optional for signal logging)
	var paper *PaperStore
	var monitor *Monitor
	if cfg.DatabaseURL != "" {
		var err error
		paper, err = newPaperStore(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("[paper] DB init: %v", err)
		}
		if cfg.PaperTrading {
			log.Println("[paper] mode ON — trades go to paper_trades table")
		} else {
			log.Println("[signals] DB connected — signal logging enabled")
		}
	} else if cfg.PaperTrading {
		log.Fatal("[config] PAPER_TRADING=true requires DATABASE_URL to be set")
	}

	bot := newBot(cfg, bybit, paper)

	if paper != nil {
		// Mirror real closed trades into the DB; in live mode the sync also
		// drives the safety rules (loss streak, rolling winrate, min equity).
		var safety *liveSafety
		if !cfg.PaperTrading {
			safety = newLiveSafety(paper.pool, bot, bybit, cfg)
			log.Printf("[livesafety] enabled: loss streak %d, rolling winrate <30%% (окно %d), MIN_EQUITY=%.0f",
				int(getenvF("LOSS_STREAK_PAUSE", 4)), rollingWindow, cfg.MinEquity)
		}
		go runTradeSync(ctx, paper.pool, bybit, safety)
		log.Println("[tradesync] closed-trade sync started")
	}

	if cfg.PaperTrading {
		safety := newSafetyChecker(paper, bot, bybit, cfg)
		monitor = newMonitor(paper, bot, safety, cfg)
		bot.monitor = monitor
		go monitor.Run(ctx)
		log.Println("[monitor] price monitor started")
	} else if cfg.OrderTTLDays > 0 {
		go runLiveTTL(ctx, bybit, bot, cfg)
	}

	ub := newUserbot(cfg, bot)
	bot.userbot = ub

	env := "mainnet"
	if cfg.Testnet {
		env = "testnet"
	}
	mode := "live"
	if cfg.PaperTrading {
		mode = "paper"
	}
	log.Printf("[bot] started  env=%s  mode=%s  risk=%.0f%%  leverage=%.0fx",
		env, mode, cfg.RiskPct, cfg.Leverage)

	if cfg.AppID != 0 && cfg.AppHash != "" && cfg.Phone != "" {
		go func() {
			if err := ub.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[userbot] stopped: %v", err)
			}
		}()
	} else {
		log.Println("[userbot] not started — set TELEGRAM_APP_ID / TELEGRAM_APP_HASH / TELEGRAM_PHONE in .env")
	}

	bot.Run(ctx)
}
