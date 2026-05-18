package bybit

import (
	"encoding/json"
	"time"
)

// Candle represents a single OHLCV candlestick.
type Candle struct {
	Time     time.Time
	Symbol   string
	Interval string
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// Ticker represents a 24h market ticker snapshot.
type Ticker struct {
	Symbol                string
	LastPrice             float64
	Volume24h             float64
	PriceChangePercent24h float64
}

// WSMessage is the top-level websocket message envelope.
type WSMessage struct {
	Op    string          `json:"op"`
	Topic string          `json:"topic"`
	Type  string          `json:"type"`
	Ts    int64           `json:"ts"`
	Data  json.RawMessage `json:"data"`
}

// KlineData represents a kline entry received over websocket.
type KlineData struct {
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Interval string `json:"interval"`
	Open     string `json:"open"`
	Close    string `json:"close"`
	High     string `json:"high"`
	Low      string `json:"low"`
	Volume   string `json:"volume"`
	Confirm  bool   `json:"confirm"`
}

// KlineWSResponse is the websocket response containing kline data.
type KlineWSResponse struct {
	Topic string      `json:"topic"`
	Data  []KlineData `json:"data"`
	Ts    int64       `json:"ts"`
}

// TickerData represents ticker data received over websocket.
type TickerData struct {
	Symbol            string `json:"symbol"`
	LastPrice         string `json:"lastPrice"`
	Volume24h         string `json:"volume24h"`
	Price24hPcnt      string `json:"price24hPcnt"`
	HighPrice24h      string `json:"highPrice24h"`
	LowPrice24h       string `json:"lowPrice24h"`
	PrevPrice24h      string `json:"prevPrice24h"`
	OpenInterest      string `json:"openInterest"`
	OpenInterestValue string `json:"openInterestValue"`
}

// TickerWSResponse is the websocket response for ticker data.
type TickerWSResponse struct {
	Topic string     `json:"topic"`
	Data  TickerData `json:"data"`
	Ts    int64      `json:"ts"`
}

// RESTResponse is a generic REST API response wrapper.
type RESTResponse[T any] struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  T      `json:"result"`
	Time    int64  `json:"time"`
}

// KlineRESTResult is the result field of a kline REST response.
type KlineRESTResult struct {
	Symbol   string     `json:"symbol"`
	Category string     `json:"category"`
	List     [][]string `json:"list"` // [time, open, high, low, close, volume, turnover]
}

// KlineRESTItem is a single kline entry from the REST API (parsed from List).
type KlineRESTItem struct {
	StartTime int64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	Turnover  float64
}

// TickerRESTResult is the result field of a tickers REST response.
type TickerRESTResult struct {
	Category string            `json:"category"`
	List     []TickerRESTEntry `json:"list"`
}

// TickerRESTEntry is a single ticker from the REST API list.
type TickerRESTEntry struct {
	Symbol        string `json:"symbol"`
	LastPrice     string `json:"lastPrice"`
	Volume24h     string `json:"volume24h"`
	Turnover24h   string `json:"turnover24h"`
	Price24hPcnt  string `json:"price24hPcnt"`
}

// WSPingMessage is used for keepalive pings.
type WSPingMessage struct {
	Op string `json:"op"`
}

// WSSubscribeMessage is used to subscribe to topics.
type WSSubscribeMessage struct {
	Op   string   `json:"op"`
	Args []string `json:"args"`
}

// WSResponseMessage is an acknowledgement from the server.
type WSResponseMessage struct {
	Success bool   `json:"success"`
	RetMsg  string `json:"ret_msg"`
	Op      string `json:"op"`
	ConnID  string `json:"conn_id"`
}
