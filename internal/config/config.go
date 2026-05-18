package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	BybitAPIKey    string
	BybitAPISecret string
	BybitTestnet   bool

	DatabaseURL string
	RedisURL    string

	TelegramBotToken string
	TelegramChatID   string

	TotalCapital   float64
	RiskPerTradePct float64
	MaxPositions   int
	MinRRRatio     float64

	Symbols   []string
	Intervals []string

	WebSocketURL string
	RESTURL      string
}

// Load reads configuration from the .env file and environment variables.
func Load() (*Config, error) {
	// Best-effort load of .env file; environment variables take precedence.
	_ = godotenv.Load(".env")

	cfg := &Config{}

	cfg.BybitAPIKey = getEnv("BYBIT_API_KEY", "")
	cfg.BybitAPISecret = getEnv("BYBIT_API_SECRET", "")
	cfg.BybitTestnet = getEnvBool("BYBIT_TESTNET", false)

	cfg.DatabaseURL = getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/crypto_monitor?sslmode=disable")
	cfg.RedisURL = getEnv("REDIS_URL", "redis://localhost:6379/0")

	cfg.TelegramBotToken = getEnv("TELEGRAM_BOT_TOKEN", "")
	cfg.TelegramChatID = getEnv("TELEGRAM_CHAT_ID", "")

	var err error
	cfg.TotalCapital, err = getEnvFloat("TOTAL_CAPITAL", 1400.0)
	if err != nil {
		return nil, fmt.Errorf("config: TOTAL_CAPITAL: %w", err)
	}

	cfg.RiskPerTradePct, err = getEnvFloat("RISK_PER_TRADE_PCT", 1.0)
	if err != nil {
		return nil, fmt.Errorf("config: RISK_PER_TRADE_PCT: %w", err)
	}

	cfg.MaxPositions, err = getEnvInt("MAX_POSITIONS", 5)
	if err != nil {
		return nil, fmt.Errorf("config: MAX_POSITIONS: %w", err)
	}

	cfg.MinRRRatio, err = getEnvFloat("MIN_RR_RATIO", 1.5)
	if err != nil {
		return nil, fmt.Errorf("config: MIN_RR_RATIO: %w", err)
	}

	symbolsRaw := getEnv("SYMBOLS", "BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,AAVEUSDT")
	cfg.Symbols = parseCSV(symbolsRaw)

	intervalsRaw := getEnv("INTERVALS", "60,240,D")
	cfg.Intervals = parseCSV(intervalsRaw)

	if cfg.BybitTestnet {
		cfg.WebSocketURL = "wss://stream-testnet.bybit.com/v5/public/linear"
		cfg.RESTURL = "https://api-testnet.bybit.com"
	} else {
		cfg.WebSocketURL = "wss://stream.bybit.com/v5/public/linear"
		cfg.RESTURL = "https://api.bybit.com"
	}

	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

func getEnvFloat(key string, defaultVal float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid float value %q for key %s: %w", v, key, err)
	}
	return f, nil
}

func getEnvInt(key string, defaultVal int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal, nil
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid int value %q for key %s: %w", v, key, err)
	}
	return i, nil
}

func parseCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
