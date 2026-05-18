package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	telegramAPIBase  = "https://api.telegram.org"
	rateLimitDelay   = time.Second
)

// TelegramBot sends messages via the Telegram Bot API.
type TelegramBot struct {
	token    string
	chatID   string
	client   *http.Client
	lastSent time.Time
}

// NewTelegramBot creates a new TelegramBot.
func NewTelegramBot(token, chatID string) *TelegramBot {
	return &TelegramBot{
		token:  token,
		chatID: chatID,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// SendMessage sends a text message to the configured chat.
// It enforces a 1-second rate limit between messages.
func (b *TelegramBot) SendMessage(ctx context.Context, text string) error {
	// Enforce rate limit.
	since := time.Since(b.lastSent)
	if since < rateLimitDelay {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rateLimitDelay - since):
		}
	}

	payload := sendMessageRequest{
		ChatID:                b.chatID,
		Text:                  text,
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase, b.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram: read response: %w", err)
	}

	var tgResp telegramResponse
	if err := json.Unmarshal(respBody, &tgResp); err != nil {
		return fmt.Errorf("telegram: unmarshal response: %w", err)
	}

	b.lastSent = time.Now()

	if !tgResp.OK {
		return fmt.Errorf("telegram: API error: %s", tgResp.Description)
	}

	return nil
}
