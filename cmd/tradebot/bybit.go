package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

const recvWindow = "5000"

// ── Client ────────────────────────────────────────────────────────────────────

type BybitClient struct {
	base   string
	key    string
	secret string
	hc     *http.Client
}

func newBybitClient(cfg *Config) *BybitClient {
	base := "https://api.bybit.com"
	if cfg.Testnet {
		base = "https://api-testnet.bybit.com"
	}
	dialer := bybit.NewResilientDialer()
	dialer.KeepWarm(strings.TrimPrefix(base, "https://"), 5*time.Minute)
	return &BybitClient{
		base:   base,
		key:    cfg.APIKey,
		secret: cfg.APISecret,
		hc: &http.Client{
			Timeout: 12 * time.Second,
			Transport: &http.Transport{
				DialContext:       dialer.DialContext,
				DisableKeepAlives: true,
			},
		},
	}
}

// doWithRetry executes fn up to 3 times on transient network errors.
func doWithRetry(fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	const maxAttempts = 3
	var lastErr error
	for i := range maxAttempts {
		raw, err := fn()
		if err == nil {
			return raw, nil
		}
		lastErr = err
		// Only retry on network/timeout errors, not Bybit API errors.
		if !isTransient(err) {
			return nil, err
		}
		if i < maxAttempts-1 {
			time.Sleep(time.Duration(2*(i+1)) * time.Second) // 2s, 4s
		}
	}
	return nil, lastErr
}

// isTransient returns true for errors worth retrying (DNS, timeout, connection refused).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{"dial tcp", "lookup", "timeout", "deadline", "connection refused", "EOF", "reset by peer"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// ── Signing ───────────────────────────────────────────────────────────────────

func (c *BybitClient) sign(param string) (sig, ts string) {
	ts = strconv.FormatInt(time.Now().UnixMilli(), 10)
	msg := ts + c.key + recvWindow + param
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(msg))
	sig = hex.EncodeToString(mac.Sum(nil))
	return
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

type bybitEnvelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
}

func (c *BybitClient) get(path, query string) (json.RawMessage, error) {
	return doWithRetry(func() (json.RawMessage, error) {
		sig, ts := c.sign(query)
		u := c.base + path
		if query != "" {
			u += "?" + query
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("X-BAPI-API-KEY", c.key)
		req.Header.Set("X-BAPI-TIMESTAMP", ts)
		req.Header.Set("X-BAPI-SIGN", sig)
		req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)

		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return checkResp(resp.Body)
	})
}

func (c *BybitClient) post(path string, payload interface{}) (json.RawMessage, error) {
	return doWithRetry(func() (json.RawMessage, error) {
		b, _ := json.Marshal(payload)
		bodyStr := string(b)
		sig, ts := c.sign(bodyStr)

		req, _ := http.NewRequest("POST", c.base+path, strings.NewReader(bodyStr))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-BAPI-API-KEY", c.key)
		req.Header.Set("X-BAPI-TIMESTAMP", ts)
		req.Header.Set("X-BAPI-SIGN", sig)
		req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)

		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return checkResp(resp.Body)
	})
}

func checkResp(body io.Reader) (json.RawMessage, error) {
	raw, _ := io.ReadAll(body)
	var env bybitEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if env.RetCode != 0 {
		return nil, fmt.Errorf("bybit %d: %s", env.RetCode, env.RetMsg)
	}
	return env.Result, nil
}

// ── Balance ───────────────────────────────────────────────────────────────────

type WalletInfo struct {
	TotalEquity        float64
	TotalWalletBalance float64 // without unrealised PnL
	TotalUnrealisedPnl float64 // sum of open position PnL
	TotalAvailBalance  float64
	Coins              []CoinBalance
}

type CoinBalance struct {
	Coin           string
	WalletBalance  float64
	Available      float64
	UnrealisedPnl  float64
}

func (c *BybitClient) Balance() (*WalletInfo, error) {
	raw, err := c.get("/v5/account/wallet-balance", "accountType=UNIFIED")
	if err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			TotalEquity            string `json:"totalEquity"`
			TotalWalletBalance     string `json:"totalWalletBalance"`
			TotalUnrealisedPnl     string `json:"totalPerpUPL"`
			TotalAvailableBalance  string `json:"totalAvailableBalance"`
			Coin                   []struct {
				Coin            string `json:"coin"`
				WalletBalance   string `json:"walletBalance"`
				AvailToWithdraw string `json:"availToWithdraw"`
				UnrealisedPnl   string `json:"unrealisedPnl"`
			} `json:"coin"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.List) == 0 {
		return nil, fmt.Errorf("parse balance: %w", err)
	}
	acc := res.List[0]
	wi := &WalletInfo{
		TotalEquity:        pf(acc.TotalEquity),
		TotalWalletBalance: pf(acc.TotalWalletBalance),
		TotalUnrealisedPnl: pf(acc.TotalUnrealisedPnl),
		TotalAvailBalance:  pf(acc.TotalAvailableBalance),
	}
	for _, coin := range acc.Coin {
		bal := pf(coin.WalletBalance)
		if bal == 0 {
			continue
		}
		wi.Coins = append(wi.Coins, CoinBalance{
			Coin:          coin.Coin,
			WalletBalance: bal,
			Available:     pf(coin.AvailToWithdraw),
			UnrealisedPnl: pf(coin.UnrealisedPnl),
		})
	}
	return wi, nil
}

// ── Instrument info ───────────────────────────────────────────────────────────

type InstrumentInfo struct {
	QtyStep     float64
	MinOrderQty float64
	TickSize    float64
}

func (c *BybitClient) InstrumentInfo(symbol string) (*InstrumentInfo, error) {
	raw, err := c.get("/v5/market/instruments-info",
		"category=linear&symbol="+symbol)
	if err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			LotSizeFilter struct {
				QtyStep     string `json:"qtyStep"`
				MinOrderQty string `json:"minOrderQty"`
			} `json:"lotSizeFilter"`
			PriceFilter struct {
				TickSize string `json:"tickSize"`
			} `json:"priceFilter"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.List) == 0 {
		return nil, fmt.Errorf("instrument not found: %s", symbol)
	}
	l := res.List[0]
	return &InstrumentInfo{
		QtyStep:     pf(l.LotSizeFilter.QtyStep),
		MinOrderQty: pf(l.LotSizeFilter.MinOrderQty),
		TickSize:    pf(l.PriceFilter.TickSize),
	}, nil
}

// ── Positions ─────────────────────────────────────────────────────────────────

type Position struct {
	Symbol        string
	Side          string
	Size          float64
	EntryPrice    float64
	Leverage      float64
	UnrealisedPnl float64
	MarkPrice     float64
}

func (c *BybitClient) Positions() ([]Position, error) {
	raw, err := c.get("/v5/position/list", "category=linear&settleCoin=USDT")
	if err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Size          string `json:"size"`
			AvgPrice      string `json:"avgPrice"`
			Leverage      string `json:"leverage"`
			UnrealisedPnl string `json:"unrealisedPnl"`
			MarkPrice     string `json:"markPrice"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range res.List {
		sz := pf(p.Size)
		if sz == 0 {
			continue
		}
		out = append(out, Position{
			Symbol:        p.Symbol,
			Side:          p.Side,
			Size:          sz,
			EntryPrice:    pf(p.AvgPrice),
			Leverage:      pf(p.Leverage),
			UnrealisedPnl: pf(p.UnrealisedPnl),
			MarkPrice:     pf(p.MarkPrice),
		})
	}
	return out, nil
}

// ── Set leverage ──────────────────────────────────────────────────────────────

func (c *BybitClient) SetLeverage(symbol string, lev float64) error {
	levStr := strconv.FormatFloat(lev, 'f', 0, 64)
	_, err := c.post("/v5/position/set-leverage", map[string]string{
		"category":     "linear",
		"symbol":       symbol,
		"buyLeverage":  levStr,
		"sellLeverage": levStr,
	})
	// 110043 = "leverage not modified" — harmless
	if err != nil && strings.Contains(err.Error(), "110043") {
		return nil
	}
	return err
}

// ── Place order ───────────────────────────────────────────────────────────────

type OrderResult struct {
	OrderID string
}

func (c *BybitClient) PlaceOrder(symbol, side, qty, price, sl, tp string) (*OrderResult, error) {
	payload := map[string]string{
		"category":    "linear",
		"symbol":      symbol,
		"side":        side,
		"orderType":   "Limit",
		"qty":         qty,
		"price":       price,
		"timeInForce": "GTC",
		"stopLoss":    sl,
		"takeProfit":  tp,
	}
	raw, err := c.post("/v5/order/create", payload)
	if err != nil {
		return nil, err
	}
	var res struct {
		OrderID string `json:"orderId"`
	}
	json.Unmarshal(raw, &res)
	return &OrderResult{OrderID: res.OrderID}, nil
}

// ── Open orders ───────────────────────────────────────────────────────────────

type OpenOrder struct {
	OrderID     string
	Symbol      string
	Side        string
	Price       float64
	Qty         float64
	CreatedTime time.Time
	ReduceOnly  bool   // closes/reduces a position (SL/TP legs are reduce-only)
	StopType    string // "StopLoss" / "TakeProfit" / "" for regular orders
}

// IsProtective reports whether the order guards an open position (SL/TP or
// any reduce-only leg) rather than waiting to open one.
func (o *OpenOrder) IsProtective() bool {
	return o.ReduceOnly || o.StopType != ""
}

func (c *BybitClient) OpenOrders() ([]OpenOrder, error) {
	raw, err := c.get("/v5/order/realtime", "category=linear&settleCoin=USDT&limit=50")
	if err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			OrderID       string `json:"orderId"`
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Price         string `json:"price"`
			Qty           string `json:"qty"`
			CreatedTime   string `json:"createdTime"`
			ReduceOnly    bool   `json:"reduceOnly"`
			StopOrderType string `json:"stopOrderType"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var out []OpenOrder
	for _, o := range res.List {
		ms, _ := strconv.ParseInt(o.CreatedTime, 10, 64)
		out = append(out, OpenOrder{
			OrderID:     o.OrderID,
			Symbol:      o.Symbol,
			Side:        o.Side,
			Price:       pf(o.Price),
			Qty:         pf(o.Qty),
			CreatedTime: time.UnixMilli(ms).UTC(),
			ReduceOnly:  o.ReduceOnly,
			StopType:    o.StopOrderType,
		})
	}
	return out, nil
}

func (c *BybitClient) CancelOrder(symbol, orderID string) error {
	_, err := c.post("/v5/order/cancel", map[string]string{
		"category": "linear",
		"symbol":   symbol,
		"orderId":  orderID,
	})
	return err
}

// ── Cancel all ────────────────────────────────────────────────────────────────

func (c *BybitClient) CancelAll() (int, error) {
	raw, err := c.post("/v5/order/cancel-all", map[string]string{
		"category":   "linear",
		"settleCoin": "USDT",
	})
	if err != nil {
		return 0, err
	}
	var res struct {
		List []json.RawMessage `json:"list"`
	}
	json.Unmarshal(raw, &res)
	return len(res.List), nil
}

// ── Closed PnL ────────────────────────────────────────────────────────────────

type ClosedTrade struct {
	Symbol     string
	Side       string
	Qty        float64
	EntryPrice float64
	ExitPrice  float64
	ClosedPnl  float64
	ClosedAt   time.Time
}

func (c *BybitClient) ClosedPnl(limit int) ([]ClosedTrade, error) {
	raw, err := c.get("/v5/position/closed-pnl",
		fmt.Sprintf("category=linear&limit=%d", limit))
	if err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			Symbol     string `json:"symbol"`
			Side       string `json:"side"`
			Qty        string `json:"qty"`
			AvgEntryPrice string `json:"avgEntryPrice"`
			AvgExitPrice  string `json:"avgExitPrice"`
			ClosedPnl  string `json:"closedPnl"`
			UpdatedTime string `json:"updatedTime"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := make([]ClosedTrade, 0, len(res.List))
	for _, t := range res.List {
		ms, _ := strconv.ParseInt(t.UpdatedTime, 10, 64)
		out = append(out, ClosedTrade{
			Symbol:     t.Symbol,
			Side:       t.Side,
			Qty:        pf(t.Qty),
			EntryPrice: pf(t.AvgEntryPrice),
			ExitPrice:  pf(t.AvgExitPrice),
			ClosedPnl:  pf(t.ClosedPnl),
			ClosedAt:   time.UnixMilli(ms).UTC(),
		})
	}
	return out, nil
}

// ClosedPnlRecord is a full closed-pnl entry used by the trade sync.
type ClosedPnlRecord struct {
	OrderID       string
	Symbol        string
	Side          string // side of the closing order
	Qty           float64
	EntryPrice    float64
	ExitPrice     float64
	ClosedPnl     float64
	Leverage      float64
	CumEntryValue float64
	CreatedAt     time.Time
	ClosedAt      time.Time
}

// ClosedPnlRange pages through /v5/position/closed-pnl for [startMs, endMs]
// (Bybit caps a single window at 7 days). Returns records and the next cursor.
func (c *BybitClient) ClosedPnlRange(startMs, endMs int64, cursor string) ([]ClosedPnlRecord, string, error) {
	q := fmt.Sprintf("category=linear&limit=100&startTime=%d&endTime=%d", startMs, endMs)
	if cursor != "" {
		q += "&cursor=" + url.QueryEscape(cursor)
	}
	raw, err := c.get("/v5/position/closed-pnl", q)
	if err != nil {
		return nil, "", err
	}
	var res struct {
		NextPageCursor string `json:"nextPageCursor"`
		List           []struct {
			OrderID       string `json:"orderId"`
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Qty           string `json:"qty"`
			AvgEntryPrice string `json:"avgEntryPrice"`
			AvgExitPrice  string `json:"avgExitPrice"`
			ClosedPnl     string `json:"closedPnl"`
			Leverage      string `json:"leverage"`
			CumEntryValue string `json:"cumEntryValue"`
			CreatedTime   string `json:"createdTime"`
			UpdatedTime   string `json:"updatedTime"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, "", err
	}
	out := make([]ClosedPnlRecord, 0, len(res.List))
	for _, t := range res.List {
		created, _ := strconv.ParseInt(t.CreatedTime, 10, 64)
		updated, _ := strconv.ParseInt(t.UpdatedTime, 10, 64)
		out = append(out, ClosedPnlRecord{
			OrderID:       t.OrderID,
			Symbol:        t.Symbol,
			Side:          t.Side,
			Qty:           pf(t.Qty),
			EntryPrice:    pf(t.AvgEntryPrice),
			ExitPrice:     pf(t.AvgExitPrice),
			ClosedPnl:     pf(t.ClosedPnl),
			Leverage:      pf(t.Leverage),
			CumEntryValue: pf(t.CumEntryValue),
			CreatedAt:     time.UnixMilli(created).UTC(),
			ClosedAt:      time.UnixMilli(updated).UTC(),
		})
	}
	return out, res.NextPageCursor, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// pf parses a float from Bybit's string fields.
func pf(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// roundStep floors val to the nearest multiple of step.
func roundStep(val, step float64) float64 {
	if step == 0 {
		return val
	}
	return math.Floor(val/step) * step
}

// decimals counts decimal places implied by a step value (e.g. 0.01 → 2).
func decimals(step float64) int {
	s := strings.TrimRight(strconv.FormatFloat(step, 'f', 10, 64), "0")
	if idx := strings.Index(s, "."); idx >= 0 {
		return len(s) - idx - 1
	}
	return 0
}

// ff formats a float with the given decimal precision.
func ff(val float64, prec int) string {
	return strconv.FormatFloat(val, 'f', prec, 64)
}
