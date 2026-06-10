package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// liveHub keeps a live copy of balance+positions fed by Bybit websockets
// (private: wallet/position events, public: mark-price tickers) and streams
// overview snapshots to Mini App clients connected to /ws.
//
// REST remains the source of truth: a snapshot is fetched on start, every
// 60s, and after every position event; tickers only move mark prices and
// PnL between snapshots.

const (
	liveSnapshotEvery = 60 * time.Second
	livePushThrottle  = 700 * time.Millisecond
	liveAuthTimeout   = 10 * time.Second
)

type liveHub struct {
	srv *server

	mu      sync.Mutex
	bal     balanceInfo
	pos     []position
	updated time.Time
	ready   bool

	cmu     sync.Mutex
	clients map[*websocket.Conn]struct{}

	pub        *bybit.WSClient
	subscribed map[string]struct{}

	dirty   chan struct{}
	refresh chan struct{}
}

func newLiveHub(s *server) *liveHub {
	return &liveHub{
		srv:        s,
		clients:    map[*websocket.Conn]struct{}{},
		subscribed: map[string]struct{}{},
		dirty:      make(chan struct{}, 1),
		refresh:    make(chan struct{}, 1),
	}
}

func (h *liveHub) run(ctx context.Context, testnet bool) {
	host := "stream.bybit.com"
	if testnet {
		host = "stream-testnet.bybit.com"
	}

	// Public ticker stream: resubscribes to the current set on reconnect.
	h.pub = bybit.NewWSClient("wss://" + host + "/v5/public/linear")
	h.pub.OnMessage("tickers.", h.onTicker)
	h.pub.SetOnConnect(func(c *bybit.WSClient) error {
		h.mu.Lock()
		topics := make([]string, 0, len(h.subscribed))
		for s := range h.subscribed {
			topics = append(topics, "tickers."+s)
		}
		h.mu.Unlock()
		if len(topics) == 0 {
			return nil
		}
		return c.Subscribe(topics)
	})
	go h.pub.Run(ctx)

	// Private stream: auth + subscribe on every (re)connect.
	priv := bybit.NewWSClient("wss://" + host + "/v5/private")
	priv.OnMessage("wallet", h.onWallet)
	priv.OnMessage("position", func(string, json.RawMessage) { h.requestRefresh() })
	priv.SetOnConnect(func(c *bybit.WSClient) error {
		expires := time.Now().Add(5 * time.Minute).UnixMilli()
		mac := hmac.New(sha256.New, []byte(h.srv.bybit.secret))
		mac.Write([]byte("GET/realtime" + strconv.FormatInt(expires, 10)))
		if err := c.Send(map[string]any{
			"op":   "auth",
			"args": []any{h.srv.bybit.key, expires, hex.EncodeToString(mac.Sum(nil))},
		}); err != nil {
			return err
		}
		// A REST snapshot resyncs anything missed while disconnected.
		h.requestRefresh()
		return c.Subscribe([]string{"position", "wallet"})
	})
	go priv.Run(ctx)

	go h.broadcaster(ctx)

	// Snapshot loop: initial + periodic + on demand after position events.
	tick := time.NewTicker(liveSnapshotEvery)
	defer tick.Stop()
	h.snapshot()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.snapshot()
		case <-h.refresh:
			h.snapshot()
		}
	}
}

func (h *liveHub) requestRefresh() {
	select {
	case h.refresh <- struct{}{}:
	default:
	}
}

func (h *liveHub) markDirty() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

// snapshot pulls balance+positions over REST and reconciles ticker subs.
func (h *liveHub) snapshot() {
	bal, err := h.srv.bybit.balance()
	if err != nil {
		log.Printf("[live] balance snapshot: %v", err)
		return
	}
	pos, err := h.srv.bybit.positions()
	if err != nil {
		log.Printf("[live] positions snapshot: %v", err)
		return
	}

	want := map[string]struct{}{}
	for _, p := range pos {
		want[p.Symbol] = struct{}{}
	}

	h.mu.Lock()
	h.bal, h.pos, h.updated, h.ready = *bal, pos, time.Now(), true
	var sub, unsub []string
	for s := range want {
		if _, ok := h.subscribed[s]; !ok {
			sub = append(sub, "tickers."+s)
			h.subscribed[s] = struct{}{}
		}
	}
	for s := range h.subscribed {
		if _, ok := want[s]; !ok {
			unsub = append(unsub, "tickers."+s)
			delete(h.subscribed, s)
		}
	}
	h.mu.Unlock()

	if len(sub) > 0 {
		if err := h.pub.Subscribe(sub); err != nil {
			log.Printf("[live] subscribe %v: %v", sub, err)
		}
	}
	if len(unsub) > 0 {
		if err := h.pub.Unsubscribe(unsub); err != nil {
			log.Printf("[live] unsubscribe %v: %v", unsub, err)
		}
	}
	h.markDirty()
}

// onTicker updates the mark price of the matching position and recomputes PnL.
func (h *liveHub) onTicker(topic string, data json.RawMessage) {
	var t struct {
		Symbol    string `json:"symbol"`
		MarkPrice string `json:"markPrice"`
	}
	if err := json.Unmarshal(data, &t); err != nil || t.MarkPrice == "" {
		return // delta pushes may omit markPrice
	}
	mark := pf(t.MarkPrice)
	if mark == 0 {
		return
	}

	h.mu.Lock()
	changed := false
	totalUPL := 0.0
	for i := range h.pos {
		p := &h.pos[i]
		if p.Symbol == t.Symbol && p.MarkPrice != mark {
			p.MarkPrice = mark
			dir := 1.0
			if p.Side == "Sell" {
				dir = -1
			}
			p.UnrealisedPnl = (mark - p.EntryPrice) * p.Size * dir
			changed = true
		}
		totalUPL += p.UnrealisedPnl
	}
	if changed {
		h.bal.UnrealisedPnl = totalUPL
		h.bal.Equity = h.bal.WalletBalance + totalUPL
		h.updated = time.Now()
	}
	h.mu.Unlock()

	if changed {
		h.markDirty()
	}
}

// onWallet applies pushed account totals.
func (h *liveHub) onWallet(_ string, data json.RawMessage) {
	var accounts []struct {
		AccountType           string `json:"accountType"`
		TotalEquity           string `json:"totalEquity"`
		TotalWalletBalance    string `json:"totalWalletBalance"`
		TotalPerpUPL          string `json:"totalPerpUPL"`
		TotalAvailableBalance string `json:"totalAvailableBalance"`
	}
	if err := json.Unmarshal(data, &accounts); err != nil {
		return
	}
	for _, a := range accounts {
		if a.AccountType != "UNIFIED" {
			continue
		}
		h.mu.Lock()
		h.bal.Equity = pf(a.TotalEquity)
		h.bal.WalletBalance = pf(a.TotalWalletBalance)
		h.bal.UnrealisedPnl = pf(a.TotalPerpUPL)
		h.bal.Available = pf(a.TotalAvailableBalance)
		h.updated = time.Now()
		h.mu.Unlock()
		h.markDirty()
	}
}

func (h *liveHub) overviewJSON() ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.ready {
		return nil, false
	}
	bal := h.bal
	pos := make([]position, len(h.pos))
	copy(pos, h.pos)
	b, err := json.Marshal(overview{Balance: &bal, Positions: pos, UpdatedAt: h.updated})
	return b, err == nil
}

// broadcaster fans out snapshots to connected clients, throttled.
func (h *liveHub) broadcaster(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.dirty:
		}

		msg, ok := h.overviewJSON()
		if !ok {
			continue
		}
		h.cmu.Lock()
		for conn := range h.clients {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				conn.Close()
				delete(h.clients, conn)
			}
		}
		h.cmu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(livePushThrottle):
		}
	}
}

var upgrader = websocket.Upgrader{
	// Auth happens via Telegram initData in the first message, so any
	// origin may attempt the handshake.
	CheckOrigin: func(*http.Request) bool { return true },
}

// handleWS upgrades the connection, expects {"initData": "..."} as the first
// message, then streams overview snapshots until the client goes away.
func (h *liveHub) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	conn.SetReadDeadline(time.Now().Add(liveAuthTimeout))
	var hello struct {
		InitData string `json:"initData"`
	}
	if err := conn.ReadJSON(&hello); err != nil {
		conn.Close()
		return
	}
	userID, err := validateInitData(hello.InitData, h.srv.botToken)
	if err != nil || userID != h.srv.allowedID {
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unauthorized"))
		conn.Close()
		return
	}

	// Immediate snapshot so the UI updates without waiting for a tick.
	if msg, ok := h.overviewJSON(); ok {
		conn.WriteMessage(websocket.TextMessage, msg)
	}

	h.cmu.Lock()
	h.clients[conn] = struct{}{}
	h.cmu.Unlock()

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	go h.pingLoop(conn)

	// Drain client messages; exit (and unregister) on error.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	}
	h.cmu.Lock()
	delete(h.clients, conn)
	h.cmu.Unlock()
	conn.Close()
}

func (h *liveHub) pingLoop(conn *websocket.Conn) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for range tick.C {
		// Writes are serialized under cmu (shared with the broadcaster):
		// gorilla/websocket allows only one concurrent writer per conn.
		h.cmu.Lock()
		_, alive := h.clients[conn]
		var err error
		if alive {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err = conn.WriteMessage(websocket.PingMessage, nil)
		}
		h.cmu.Unlock()
		if !alive || err != nil {
			return
		}
	}
}
