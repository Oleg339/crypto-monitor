package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

const recvWindow = "5000"

// bybitClient is a read-only Bybit v5 client: balance and positions only.
type bybitClient struct {
	base   string
	key    string
	secret string
	hc     *http.Client
}

func newBybitClient(base, key, secret string) *bybitClient {
	dialer := bybit.NewResilientDialer()
	dialer.KeepWarm("api.bybit.com", 5*time.Minute)
	return &bybitClient{
		base:   base,
		key:    key,
		secret: secret,
		hc: &http.Client{
			Timeout: 12 * time.Second,
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
		},
	}
}

func (c *bybitClient) get(path, query string) (json.RawMessage, error) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(ts + c.key + recvWindow + query))
	sig := hex.EncodeToString(mac.Sum(nil))

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

	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		RetCode int             `json:"retCode"`
		RetMsg  string          `json:"retMsg"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if env.RetCode != 0 {
		return nil, fmt.Errorf("bybit %d: %s", env.RetCode, env.RetMsg)
	}
	return env.Result, nil
}

func pf(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

type balanceInfo struct {
	Equity        float64 `json:"equity"`
	WalletBalance float64 `json:"walletBalance"`
	UnrealisedPnl float64 `json:"unrealisedPnl"`
	Available     float64 `json:"available"`
}

func (c *bybitClient) balance() (*balanceInfo, error) {
	raw, err := c.get("/v5/account/wallet-balance", "accountType=UNIFIED")
	if err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			TotalEquity           string `json:"totalEquity"`
			TotalWalletBalance    string `json:"totalWalletBalance"`
			TotalPerpUPL          string `json:"totalPerpUPL"`
			TotalAvailableBalance string `json:"totalAvailableBalance"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.List) == 0 {
		return nil, fmt.Errorf("parse balance: %w", err)
	}
	acc := res.List[0]
	return &balanceInfo{
		Equity:        pf(acc.TotalEquity),
		WalletBalance: pf(acc.TotalWalletBalance),
		UnrealisedPnl: pf(acc.TotalPerpUPL),
		Available:     pf(acc.TotalAvailableBalance),
	}, nil
}

type position struct {
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Size          float64 `json:"size"`
	EntryPrice    float64 `json:"entryPrice"`
	MarkPrice     float64 `json:"markPrice"`
	Leverage      float64 `json:"leverage"`
	UnrealisedPnl float64 `json:"unrealisedPnl"`
	StopLoss      float64 `json:"stopLoss"`
	TakeProfit    float64 `json:"takeProfit"`
}

// openOrder — лимитный ордер на вход, ещё не исполненный биржей.
type openOrder struct {
	OrderID   string    `json:"orderId"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Price     float64   `json:"price"`
	Qty       float64   `json:"qty"`
	StopLoss  float64   `json:"stopLoss"`
	TakeProfit float64  `json:"takeProfit"`
	CreatedAt time.Time `json:"createdAt"`
	LastPrice float64   `json:"lastPrice"` // текущая цена — чтобы видеть, далеко ли до исполнения
}

// openOrders возвращает ожидающие лимитники на вход (без SL/TP-ног позиций).
func (c *bybitClient) openOrders() ([]openOrder, error) {
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
			StopLoss      string `json:"stopLoss"`
			TakeProfit    string `json:"takeProfit"`
			CreatedTime   string `json:"createdTime"`
			ReduceOnly    bool   `json:"reduceOnly"`
			StopOrderType string `json:"stopOrderType"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := []openOrder{}
	for _, o := range res.List {
		// SL/TP-ноги открытых позиций — не «ожидающие входы», пропускаем
		if o.ReduceOnly || o.StopOrderType != "" {
			continue
		}
		ms, _ := strconv.ParseInt(o.CreatedTime, 10, 64)
		out = append(out, openOrder{
			OrderID:    o.OrderID,
			Symbol:     o.Symbol,
			Side:       o.Side,
			Price:      pf(o.Price),
			Qty:        pf(o.Qty),
			StopLoss:   pf(o.StopLoss),
			TakeProfit: pf(o.TakeProfit),
			CreatedAt:  time.UnixMilli(ms),
		})
	}
	// подтягиваем текущие цены (ордеров мало — по одному запросу на символ)
	for i := range out {
		if lp, err := c.lastPrice(out[i].Symbol); err == nil {
			out[i].LastPrice = lp
		}
	}
	return out, nil
}

func (c *bybitClient) lastPrice(symbol string) (float64, error) {
	raw, err := c.get("/v5/market/tickers", "category=linear&symbol="+symbol)
	if err != nil {
		return 0, err
	}
	var res struct {
		List []struct {
			LastPrice string `json:"lastPrice"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.List) == 0 {
		return 0, fmt.Errorf("parse ticker: %w", err)
	}
	return pf(res.List[0].LastPrice), nil
}

func (c *bybitClient) positions() ([]position, error) {
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
			MarkPrice     string `json:"markPrice"`
			Leverage      string `json:"leverage"`
			UnrealisedPnl string `json:"unrealisedPnl"`
			StopLoss      string `json:"stopLoss"`
			TakeProfit    string `json:"takeProfit"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := []position{}
	for _, p := range res.List {
		sz := pf(p.Size)
		if sz == 0 {
			continue
		}
		out = append(out, position{
			Symbol:        p.Symbol,
			Side:          p.Side,
			Size:          sz,
			EntryPrice:    pf(p.AvgPrice),
			MarkPrice:     pf(p.MarkPrice),
			Leverage:      pf(p.Leverage),
			UnrealisedPnl: pf(p.UnrealisedPnl),
			StopLoss:      pf(p.StopLoss),
			TakeProfit:    pf(p.TakeProfit),
		})
	}
	return out, nil
}
