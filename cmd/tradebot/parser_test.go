package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

// TestParseRealTelegramMessages tests the parser against real Telegram messages
func TestParseRealTelegramMessages(t *testing.T) {
	// Load config
	if err := godotenv.Load(); err != nil {
		t.Logf("Warning: .env not found, using environment variables")
	}

	cfg := loadTestConfig()
	if cfg.AppID == 0 || cfg.AppHash == "" || cfg.Phone == "" {
		t.Skip("Telegram credentials not provided. Set TELEGRAM_APP_ID, TELEGRAM_APP_HASH, TELEGRAM_PHONE in .env")
	}

	if cfg.Trader3Channel == "" {
		t.Skip("TRADER3_CHANNEL not provided in .env")
	}

	parser := &SignalParser{
		channelID: parseChannelID(cfg.Trader3Channel),
		threadID:  cfg.Trader3ThreadID,
		signals:   make([]ParsedSignal, 0),
		messages:  make([]string, 0),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Logf("Connecting to Telegram channel: %s (thread: %d)", cfg.Trader3Channel, cfg.Trader3ThreadID)
	
	err := parser.Run(ctx, cfg)
	if err != nil && err != context.DeadlineExceeded {
		t.Fatalf("Failed to run parser: %v", err)
	}

	t.Logf("\n=== PARSING RESULTS ===")
	t.Logf("Total messages processed: %d", len(parser.messages))
	t.Logf("Valid signals found: %d", len(parser.signals))
	
	if len(parser.messages) == 0 {
		t.Log("No messages received. Channel might be inactive or credentials incorrect.")
		return
	}

	t.Logf("\n=== RAW MESSAGES ===")
	for i, msg := range parser.messages {
		t.Logf("\nMessage %d:\n%s", i+1, msg)
		t.Logf("---")
	}

	t.Logf("\n=== PARSED SIGNALS ===")
	for i, sig := range parser.signals {
		t.Logf("\nSignal %d:", i+1)
		t.Logf("  ID: %d", sig.ID)
		t.Logf("  Symbol: %s", sig.Symbol)
		t.Logf("  Direction: %s", sig.Direction)
		t.Logf("  Entry: %.6g - %.6g", sig.EntryLow, sig.EntryHigh)
		t.Logf("  SL: %.6g", sig.SL)
		t.Logf("  TPs: %v", sig.TPs)
		
		// Calculate percentages
		mid := (sig.EntryLow + sig.EntryHigh) / 2
		slPct := 0.0
		if mid > 0 {
			slPct = (mid - sig.SL) / mid * 100
		}
		t.Logf("  SL%%: -%.1f%%", slPct)
		
		if len(sig.TPs) > 0 {
			for j, tp := range sig.TPs {
				tpPct := 0.0
				if mid > 0 {
					tpPct = (tp - mid) / mid * 100
				}
				t.Logf("  TP%d%%: +%.1f%%", j+1, tpPct)
			}
		}
		
		// Show formatted alert
		alert := formatSignalAlert(&sig)
		t.Logf("\nFormatted Alert:\n%s", alert)
		t.Logf("---")
	}

	// Statistics
	if len(parser.messages) > 0 {
		parseRate := float64(len(parser.signals)) / float64(len(parser.messages)) * 100
		t.Logf("\n=== STATISTICS ===")
		t.Logf("Parse success rate: %.1f%% (%d/%d)", parseRate, len(parser.signals), len(parser.messages))
	}
}

// SignalParser collects and parses messages from Telegram
type SignalParser struct {
	channelID int64
	threadID  int
	signals   []ParsedSignal
	messages  []string
}

func (p *SignalParser) Run(ctx context.Context, cfg *testConfig) error {
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, upd *tg.UpdateNewChannelMessage) error {
		return p.handleMsg(upd)
	})

	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: "session_parser_test.json"},
		UpdateHandler:  dispatcher,
	})

	return client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			auth.Constant(cfg.Phone, "", &testCodeReader{}),
			auth.SendCodeOptions{},
		)
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		
		log.Printf("[test] authenticated — monitoring channel ID=%d", p.channelID)

		// Wait for messages
		<-ctx.Done()
		return ctx.Err()
	})
}

func (p *SignalParser) handleMsg(upd *tg.UpdateNewChannelMessage) error {
	msg, ok := upd.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	// Channel filter
	if p.channelID != 0 {
		peer, ok := msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != p.channelID {
			return nil
		}
	}

	// Thread filter
	if p.threadID != 0 {
		replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
		topID := 0
		if ok {
			if replyTo.ForumTopic {
				topID = replyTo.ReplyToMsgID
			} else {
				topID = replyTo.ReplyToTopID
			}
		}
		if topID != p.threadID {
			return nil
		}
	}

	text := msg.Message
	if text == "" {
		return nil
	}

	log.Printf("[test] received message id=%d len=%d", msg.ID, len(text))
	
	// Store all messages
	p.messages = append(p.messages, text)

	// Try to parse as signal
	if sig, ok := parseSignalText(text); ok {
		p.signals = append(p.signals, *sig)
		log.Printf("[test] parsed signal #%d %s %s", sig.ID, sig.Symbol, sig.Direction)
	}

	return nil
}

// Test configuration
type testConfig struct {
	AppID           int
	AppHash         string
	Phone           string
	Trader3Channel  string
	Trader3ThreadID int
}

func loadTestConfig() *testConfig {
	appID := 0
	if v := os.Getenv("TELEGRAM_APP_ID"); v != "" {
		fmt.Sscanf(v, "%d", &appID)
	}

	threadID := 8 // default
	if v := os.Getenv("TRADER3_THREAD_ID"); v != "" {
		fmt.Sscanf(v, "%d", &threadID)
	}

	return &testConfig{
		AppID:           appID,
		AppHash:         os.Getenv("TELEGRAM_APP_HASH"),
		Phone:           os.Getenv("TELEGRAM_PHONE"),
		Trader3Channel:  os.Getenv("TRADER3_CHANNEL"),
		Trader3ThreadID: threadID,
	}
}

// Test code reader
type testCodeReader struct{}

func (testCodeReader) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter Telegram code for test: ")
	var code string
	fmt.Scanln(&code)
	return code, nil
}

// Benchmark the parser performance
func BenchmarkSignalParsing(b *testing.B) {
	// Sample messages from your screenshots
	testMessages := []string{
		`[trader#3] 🌹SIGNAL ID: #2133🌹
COIN: $FIL/USDT (2-5x)
Direction: LONG

ENTRY: 1.040 - 1.050

TARGETS: 1.100 - 1.150 - 1.225 - 1.300 - 1.400 - 1.500 - 1.625 - 1.750

STOP LOSS: 0.950

8H FVG confluent with ascending trendline support at entry.`,
		
		`[trader#3] 🌹SIGNAL ID: #2132🌹
COIN: $NIGHT/USDT (2-5x)
Direction: LONG

ENTRY: 0.0348 - 0.0350

TARGETS: 0.0365 - 0.0380 - 0.0400 - 0.0425 - 0.0450 - 0.0480 - 0.0510 - 0.0550

STOP LOSS: 0.0315

8H FVG confluent with ascending trendline at entry.`,
		
		// Invalid message (should not parse)
		`This is just a regular message without signal structure`,
		
		// GEM signal (should be filtered out)
		`GEM SIGNAL ID: #123
COIN: $TEST/USDT
Direction: LONG
ENTRY: 1.0 - 1.1
TARGETS: 1.2 - 1.3
STOP LOSS: 0.9`,
		
		// Profit/loss message (should be filtered out)
		`Signal #123 hit TP1 for 15.5% PROFIT`,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, msg := range testMessages {
			parseSignalText(msg)
		}
	}
}

// Test individual parsing functions
func TestParsingFunctions(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
		signal   *ParsedSignal
	}{
		{
			name: "Valid FIL signal",
			input: `[trader#3] 🌹SIGNAL ID: #2133🌹
COIN: $FIL/USDT (2-5x)
Direction: LONG

ENTRY: 1.040 - 1.050

TARGETS: 1.100 - 1.150 - 1.225 - 1.300 - 1.400 - 1.500 - 1.625 - 1.750

STOP LOSS: 0.950

8H FVG confluent with ascending trendline support at entry.`,
			expected: true,
			signal: &ParsedSignal{
				ID:        2133,
				Symbol:    "FILUSDT",
				Direction: "LONG",
				EntryLow:  1.040,
				EntryHigh: 1.050,
				SL:        0.950,
				TPs:       []float64{1.100, 1.150, 1.225, 1.300, 1.400, 1.500, 1.625, 1.750},
			},
		},
		{
			name: "Valid NIGHT signal",
			input: `[trader#3] 🌹SIGNAL ID: #2132🌹
COIN: $NIGHT/USDT (2-5x)
Direction: LONG

ENTRY: 0.0348 - 0.0350

TARGETS: 0.0365 - 0.0380 - 0.0400 - 0.0425 - 0.0450 - 0.0480 - 0.0510 - 0.0550

STOP LOSS: 0.0315

8H FVG confluent with ascending trendline at entry.`,
			expected: true,
			signal: &ParsedSignal{
				ID:        2132,
				Symbol:    "NIGHTUSDT",
				Direction: "LONG",
				EntryLow:  0.0348,
				EntryHigh: 0.0350,
				SL:        0.0315,
				TPs:       []float64{0.0365, 0.0380, 0.0400, 0.0425, 0.0450, 0.0480, 0.0510, 0.0550},
			},
		},
		{
			name:     "GEM signal (should be filtered)",
			input:    "GEM SIGNAL ID: #123\nCOIN: $TEST/USDT\nDirection: LONG\nENTRY: 1.0 - 1.1\nTARGETS: 1.2 - 1.3\nSTOP LOSS: 0.9",
			expected: false,
		},
		{
			name:     "Profit message (should be filtered)",
			input:    "Signal #123 hit TP1 for 15.5% PROFIT",
			expected: false,
		},
		{
			name:     "Regular text (should not parse)",
			input:    "This is just a regular message",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := parseSignalText(tt.input)
			
			if ok != tt.expected {
				t.Errorf("parseSignalText() ok = %v, expected %v", ok, tt.expected)
				return
			}
			
			if tt.expected && sig != nil && tt.signal != nil {
				// Print detailed parsing results
				t.Logf("\n🎯 PARSED SIGNAL STRUCTURE:")
				t.Logf("   Raw input length: %d characters", len(tt.input))
				t.Logf("   Signal ID: %d", sig.ID)
				t.Logf("   Symbol: %s (from %q)", sig.Symbol, extractCoin(tt.input))
				t.Logf("   Direction: %s", sig.Direction)
				t.Logf("   Entry Range: %.6g - %.6g", sig.EntryLow, sig.EntryHigh)
				t.Logf("   Stop Loss: %.6g", sig.SL)
				t.Logf("   Take Profits (%d targets): %v", len(sig.TPs), sig.TPs)
				
				// Calculate and show percentages
				mid := (sig.EntryLow + sig.EntryHigh) / 2
				if mid > 0 {
					slPct := (mid - sig.SL) / mid * 100
					t.Logf("   SL Risk: -%.2f%%", slPct)
					
					if len(sig.TPs) > 0 {
						t.Logf("   TP Rewards:")
						for i, tp := range sig.TPs {
							tpPct := (tp - mid) / mid * 100
							rr := tpPct / slPct
							t.Logf("     TP%d: %.6g (+%.2f%%, R:R %.2f:1)", i+1, tp, tpPct, rr)
						}
					}
				}
				
				// Show formatted alert
				alert := formatSignalAlert(sig)
				t.Logf("\n📱 FORMATTED TELEGRAM ALERT:\n%s", alert)
				
				// Verify expected values
				if sig.ID != tt.signal.ID {
					t.Errorf("ID = %d, expected %d", sig.ID, tt.signal.ID)
				}
				if sig.Symbol != tt.signal.Symbol {
					t.Errorf("Symbol = %s, expected %s", sig.Symbol, tt.signal.Symbol)
				}
				if sig.Direction != tt.signal.Direction {
					t.Errorf("Direction = %s, expected %s", sig.Direction, tt.signal.Direction)
				}
				if sig.EntryLow != tt.signal.EntryLow {
					t.Errorf("EntryLow = %f, expected %f", sig.EntryLow, tt.signal.EntryLow)
				}
				if sig.EntryHigh != tt.signal.EntryHigh {
					t.Errorf("EntryHigh = %f, expected %f", sig.EntryHigh, tt.signal.EntryHigh)
				}
				if sig.SL != tt.signal.SL {
					t.Errorf("SL = %f, expected %f", sig.SL, tt.signal.SL)
				}
				if len(sig.TPs) != len(tt.signal.TPs) {
					t.Errorf("TPs length = %d, expected %d", len(sig.TPs), len(tt.signal.TPs))
				}
			} else if !tt.expected {
				t.Logf("✅ Correctly filtered out: %q", truncateString(tt.input, 100))
			}
		})
	}
}

// Helper functions for detailed output
func extractCoin(text string) string {
	re := regexp.MustCompile(`(?i)COIN\s*:\s*(\S+)`)
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}