package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const bybitWS = "wss://stream.bybit.com/v5/public/linear"

type wsMsg struct {
	Op   string          `json:"op,omitempty"`
	Args []string        `json:"args,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Type string          `json:"type,omitempty"`
}

type tickerData struct {
	Symbol    string `json:"symbol"`
	LastPrice string `json:"lastPrice"`
	MarkPrice string `json:"markPrice"`
}

// ── Monitor ───────────────────────────────────────────────────────────────────

type Monitor struct {
	store  *PaperStore
	bot    *Bot
	safety *SafetyChecker
	cfg    *Config

	mu         sync.Mutex
	subscribed map[string]bool
	lastPrice  map[string]float64 // symbol → latest mark price
	subQueue   chan string
}

func newMonitor(store *PaperStore, bot *Bot, safety *SafetyChecker, cfg *Config) *Monitor {
	return &Monitor{
		store:      store,
		bot:        bot,
		safety:     safety,
		cfg:        cfg,
		subscribed: map[string]bool{},
		lastPrice:  map[string]float64{},
		subQueue:   make(chan string, 64),
	}
}

func (m *Monitor) Subscribe(symbol string) {
	m.subQueue <- symbol
}

func (m *Monitor) Run(ctx context.Context) {
	// Re-subscribe open trades from DB on startup
	go func() {
		trades, err := m.store.OpenTrades(ctx)
		if err != nil {
			log.Printf("[monitor] load open trades: %v", err)
			return
		}
		for _, t := range trades {
			m.subQueue <- t.Symbol
		}
	}()

	// TTL checker: every hour expire stale paper trades
	if m.cfg.OrderTTLDays > 0 {
		go m.runTTL(ctx)
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.runOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[monitor] ws error: %v — reconnecting in 5s", err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

// runTTL checks every hour for paper trades older than ORDER_TTL_DAYS.
func (m *Monitor) runTTL(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	// Also run once immediately on start
	m.checkTTL(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkTTL(ctx)
		}
	}
}

func (m *Monitor) checkTTL(ctx context.Context) {
	trades, err := m.store.OpenTrades(ctx)
	if err != nil {
		log.Printf("[monitor] ttl: open trades: %v", err)
		return
	}
	ttl := time.Duration(m.cfg.OrderTTLDays * float64(24*time.Hour))
	now := time.Now()

	for _, t := range trades {
		age := now.Sub(t.OpenAt)
		if age < ttl {
			continue
		}

		// Close at last known mark price, fallback to entry
		m.mu.Lock()
		closePrice := m.lastPrice[t.Symbol]
		m.mu.Unlock()
		if closePrice == 0 {
			closePrice = t.EntryPrice
		}

		if err := m.store.CloseTrade(ctx, t.ID, "ttl_expired", closePrice); err != nil {
			log.Printf("[monitor] ttl: close trade %d: %v", t.ID, err)
			continue
		}

		log.Printf("[monitor] ttl: trade %d %s expired after %.1fd, closed @ %.6g",
			t.ID, t.Symbol, age.Hours()/24, closePrice)

		pnl := (closePrice - t.EntryPrice) * t.Qty
		sign := "+"
		if pnl < 0 {
			sign = ""
		}
		m.bot.send(m.bot.chatID, 0, fmt.Sprintf(
			"⏰ <b>[PAPER] TTL истёк — %s/USDT</b>\n\n"+
				"Позиция открыта %.1f дней — закрыта по истечению ORDER_TTL_DAYS=%g\n"+
				"Закрыто @ <code>$%.6g</code>\n"+
				"PnL: <code>%s$%.2f</code>\n"+
				"<i>Trade ID: %d</i>",
			fmt.Sprintf("%s", t.Symbol[:len(t.Symbol)-4]),
			age.Hours()/24,
			m.cfg.OrderTTLDays,
			closePrice,
			sign, pnl,
			t.ID,
		), nil)

		go m.safety.RunAfterClose(ctx)
	}
}

func (m *Monitor) runOnce(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, bybitWS, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	log.Println("[monitor] WebSocket connected")

	m.mu.Lock()
	m.subscribed = map[string]bool{}
	m.mu.Unlock()

	drainAndSubscribe := func() {
		for {
			select {
			case sym := <-m.subQueue:
				m.doSubscribe(conn, sym)
			default:
				return
			}
		}
	}

	sendErrs := make(chan error, 1)
	go func() {
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		drainAndSubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case sym := <-m.subQueue:
				if err := m.doSubscribe(conn, sym); err != nil {
					sendErrs <- err
					return
				}
			case <-ping.C:
				if err := conn.WriteJSON(map[string]string{"op": "ping"}); err != nil {
					sendErrs <- err
					return
				}
			}
		}
	}()

	for {
		select {
		case err := <-sendErrs:
			return err
		case <-ctx.Done():
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		var msg wsMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if msg.Type == "snapshot" || msg.Type == "delta" {
			m.handleTick(ctx, msg.Data)
		}
	}
}

func (m *Monitor) doSubscribe(conn *websocket.Conn, symbol string) error {
	m.mu.Lock()
	already := m.subscribed[symbol]
	if !already {
		m.subscribed[symbol] = true
	}
	m.mu.Unlock()

	if already {
		return nil
	}
	arg := "tickers." + symbol
	log.Printf("[monitor] subscribing %s", arg)
	return conn.WriteJSON(wsMsg{Op: "subscribe", Args: []string{arg}})
}

func (m *Monitor) handleTick(ctx context.Context, raw json.RawMessage) {
	var tick tickerData
	if err := json.Unmarshal(raw, &tick); err != nil || tick.Symbol == "" {
		return
	}

	price := pf(tick.MarkPrice)
	if price == 0 {
		price = pf(tick.LastPrice)
	}
	if price == 0 {
		return
	}

	// Update last known price
	m.mu.Lock()
	m.lastPrice[tick.Symbol] = price
	m.mu.Unlock()

	trades, err := m.store.OpenTrades(ctx)
	if err != nil {
		log.Printf("[monitor] open trades: %v", err)
		return
	}

	for _, t := range trades {
		if t.Symbol != tick.Symbol {
			continue
		}
		m.checkClose(ctx, t, price)
	}
}

func (m *Monitor) checkClose(ctx context.Context, t *PaperTrade, price float64) {
	var status string
	var closePrice float64

	if price <= t.SL {
		status = "sl_hit"
		closePrice = t.SL
	} else if price >= t.TP {
		status = "tp_hit"
		closePrice = t.TP
	}

	if status == "" {
		return
	}

	if err := m.store.CloseTrade(ctx, t.ID, status, closePrice); err != nil {
		log.Printf("[monitor] close trade %d: %v", t.ID, err)
		return
	}

	log.Printf("[monitor] trade %d %s closed %s @ %.6g", t.ID, t.Symbol, status, closePrice)
	m.bot.send(m.bot.chatID, 0, formatPaperClose(t, closePrice, status), nil)

	go m.safety.RunAfterClose(ctx)
}
