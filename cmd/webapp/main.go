package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

//go:embed web
var webFS embed.FS

type server struct {
	bybit     *bybitClient
	db        *pgxpool.Pool
	botToken  string
	allowedID int64
	// Backtest expectations, for "is the live winrate still plausible" checks.
	expWinRate float64
	expAvgPnl  float64
	// TTL ордеров у tradebot — чтобы показать, когда лимитник будет снят.
	orderTTLDays float64

	mu        sync.Mutex
	cached    overview
	cachedAt  time.Time
}

type overview struct {
	Balance   *balanceInfo `json:"balance"`
	Positions []position   `json:"positions"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("[config] .env not found, using environment variables")
	}

	base := "https://api.bybit.com"
	if getenv("BYBIT_TESTNET", "false") == "true" {
		base = "https://api-testnet.bybit.com"
	}
	chatID, _ := strconv.ParseInt(getenv("TELEGRAM_CHAT_ID", "0"), 10, 64)
	if chatID == 0 {
		log.Fatal("[webapp] TELEGRAM_CHAT_ID is required")
	}
	botToken := getenv("TELEGRAM_BOT_TOKEN", "")
	if botToken == "" {
		log.Fatal("[webapp] TELEGRAM_BOT_TOKEN is required")
	}

	db, err := pgxpool.New(context.Background(), getenv("DATABASE_URL", ""))
	if err != nil {
		log.Fatalf("[webapp] db connect: %v", err)
	}
	// Same idempotent bootstrap as the tradebot's trade sync, so neither
	// container depends on the other's startup order.
	for _, stmt := range []string{
		`ALTER TABLE positions ADD COLUMN IF NOT EXISTS order_id TEXT`,
		`ALTER TABLE positions ADD COLUMN IF NOT EXISTS qty FLOAT8`,
		`ALTER TABLE positions ADD COLUMN IF NOT EXISTS pnl FLOAT8`,
		`CREATE TABLE IF NOT EXISTS bot_settings (
			key        TEXT        PRIMARY KEY,
			value      TEXT        NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	} {
		if _, err := db.Exec(context.Background(), stmt); err != nil {
			log.Printf("[webapp] schema bootstrap: %v", err)
		}
	}

	// Same defaults as the tradebot's safety rules.
	expWin, _ := strconv.ParseFloat(getenv("BACKTEST_WIN_RATE", "60.7"), 64)
	expAvg, _ := strconv.ParseFloat(getenv("BACKTEST_AVG_PNL", "4.80"), 64)
	ttlDays, _ := strconv.ParseFloat(getenv("ORDER_TTL_DAYS", "8"), 64)

	s := &server{
		bybit:        newBybitClient(base, getenv("BYBIT_API_KEY", ""), getenv("BYBIT_API_SECRET", "")),
		db:           db,
		botToken:     botToken,
		allowedID:    chatID,
		expWinRate:   expWin,
		expAvgPnl:    expAvg,
		orderTTLDays: ttlDays,
	}

	hub := newLiveHub(s)
	go hub.run(context.Background(), getenv("BYBIT_TESTNET", "false") == "true")

	staticFS, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/ws", hub.handleWS)
	mux.HandleFunc("/api/overview", s.authMiddleware(s.handleOverview))
	mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
	mux.HandleFunc("/api/signals", s.authMiddleware(s.handleSignals))
	mux.HandleFunc("/api/equity", s.authMiddleware(s.handleEquity))
	mux.HandleFunc("/api/stats", s.authMiddleware(s.handleStats))
	mux.HandleFunc("/api/settings", s.authMiddleware(s.handleSettings))
	mux.HandleFunc("/api/orders", s.authMiddleware(s.handleOrders))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := ":" + getenv("WEBAPP_PORT", "8081")
	log.Printf("[webapp] listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[webapp] encode: %v", err)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// handleOverview returns live balance + positions, cached for 5s so rapid
// UI refreshes don't hammer Bybit.
func (s *server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	if time.Since(s.cachedAt) < 5*time.Second {
		ov := s.cached
		s.mu.Unlock()
		writeJSON(w, ov)
		return
	}
	s.mu.Unlock()

	bal, err := s.bybit.balance()
	if err != nil {
		writeErr(w, err)
		return
	}
	pos, err := s.bybit.positions()
	if err != nil {
		writeErr(w, err)
		return
	}
	ov := overview{Balance: bal, Positions: pos, UpdatedAt: time.Now()}

	s.mu.Lock()
	s.cached, s.cachedAt = ov, time.Now()
	s.mu.Unlock()
	writeJSON(w, ov)
}

func limitParam(r *http.Request, def, max int) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// handleSettings — авторежим бота. Настройка живёт в bot_settings (общая с
// tradebot): переключение здесь равнозначно команде /auto в Telegram.
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method == http.MethodPost {
		var req struct {
			Auto bool `json:"auto"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, err)
			return
		}
		v := "0"
		if req.Auto {
			v = "1"
		}
		if _, err := s.db.Exec(ctx, `
			INSERT INTO bot_settings (key, value, updated_at) VALUES ('auto_trade', $1, NOW())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`, v); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"auto": req.Auto})
		return
	}

	// Ключа ещё нет — дефолт тот же, что у бота: AUTO_TRADE из .env.
	auto := getenv("AUTO_TRADE", "false") == "true"
	var v string
	err := s.db.QueryRow(ctx, `SELECT value FROM bot_settings WHERE key = 'auto_trade'`).Scan(&v)
	switch {
	case err == nil:
		auto = v == "1"
	case errors.Is(err, pgx.ErrNoRows):
		// оставляем дефолт
	default:
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"auto": auto})
}

// handleOrders — ожидающие лимитные ордеры на вход (ещё не исполнены).
func (s *server) handleOrders(w http.ResponseWriter, _ *http.Request) {
	orders, err := s.bybit.openOrders()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"orders": orders, "ttlDays": s.orderTTLDays})
}

func (s *server) handleTrades(w http.ResponseWriter, r *http.Request) {
	trades, err := loadTrades(r.Context(), s.db, limitParam(r, 50, 500))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, trades)
}

func (s *server) handleSignals(w http.ResponseWriter, r *http.Request) {
	signals, err := loadSignals(r.Context(), s.db, limitParam(r, 50, 500))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, signals)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := loadStats(r.Context(), s.db)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := struct {
		*stats
		ExpWinRate *float64 `json:"expWinRate,omitempty"`
		ExpAvgPnl  *float64 `json:"expAvgPnl,omitempty"`
		// WinRateP is P(wins <= observed) assuming the backtest winrate is
		// true: how plausible the current drawdown is as pure chance.
		WinRateP *float64 `json:"winRateP,omitempty"`
	}{stats: st}
	if s.expWinRate > 0 {
		resp.ExpWinRate = &s.expWinRate
		if st.Closed > 0 {
			p := pBinomLE(st.Wins, st.Closed, s.expWinRate/100)
			resp.WinRateP = &p
		}
	}
	if s.expAvgPnl != 0 {
		resp.ExpAvgPnl = &s.expAvgPnl
	}
	writeJSON(w, resp)
}

// pBinomLE returns P(X <= k) for X ~ Binomial(n, p).
func pBinomLE(k, n int, p float64) float64 {
	if n <= 0 || p <= 0 {
		return 1
	}
	if p >= 1 {
		if k >= n {
			return 1
		}
		return 0
	}
	q := 1 - p
	term := math.Pow(q, float64(n))
	sum := 0.0
	for i := 0; i <= k && i <= n; i++ {
		if i > 0 {
			term *= float64(n-i+1) / float64(i) * p / q
		}
		sum += term
	}
	return math.Min(sum, 1)
}

func (s *server) handleEquity(w http.ResponseWriter, r *http.Request) {
	points, err := loadEquity(r.Context(), s.db)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, points)
}
