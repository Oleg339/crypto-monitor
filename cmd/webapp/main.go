package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

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

	s := &server{
		bybit:     newBybitClient(base, getenv("BYBIT_API_KEY", ""), getenv("BYBIT_API_SECRET", "")),
		db:        db,
		botToken:  botToken,
		allowedID: chatID,
	}

	staticFS, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/overview", s.authMiddleware(s.handleOverview))
	mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
	mux.HandleFunc("/api/signals", s.authMiddleware(s.handleSignals))
	mux.HandleFunc("/api/equity", s.authMiddleware(s.handleEquity))
	mux.HandleFunc("/api/stats", s.authMiddleware(s.handleStats))
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
	writeJSON(w, st)
}

func (s *server) handleEquity(w http.ResponseWriter, r *http.Request) {
	points, err := loadEquity(r.Context(), s.db)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, points)
}
