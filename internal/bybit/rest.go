package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/yourorg/crypto-monitor/internal/config"
)

// RESTClient handles HTTP communication with the Bybit REST API.
type RESTClient struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	httpClient *http.Client
}

// NewRESTClient creates a new REST client from the provided configuration.
func NewRESTClient(cfg *config.Config) *RESTClient {
	return &RESTClient{
		baseURL:   cfg.RESTURL,
		apiKey:    cfg.BybitAPIKey,
		apiSecret: cfg.BybitAPISecret,
		httpClient: newResilientHTTPClient(cfg.RESTURL),
	}
}

// GetKlines fetches historical klines from the Bybit REST API.
// It handles pagination automatically and inserts a 100ms delay between requests.
func (c *RESTClient) GetKlines(ctx context.Context, symbol, interval string, start, end time.Time, limit int) ([]Candle, error) {
	const maxPerRequest = 200

	var allCandles []Candle

	currentEnd := end

	for {
		select {
		case <-ctx.Done():
			return allCandles, ctx.Err()
		default:
		}

		params := url.Values{}
		params.Set("category", "linear")
		params.Set("symbol", symbol)
		params.Set("interval", interval)
		params.Set("limit", strconv.Itoa(maxPerRequest))
		params.Set("start", strconv.FormatInt(start.UnixMilli(), 10))
		params.Set("end", strconv.FormatInt(currentEnd.UnixMilli(), 10))

		endpoint := c.baseURL + "/v5/market/kline?" + params.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("bybit rest: build request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("bybit rest: do request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bybit rest: read response body: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bybit rest: unexpected status %d: %s", resp.StatusCode, body)
		}

		var apiResp RESTResponse[KlineRESTResult]
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return nil, fmt.Errorf("bybit rest: unmarshal response: %w", err)
		}

		if apiResp.RetCode != 0 {
			return nil, fmt.Errorf("bybit rest: API error %d: %s", apiResp.RetCode, apiResp.RetMsg)
		}

		list := apiResp.Result.List
		if len(list) == 0 {
			break
		}

		batch := make([]Candle, 0, len(list))
		for _, item := range list {
			if len(item) < 7 {
				continue
			}

			tsMs, err := strconv.ParseInt(item[0], 10, 64)
			if err != nil {
				continue
			}
			t := time.UnixMilli(tsMs).UTC()
			if t.Before(start) || t.After(end) {
				continue
			}

			open, _ := strconv.ParseFloat(item[1], 64)
			high, _ := strconv.ParseFloat(item[2], 64)
			low, _ := strconv.ParseFloat(item[3], 64)
			close_, _ := strconv.ParseFloat(item[4], 64)
			vol, _ := strconv.ParseFloat(item[5], 64)

			batch = append(batch, Candle{
				Time:     t,
				Symbol:   symbol,
				Interval: interval,
				Open:     open,
				High:     high,
				Low:      low,
				Close:    close_,
				Volume:   vol,
			})
		}

		allCandles = append(allCandles, batch...)

		// Bybit returns newest first; determine the oldest candle in this batch.
		oldestInBatch := batch[len(batch)-1].Time
		if len(batch) < maxPerRequest || !oldestInBatch.After(start) {
			break
		}

		// Move the window backward by one millisecond to avoid duplicates.
		currentEnd = oldestInBatch.Add(-time.Millisecond)

		if limit > 0 && len(allCandles) >= limit {
			break
		}

		// Respect rate limit.
		time.Sleep(100 * time.Millisecond)
	}

	// Sort ascending by time.
	sort.Slice(allCandles, func(i, j int) bool {
		return allCandles[i].Time.Before(allCandles[j].Time)
	})

	if limit > 0 && len(allCandles) > limit {
		allCandles = allCandles[len(allCandles)-limit:]
	}

	return allCandles, nil
}

// GetTopSymbols returns the top symbols by 24h turnover (USDT volume).
func (c *RESTClient) GetTopSymbols(ctx context.Context, limit int) ([]string, error) {
	params := url.Values{}
	params.Set("category", "linear")

	endpoint := c.baseURL + "/v5/market/tickers?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("bybit rest: build tickers request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bybit rest: do tickers request: %w", err)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("bybit rest: read tickers response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bybit rest: tickers unexpected status %d: %s", resp.StatusCode, body)
	}

	var apiResp RESTResponse[TickerRESTResult]
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("bybit rest: unmarshal tickers response: %w", err)
	}

	if apiResp.RetCode != 0 {
		return nil, fmt.Errorf("bybit rest: tickers API error %d: %s", apiResp.RetCode, apiResp.RetMsg)
	}

	type symbolVolume struct {
		symbol   string
		turnover float64
	}

	entries := make([]symbolVolume, 0, len(apiResp.Result.List))
	for _, e := range apiResp.Result.List {
		// Only include perpetual USDT pairs.
		if len(e.Symbol) < 5 || e.Symbol[len(e.Symbol)-4:] != "USDT" {
			continue
		}
		t, _ := strconv.ParseFloat(e.Turnover24h, 64)
		entries = append(entries, symbolVolume{symbol: e.Symbol, turnover: t})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].turnover > entries[j].turnover
	})

	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	symbols := make([]string, len(entries))
	for i, e := range entries {
		symbols[i] = e.symbol
	}

	return symbols, nil
}
