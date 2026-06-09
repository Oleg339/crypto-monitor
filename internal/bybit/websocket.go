package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingInterval      = 20 * time.Second
	maxReconnectDelay = 60 * time.Second
	initialBackoff    = time.Second
	semaphoreSize     = 10
	batchSize         = 10
)

// MessageHandler is a callback invoked for each incoming message matching a topic prefix.
type MessageHandler func(topic string, data json.RawMessage)

// WSClient manages a websocket connection to Bybit with auto-reconnect.
type WSClient struct {
	url        string
	dialer     *ResilientDialer
	conn       *websocket.Conn
	mu         sync.RWMutex
	handlers   map[string]MessageHandler
	semaphore  chan struct{}
	reconnects int64
	messages   int64
}

// NewWSClient creates a new websocket client.
func NewWSClient(wsURL string) *WSClient {
	dialer := NewResilientDialer()
	if u, err := url.Parse(wsURL); err == nil && u.Hostname() != "" {
		dialer.KeepWarm(u.Hostname(), 5*time.Minute)
	}
	return &WSClient{
		url:       wsURL,
		dialer:    dialer,
		handlers:  make(map[string]MessageHandler),
		semaphore: make(chan struct{}, semaphoreSize),
	}
}

// OnMessage registers a handler for topics that start with prefix.
func (c *WSClient) OnMessage(prefix string, handler MessageHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[prefix] = handler
}

// Connect establishes the websocket connection.
func (c *WSClient) Connect(ctx context.Context) error {
	dialer := &websocket.Dialer{
		Proxy:            websocket.DefaultDialer.Proxy,
		HandshakeTimeout: websocket.DefaultDialer.HandshakeTimeout,
		NetDialContext:   c.dialer.DialContext,
	}
	conn, _, err := dialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("ws: dial %s: %w", c.url, err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	return nil
}

// Subscribe sends a subscribe message for the given topics (batched by batchSize).
func (c *WSClient) Subscribe(topics []string) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("ws: not connected")
	}

	for i := 0; i < len(topics); i += batchSize {
		end := i + batchSize
		if end > len(topics) {
			end = len(topics)
		}

		msg := WSSubscribeMessage{
			Op:   "subscribe",
			Args: topics[i:end],
		}

		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("ws: marshal subscribe: %w", err)
		}

		c.mu.Lock()
		err = conn.WriteMessage(websocket.TextMessage, data)
		c.mu.Unlock()
		if err != nil {
			return fmt.Errorf("ws: write subscribe: %w", err)
		}
	}

	return nil
}

// Reconnects returns the total number of reconnections performed.
func (c *WSClient) Reconnects() int64 { return atomic.LoadInt64(&c.reconnects) }

// Messages returns the total number of messages received.
func (c *WSClient) Messages() int64 { return atomic.LoadInt64(&c.messages) }

// Run starts the read loop, reconnecting on failure. It blocks until ctx is cancelled.
func (c *WSClient) Run(ctx context.Context) error {
	backoff := initialBackoff

	for {
		select {
		case <-ctx.Done():
			c.closeConn()
			return ctx.Err()
		default:
		}

		if err := c.Connect(ctx); err != nil {
			slog.Error("ws: connect failed", "err", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
			continue
		}

		atomic.AddInt64(&c.reconnects, 1)
		slog.Info("ws: connected", "url", c.url)
		backoff = initialBackoff

		if err := c.readLoop(ctx); err != nil {
			if ctx.Err() != nil {
				c.closeConn()
				return ctx.Err()
			}
			slog.Error("ws: read loop error", "err", err, "backoff", backoff)
			c.closeConn()
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
		}
	}
}

func (c *WSClient) readLoop(ctx context.Context) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	// Start ping goroutine.
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ping := WSPingMessage{Op: "ping"}
				data, _ := json.Marshal(ping)
				c.mu.Lock()
				err := conn.WriteMessage(websocket.TextMessage, data)
				c.mu.Unlock()
				if err != nil {
					slog.Warn("ws: ping failed", "err", err)
					return
				}
			}
		}
	}()

	defer func() { <-pingDone }()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ws: read: %w", err)
		}

		atomic.AddInt64(&c.messages, 1)

		// Acquire semaphore slot.
		select {
		case c.semaphore <- struct{}{}:
		case <-ctx.Done():
			return nil
		}

		go func(payload []byte) {
			defer func() { <-c.semaphore }()
			c.dispatch(payload)
		}(raw)
	}
}

func (c *WSClient) dispatch(raw []byte) {
	var msg WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("ws: unmarshal message", "err", err)
		return
	}

	// Ignore pong / subscription confirmations.
	if msg.Op == "pong" || msg.Op == "subscribe" {
		return
	}

	if msg.Topic == "" {
		return
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	for prefix, handler := range c.handlers {
		if strings.HasPrefix(msg.Topic, prefix) {
			handler(msg.Topic, msg.Data)
			return
		}
	}
}

func (c *WSClient) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// sleep waits for d or until ctx is cancelled. Returns false if ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}
